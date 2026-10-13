"""Turns a failure into a report (the ``exception`` / ``handled_failure``
message of docs/SERVER_SDK_SPEC.md, as the Ruby and Go SDKs build it) and
hands it to the active transport.

Every failure here is swallowed. This is the one place where that is correct:
everywhere else it is the exact behaviour the product exists to find.
"""

from __future__ import annotations

import collections
import os
import threading
from types import TracebackType
from typing import Any, List, Optional

from . import context
from .fingerprint import utf8
from .version import PROTOCOL_VERSION

# docs/server-sdk/control-socket.md, "Control socket — v1".
MAX_MESSAGE = 2000
MAX_HANDLED_MESSAGE = 500
MAX_FRAMES = 50
# Bounds one frame so 50 of them stay small.
MAX_FIELD = 1024

CONTEXTS = ("request", "job", "explicit")

# Status codes and framework control flow, not failures. Matched by name
# against the exception's class and its bases (module.QualName), so none of
# these packages needs to be importable. Most never reach a hook (Django,
# Flask and Starlette answer them before any error signal), but an app may
# pass one to capture_exception. Extend with devbench.ignore_exceptions.
DEFAULT_IGNORED = (
    "django.http.response.Http404",
    "django.core.exceptions.PermissionDenied",
    "django.core.exceptions.SuspiciousOperation",
    "django.core.exceptions.BadRequest",
    "werkzeug.exceptions.HTTPException",
    "starlette.exceptions.HTTPException",
    "celery.exceptions.Retry",
    "celery.exceptions.Ignore",
    "celery.exceptions.Reject",
)

_MARK = "__devbench_reported__"
# Exceptions that refuse an attribute: remembered by id, bounded.
_UNMARKABLE_MAX = 1024

_ignored: List[str] = list(DEFAULT_IGNORED)
_unmarkable: "collections.OrderedDict[int, bool]" = collections.OrderedDict()
_unmarkable_lock = threading.Lock()


def ignore_exceptions() -> List[str]:
    """The class names never reported (a list the app may extend)::

        devbench.ignore_exceptions().append("myapp.errors.NotAuthorized")
    """
    return _ignored


def class_name(cls: type) -> str:
    """``module.QualName``; builtins by their plain name (``ValueError``)."""
    module = getattr(cls, "__module__", None) or ""
    qual = getattr(cls, "__qualname__", None) or getattr(cls, "__name__", "Exception")
    return qual if module in ("builtins", "exceptions", "") else "%s.%s" % (module, qual)


def ignored(error: BaseException) -> bool:
    # Process control, not application failures: SystemExit,
    # KeyboardInterrupt, GeneratorExit, asyncio.CancelledError.
    if not isinstance(error, Exception):
        return True
    try:
        names = {class_name(c) for c in type(error).__mro__}
        return any(name in names for name in _ignored)
    except Exception:
        return False


def reported(error: BaseException) -> bool:
    """Whether any hook already sent this exception object."""
    try:
        if getattr(error, _MARK, False) is True:
            return True
    except Exception:
        pass
    with _unmarkable_lock:
        return id(error) in _unmarkable


def _mark(error: BaseException) -> None:
    try:
        setattr(error, _MARK, True)
        if getattr(error, _MARK, False) is True:
            return
    except Exception:
        pass
    with _unmarkable_lock:
        _unmarkable[id(error)] = True
        while len(_unmarkable) > _UNMARKABLE_MAX:
            _unmarkable.popitem(last=False)


def capture(error: Any, context_name: str, handled: bool, symbol: Optional[str] = None) -> None:
    """Sends an ``exception`` report, once per exception object. Never
    raises: this runs inside the app's own error paths, and anything raised
    here would replace the application's exception."""
    try:
        if not isinstance(error, BaseException):
            return
        if reported(error) or ignored(error):
            return
        # Marked before sending, so a second hook reached while this one is
        # still sending does not send it again.
        _mark(error)
        _mark_request()
        _emit(exception_payload(error, context_name, handled, symbol))
    except Exception:
        pass


def _mark_request() -> None:
    """An exception reported during a request is that request's report: a
    5xx it ends with is not also ``failed`` (Capability 2b). A handled
    failure does not mark it."""
    try:
        scope = context.current()
        request = scope.request if scope is not None else None
        if request is not None:
            request.exception_reported = True
    except Exception:
        pass


