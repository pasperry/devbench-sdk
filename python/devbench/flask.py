"""Flask::

    from flask import Flask
    from devbench.flask import Devbench

    app = Flask(__name__)
    Devbench(app)          # or devbench_ext = Devbench(); devbench_ext.init_app(app)

Wraps ``app.wsgi_app`` in the WSGI middleware (trace, identity scope,
``x-adt-handled``) and subscribes to ``got_request_exception``, which Flask
sends for an unhandled exception before it answers 500 (an ``abort(404)`` or
any other HTTPException is a response, not a failure, and never reaches it).
"""

from __future__ import annotations

from typing import Any, Optional

from flask import got_request_exception, request

from . import _integration
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
