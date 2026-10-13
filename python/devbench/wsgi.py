"""WSGI middleware: accepts the correlation id for the duration of the
request, scopes identity to it, reports an exception that escapes the app
(then re-raises the same object), and marks a traced response during which
failures were handled (``x-adt-handled``).

Flask's integration (``devbench.flask.Devbench``) installs it; any other WSGI
app can wrap itself::

    application = DevbenchMiddleware(application)

Never rejects a request, never alters the response body, never raises
anything of its own.
"""

from __future__ import annotations

from typing import Any, Callable, Iterable, Optional

from . import _core, _integration, _requests, context
from .trace import WSGI_HEADER, Trace


class DevbenchMiddleware:
    def __init__(self, app: Callable[..., Iterable[bytes]], symbol: Optional[Callable[[dict], Optional[str]]] = None) -> None:
        self.app = app
        # Where a failure surfaced, read from the environ after the app ran.
        self._symbol = symbol
        _integration.setup()

    def __call__(self, environ: dict, start_response: Callable[..., Any]) -> Iterable[bytes]:
        # DEVBENCH_ENABLED=false: not even the scope or the header.
        if not _core.enabled():
            return self.app(environ, start_response)
        # Inside another Dev Bench middleware (Flask's, wrapped again by
        # hand): that one scopes, reports and observes this request.
        if _requests.observing():
            return self.app(environ, start_response)
        trace = _parse(environ)
        answered: list = []

        def start(status: str, headers: list, exc_info: Any = None) -> Any:
            try:
                answered[:] = [status, _requests.header(headers, "content-type")]
            except Exception:
                pass
            try:
                headers = _integration.with_handled(list(headers))
            except Exception:
                pass
            if exc_info is not None:
                return start_response(status, headers, exc_info)
            return start_response(status, headers)

        # Scoped even without a trace: identity set during this request must
        # end with it, and a thread's next request must not inherit it.
        with context.scoped(trace) as scope:
            request = _requests.begin(scope, environ.get("REQUEST_METHOD"))
            try:
                body = self.app(environ, start)
            except Exception as error:
                # Reported inside the scope, so the report carries this
                # request's trace and user. capture never raises.
                _integration.report_request_error(error, self._symbol_for(environ))
                raise
            if request is not None:
                if answered:
                    _requests.finish(request, answered[0], _requests.event_stream(answered[1]))
                else:
                    # A generator app calls start_response on its first
                    # item: decide once the server has read it.
                    return _Deferred(body, request, answered, scope)
            return body

    def _symbol_for(self, environ: dict) -> Optional[str]:
        try:
            return self._symbol(environ) if self._symbol is not None else None
        except Exception:
            return None




class _Deferred:
    """The body of an app that had not called start_response when it
    returned (a generator). Its duration is fixed when the callable
    returned; its status is known once the server has iterated. Evaluated at
    close(), in the request's scope, unless iterating raised."""

    def __init__(self, body: Iterable[bytes], request: Any, answered: list, scope: context.Scope) -> None:
        self._body = body
        self._iter = iter(body)
        self._request = request
        self._answered = answered
        self._scope = scope
        self._duration_ms = _requests.elapsed_ms(request)
        self._raised = False

    def __iter__(self) -> "_Deferred":
        return self

    def __next__(self) -> bytes:
        try:
            return next(self._iter)
        except StopIteration:
            raise
        except Exception:
            self._raised = True
            raise

    def close(self) -> None:
        try:
            close = getattr(self._body, "close", None)
            if close is not None:
                close()
        finally:
            if not self._raised and self._answered:
                with context.within(self._scope):
                    _requests.finish(self._request, self._answered[0], _requests.event_stream(self._answered[1]),
                                     duration_ms=self._duration_ms)


def _parse(environ: dict) -> Optional[Trace]:
    try:
        return Trace.parse(environ.get(WSGI_HEADER))
    except Exception:
        return None
