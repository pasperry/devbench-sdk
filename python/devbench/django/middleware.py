"""DevbenchMiddleware for Django (sync and async). Added first in MIDDLEWARE
by the app config.

Django turns every exception inside the stack into a response before it
reaches this middleware, so failures are reported from
``got_request_exception``, which Django sends for exactly the exceptions it
answers with a 500 (an Http404, PermissionDenied, BadRequest or
SuspiciousOperation is a response, not a failure, and never reaches it). The
signal runs inside this middleware's scope, so the report carries the
request's trace and user.
"""

from __future__ import annotations

import sys
from typing import Any, Callable, Optional

from asgiref.sync import iscoroutinefunction, markcoroutinefunction

from .. import _core, _integration, _requests, context
from ..trace import WSGI_HEADER, Trace


class DevbenchMiddleware:
    sync_capable = True
    async_capable = True

    def __init__(self, get_response: Callable[..., Any]) -> None:
        self.get_response = get_response
        self._async = iscoroutinefunction(get_response)
        if self._async:
            markcoroutinefunction(self)

    def __call__(self, request: Any) -> Any:
        if self._async:
            return self.__acall__(request)
        if not _core.enabled() or _requests.observing():
            return self.get_response(request)
        with context.scoped(_parse(request)) as scope:
            observed = _requests.begin(scope, getattr(request, "method", None))
            response = self.get_response(request)
            _mark_handled(response)
            _finish(observed, request, response)
            return response

    async def __acall__(self, request: Any) -> Any:
        if not _core.enabled() or _requests.observing():
            return await self.get_response(request)
        with context.scoped(_parse(request)) as scope:
            observed = _requests.begin(scope, getattr(request, "method", None))
            response = await self.get_response(request)
            _mark_handled(response)
            _finish(observed, request, response)
            return response


def on_request_exception(sender: Any = None, request: Any = None, **_: Any) -> None:
    """got_request_exception: the exception being handled is sys.exc_info()."""
    try:
        _integration.report_request_error(sys.exc_info()[1], _symbol(request))
    except Exception:
        pass


def _parse(request: Any) -> Optional[Trace]:
    try:
        return Trace.parse(request.META.get(WSGI_HEADER))
    except Exception:
        return None


def _symbol(request: Any) -> Optional[str]:
    # "shop.views.checkout": the resolved view, when routing got that far.
    try:
        match = getattr(request, "resolver_match", None)
        if match is None:
            return None
        return getattr(match, "_func_path", None) or _integration.callable_path(match.func)
    except Exception:
        return None


def _finish(observed: Any, request: Any, response: Any) -> None:
    """Capability 2b: the route is the resolved URL pattern
    (``resolver_match.route``, joined across includes); none matched is
    "*"."""
    if observed is None:
        return
    try:
        match = getattr(request, "resolver_match", None)
        observed.route = getattr(match, "route", None) if match is not None else None
        status = getattr(response, "status_code", 0)
        content_type = response.get("Content-Type") if response is not None else None
        _requests.finish(observed, status, _requests.event_stream(content_type))
    except Exception:
        pass


def _mark_handled(response: Any) -> None:
    try:
        count = _integration.handled_count()
        if count <= 0 or response is None:
            return
        response[_integration.HANDLED_HEADER] = str(count)
        response[_integration.EXPOSE_HEADER] = _integration.exposed(response.get(_integration.EXPOSE_HEADER))
    except Exception:
        pass
