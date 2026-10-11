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

from . import _core, _integration, context
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
        trace = _parse(environ)

        def start(status: str, headers: list, exc_info: Any = None) -> Any:
            try:
                headers = _integration.with_handled(list(headers))
            except Exception:
                pass
            if exc_info is not None:
                return start_response(status, headers, exc_info)
            return start_response(status, headers)

        # Scoped even without a trace: identity set during this request must
        # end with it, and a thread's next request must not inherit it.
        with context.scoped(trace):
            try:
                return self.app(environ, start)
            except Exception as error:
                # Reported inside the scope, so the report carries this
                # request's trace and user. capture never raises.
                _integration.report_request_error(error, self._symbol_for(environ))
                raise

    def _symbol_for(self, environ: dict) -> Optional[str]:
        try:
            return self._symbol(environ) if self._symbol is not None else None
        except Exception:
            return None


def _parse(environ: dict) -> Optional[Trace]:
    try:
        return Trace.parse(environ.get(WSGI_HEADER))
    except Exception:
        return None
