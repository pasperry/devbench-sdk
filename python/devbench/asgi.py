"""ASGI middleware for FastAPI, Starlette and any ASGI app::

    from devbench.asgi import DevbenchMiddleware   # or devbench.fastapi
    app.add_middleware(DevbenchMiddleware)

Accepts the correlation id for the duration of the request, scopes identity
to it, reports an exception that escapes the app and re-raises the same
object (Starlette's ServerErrorMiddleware, outside it, still answers 500 and
runs your handlers), and marks a traced response during which failures were
handled (``x-adt-handled``).

A plain ASGI middleware, not BaseHTTPMiddleware: the request's context (the
trace, the user) reaches the endpoint, its thread pool and its logging calls
unchanged, and responses stream as before.
"""

from __future__ import annotations

from typing import Any, Awaitable, Callable, MutableMapping, Optional

from . import _core, _integration, _requests, context
from .trace import Trace

Scope = MutableMapping[str, Any]
Message = MutableMapping[str, Any]
Receive = Callable[[], Awaitable[Message]]
Send = Callable[[Message], Awaitable[None]]

_TRACE_HEADER = b"x-adt-trace"


class DevbenchMiddleware:
    def __init__(self, app: Callable[[Scope, Receive, Send], Awaitable[None]]) -> None:
        self.app = app
        _integration.setup()

    async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
        if scope.get("type") != "http" or not _core.enabled():
            await self.app(scope, receive, send)
            return
        # Inside another Dev Bench middleware: that one scopes, reports and
        # observes this request.
        if _requests.observing():
            await self.app(scope, receive, send)
            return
        trace = _parse(scope)
        answered: list = []

        async def send_wrapper(message: Message) -> None:
            if message.get("type") == "http.response.start":
                try:
                    if not answered:
                        answered[:] = [message.get("status"), _requests.header(message.get("headers"), "content-type")]
                except Exception:
                    pass
                try:
                    message = _with_handled(message)
                except Exception:
                    pass
            await send(message)

        with context.scoped(trace) as request_scope:
            request = _requests.begin(request_scope, scope.get("method"))
            try:
                await self.app(scope, receive, send_wrapper)
            except Exception as error:
                _integration.report_request_error(error, _symbol(scope))
                raise
            if request is not None:
                # An app that returned without answering: the server sends
                # 500 (uvicorn, hypercorn), and nothing was reported.
                status = answered[0] if answered else 500
                content_type = answered[1] if answered else None
                request.route = _route(scope)
                _requests.finish(request, status, _requests.event_stream(content_type))


def _parse(scope: Scope) -> Optional[Trace]:
    try:
        for name, value in scope.get("headers") or ():
            if name.lower() == _TRACE_HEADER:
                return Trace.parse(value)
    except Exception:
        pass
    return None


def _route(scope: Scope) -> Optional[str]:
    """The matched route's pattern: ``scope["route"].path`` (FastAPI, and
    Starlette since it records the route), else the route whose endpoint the
    router put in the scope. None (reported as "*") when nothing matched."""
    try:
        route = scope.get("route")
        path = getattr(route, "path", None) if route is not None else None
        if not isinstance(path, str):
            endpoint, router = scope.get("endpoint"), scope.get("router")
            if endpoint is not None and router is not None:
                for candidate in getattr(router, "routes", None) or ():
                    if getattr(candidate, "endpoint", None) is endpoint:
                        path = getattr(candidate, "path", None)
                        break
        return path if isinstance(path, str) else None
    except Exception:
        return None


def _symbol(scope: Scope) -> Optional[str]:
    # Starlette's router puts the matched endpoint in the scope.
    endpoint = scope.get("endpoint")
    return _integration.callable_path(endpoint) if endpoint is not None else None


def _with_handled(message: Message) -> Message:
    if _integration.handled_count() <= 0:
        return message
    headers = [(k.decode("latin-1"), v.decode("latin-1")) for k, v in message.get("headers") or ()]
    updated = _integration.with_handled(headers)
    message = dict(message)
    message["headers"] = [(k.lower().encode("latin-1"), v.encode("latin-1")) for k, v in updated]
    return message