def request_payload(signal: str, method: str, route: str, status: int, duration_ms: int) -> dict:
    """The ``request`` message (docs/server-sdk/control-socket.md) for a
    ``failed`` or ``slow`` request, with the request's trace and user."""
    payload = {
        "v": PROTOCOL_VERSION,
        "kind": "request",
        "signal": signal,
        "method": method,
        "route": route,
        "status": int(status),
        "duration_ms": int(duration_ms),
        "trace": _safely(lambda: _trace_text()),
        "user": _safely(context.current_user),
    }
    return {k: v for k, v in payload.items() if v is not None}


def report_request(signal: str, method: str, route: str, status: int, duration_ms: int) -> None:
    try:
        _emit(request_payload(signal, method, route, status, duration_ms))
    except Exception:
        pass


def report_handled(error: Any, symbol: str, reason: Optional[str] = None) -> None:
    """Records that a failure was handled here. Does not change behaviour."""
    # Counted first, and regardless of whether anything after it works: the
    # count is what the browser is told.
    try:
        context.ensure().handled += 1
    except Exception:
        pass
    try:
        _emit(handled_payload(error, symbol, reason))
    except Exception:
        pass


def exception_payload(error: BaseException, context_name: str, handled: bool, symbol: Optional[str]) -> dict:
    payload = {
        "v": PROTOCOL_VERSION,
        "kind": "exception",
        "context": context_name if context_name in CONTEXTS else "explicit",
        "handled": bool(handled),
        "error": _safely(lambda: class_name(type(error))) or "Exception",
        "message": _safely(lambda: truncate(_message(error), MAX_MESSAGE)),
        "symbol": _safely(lambda: utf8(symbol) if symbol is not None else "") or "",
        "frames": _safely(lambda: frames(error.__traceback__, _app_root())) or [],
        "trace": _safely(lambda: _trace_text()),
        "user": _safely(context.current_user),
    }
    return {k: v for k, v in payload.items() if v is not None}


def handled_payload(error: Any, symbol: Any, reason: Any) -> dict:
    # Each field is read on its own: an exception whose __str__ raises still
    # reports its class and site, which are the fingerprint.
    payload = {
        "v": PROTOCOL_VERSION,
        "kind": "handled_failure",
        "symbol": _safely(lambda: utf8(symbol)) if symbol is not None else None,
        "error": _safely(lambda: class_name(type(error))),
        "message": _safely(lambda: truncate(_message(error), MAX_HANDLED_MESSAGE)),
        "reason": _safely(lambda: utf8(reason)) if reason is not None else None,
        "trace": _safely(lambda: _trace_text()),
        "user": _safely(context.current_user),
    }
    return {k: v for k, v in payload.items() if v is not None}


def frames(tb: Optional[TracebackType], root: Optional[str]) -> list:
    """A traceback as ``[{function, file, line}]``, innermost first, at most
    MAX_FRAMES, with files under ``root`` made relative to it."""
    window: "collections.deque" = collections.deque(maxlen=MAX_FRAMES)
    while tb is not None:
        window.append(tb)
        tb = tb.tb_next
    prefix = _root_prefix(root)
    out = []
    for item in reversed(window):
        code = item.tb_frame.f_code
        function = getattr(code, "co_qualname", None) or code.co_name
        file = utf8(code.co_filename)
        if prefix and file.startswith(prefix):
            file = file[len(prefix):]
        out.append({"function": _clip(utf8(function)), "file": _clip(file), "line": int(item.tb_lineno or 0)})
    return out


def truncate(text: Any, limit: int) -> str:
    """At most ``limit`` characters, including the ellipsis."""
    text = utf8(text)
    return text[: limit - 1] + "…" if len(text) > limit else text


def _message(error: Any) -> str:
    return str(error)


def _trace_text() -> Optional[str]:
    trace = context.current_trace()
    return str(trace) if trace is not None else None


def _app_root() -> Optional[str]:
    from . import _core

    return _core.config().resolved_root()


def _root_prefix(root: Optional[str]) -> Optional[str]:
    if not root:
        return None
    return root if root.endswith(os.sep) else root + os.sep


def _clip(text: str) -> str:
    return text[:MAX_FIELD] if len(text) > MAX_FIELD else text


def _safely(fn: Any) -> Any:
    try:
        return fn()
    except Exception:
        return None


def _emit(payload: dict) -> None:
    from . import _core

    try:
        _core.transport().deliver(payload)
    except Exception:
        pass
