"""How the request middlewares observe Capability 2b (reqsignal has the
rules).

Once per request, after the application has handed back its response
without raising: classify it (failed / slow / nothing), report the signal
like any other report, and feed the process's 429 spike detector. On the
request's thread, none of it blocking: a classification, a count under a
lock, one mutex-guarded table update.

A request that raised out of the middleware is not evaluated: the exception
is its report. Only the outermost Dev Bench middleware observes (a nested one
passes straight through), so a request is never counted twice.
"""

from __future__ import annotations

import threading
from typing import Any, Iterable, Optional, Tuple

from . import _core, context, reporter, reqsignal


# Requests evaluated in this process. A test seam: an ASGI request is
# evaluated after its response has reached the client (when the app's
# coroutine completes), so a test waits for this to move. Changes nothing.
_finished = 0
_finished_lock = threading.Lock()


def finished() -> int:
    return _finished


class Request:
    """One request being observed, held on the request's Scope."""

    __slots__ = ("method", "route", "started", "exception_reported")

    def __init__(self, method: Any) -> None:
        self.method = method
        # The framework's matched route pattern, set when routing ran; None
        # reports as "*".
        self.route: Optional[str] = None
        self.started = reqsignal.monotonic_ns()
        # An `exception` was reported during this request (any context).
        self.exception_reported = False


def observing() -> bool:
    """True inside a request an outer Dev Bench middleware already
    observes."""
    scope = context.current()
    return scope is not None and scope.request is not None


def begin(scope: context.Scope, method: Any) -> Optional[Request]:
    """Starts observing the request ``scope`` belongs to. None when this
    process reports nothing (no DSN, or a DSN that does not parse): then
    nothing is timed or counted. Never raises."""
    try:
        if not _core.direct():
            return None
        request = Request(method)
        scope.request = request
        return request
    except Exception:
        return None


def set_route(route: Any) -> None:
    """Records the matched route pattern on the request being observed."""
    try:
        scope = context.current()
        request = scope.request if scope is not None else None
        if request is not None and route is not None:
            request.route = str(route)
    except Exception:
        pass


def elapsed_ms(request: Request) -> int:
    """Whole milliseconds since the middleware's entry (truncated, so 3000.9
    ms is 3000: not slow)."""
    return (reqsignal.monotonic_ns() - request.started) // 1000000


def finish(request: Optional[Request], status: Any, streaming: bool, duration_ms: Optional[int] = None) -> None:
    """The application handed back its response: classify, report, feed the
    spike detector. ``duration_ms`` defaults to the time since the
    middleware's entry. Never raises."""
    if request is None:
        return
    try:
        if duration_ms is None:
            duration_ms = elapsed_ms(request)
        code = status_code(status)
        method = reqsignal.normalize_method(request.method)
        route = reqsignal.normalize_route(request.route)
        signal = reqsignal.classify(code, duration_ms, request.exception_reported, streaming)
        if signal:
            reporter.report_request(signal, method, route, code, duration_ms)
        observe = getattr(_core.transport(), "observe_request", None)
        if observe is not None:
            observe(reqsignal.now_ms(), method, route, code)
    except Exception:
        pass
    finally:
        global _finished
        with _finished_lock:
            _finished += 1


def status_code(status: Any) -> int:
    """An int from 503, "503 Service Unavailable" (WSGI) or b"503"; 0 when
    there is none."""
    try:
        if isinstance(status, int):
            return status
        if isinstance(status, bytes):
            status = status.decode("latin-1")
        head = str(status).strip().split(" ", 1)[0]
        return int(head) if head.isdigit() and len(head) == 3 else 0
    except Exception:
        return 0


def event_stream(content_type: Any) -> bool:
    """A ``text/event-stream`` response (any case, parameters allowed): a
    connection's lifetime, never slow."""
    try:
        if isinstance(content_type, bytes):
            content_type = content_type.decode("latin-1")
        return str(content_type or "").strip().lower().startswith("text/event-stream")
    except Exception:
        return False


def header(headers: Optional[Iterable[Tuple[Any, Any]]], name: str) -> Any:
    """The first value of a header in a WSGI/ASGI header list."""
    want = name.lower()
    for key, value in headers or ():
        k = key.decode("latin-1") if isinstance(key, bytes) else str(key)
        if k.lower() == want:
            return value
    return None
