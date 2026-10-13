"""Flask::

    from flask import Flask
    from devbench.flask import Devbench

    app = Flask(__name__)
    Devbench(app)          # or devbench_ext = Devbench(); devbench_ext.init_app(app)

Wraps ``app.wsgi_app`` in the WSGI middleware (trace, identity scope,
``x-adt-handled``) and subscribes to ``got_request_exception``, which Flask
sends for an unhandled exception before it answers 500 (an ``abort(404)`` or
any other HTTPException is a response, not a failure, and never reaches it),
and to ``request_started``, which records the matched URL rule as the
request's route (Capability 2b: failed and slow requests, 429 spikes).
"""

from __future__ import annotations

from typing import Any, Optional

from flask import got_request_exception, request, request_started

from . import _integration, _requests
from .wsgi import DevbenchMiddleware


class Devbench:
    def __init__(self, app: Any = None) -> None:
        if app is not None:
            self.init_app(app)

    def init_app(self, app: Any) -> None:
        extensions = getattr(app, "extensions", None)
        if isinstance(extensions, dict):
            if "devbench" in extensions:
                return
            extensions["devbench"] = self
        _integration.setup(service=getattr(app, "import_name", None))
        app.wsgi_app = DevbenchMiddleware(app.wsgi_app)
        got_request_exception.connect(_on_exception, app, weak=False)
        request_started.connect(_on_request, app, weak=False)


def _on_request(sender: Any, **_: Any) -> None:
    """Routing has run: the matched rule (``/orders/<int:id>``) is the
    request's route for Capability 2b. None matched (a 404) is "*"."""
    try:
        rule = request.url_rule
        if rule is not None:
            _requests.set_route(rule.rule)
    except Exception:
        pass


def _on_exception(sender: Any, exception: Optional[BaseException] = None, **_: Any) -> None:
    try:
        _integration.report_request_error(exception, _symbol(sender))
    except Exception:
        pass


def _symbol(app: Any) -> Optional[str]:
    try:
        endpoint = request.endpoint
        view = app.view_functions.get(endpoint) if endpoint else None
        return _integration.callable_path(view) if view is not None else endpoint
    except Exception:
        return None
