"""What every framework integration shares."""

from __future__ import annotations

import os
import sys
from typing import Any, List, Optional, Tuple

from . import _core, context, reporter
from .trace import HEADER as TRACE_HEADER

HANDLED_HEADER = "x-adt-handled"
EXPOSE_HEADER = "Access-Control-Expose-Headers"

# Names a framework gives an app that say nothing about which app it is.
_GENERIC = frozenset(["", "__main__", "app", "main", "wsgi", "asgi", "application", "server", "api"])


def setup(service: Optional[str] = None, root: Optional[str] = None) -> None:
    """Records what the framework knows about the app (its name, its root,
    whether this is a Celery worker), unless something more specific already
    did, then starts log capture. A transport built before the hints arrived
    is rebuilt, so the service name never depends on import order. Never
    raises."""
    try:
        cfg = _core.config()
        changed = False
        name = (service or "").split(".")[0].strip()
        if name not in _GENERIC and not cfg.framework_service:
            cfg.framework_service = name
            changed = True
        if root and not cfg.app_root:
            cfg.app_root = str(root)
            changed = True
        if not cfg.worker and celery_worker_command():
            cfg.worker = "celery"
            changed = True
        if changed:
            _core.rebuild_transport()
        _core.capture_logs()
    except Exception:
        pass


def celery_worker_command(argv: Optional[List[str]] = None) -> bool:
    """`celery -A proj worker ...`: the command line is all there is before
    Celery's worker_init fires, and the service must be known before the
    first report."""
    argv = sys.argv if argv is None else argv
    if not argv:
        return False
    words = [a for a in argv[1:] if not a.startswith("-")]
    return "worker" in words and "celery" in os.path.basename(argv[0] or "").lower()


def callable_path(fn: Any) -> Optional[str]:
    """``module.qualname`` of a view, endpoint or task function: where a
    failure surfaced (the report's ``symbol``)."""
    try:
        target = getattr(fn, "view_class", None) or getattr(fn, "cls", None) or fn
        module = getattr(target, "__module__", None)
        qual = getattr(target, "__qualname__", None) or getattr(target, "__name__", None)
        if not qual:
            return None
        return "%s.%s" % (module, qual) if module else str(qual)
    except Exception:
        return None


def report_request_error(error: Any, symbol: Optional[str]) -> None:
    reporter.capture(error, "request", False, symbol)


def handled_count() -> int:
    """Failures handled during this traced request (0 when it carries no
    trace: the browser that reads the header always sends one)."""
    scope = context.current()
    if scope is None or scope.trace is None:
        return 0
    return scope.handled


def exposed(current: Optional[str]) -> str:
    """Access-Control-Expose-Headers with x-adt-handled added. A browser
    cannot read a custom response header cross-origin unless it is listed
    there (an SPA on another origin than its API would see nothing)."""
    current = current or ""
    if HANDLED_HEADER in current.lower():
        return current
    return "%s, %s" % (current, HANDLED_HEADER) if current else HANDLED_HEADER


def with_handled(headers: List[Tuple[str, str]]) -> List[Tuple[str, str]]:
    """A WSGI/ASGI header list with x-adt-handled set when failures were
    handled during this traced request; the list unchanged otherwise. The
    value is a count and nothing else: any script on the page can read it."""
    count = handled_count()
    if count <= 0:
        return headers
    out = [(k, v) for k, v in headers if k.lower() not in (HANDLED_HEADER, EXPOSE_HEADER.lower())]
    current = next((v for k, v in headers if k.lower() == EXPOSE_HEADER.lower()), None)
    out.append((HANDLED_HEADER, str(count)))
    out.append((EXPOSE_HEADER, exposed(current)))
    return out


__all__ = ["TRACE_HEADER", "HANDLED_HEADER", "EXPOSE_HEADER", "setup", "callable_path", "report_request_error", "handled_count", "exposed", "with_handled"]
