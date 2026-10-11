"""Dev Bench server SDK for Python.

    pip install devbench
    DEVBENCH_DSN=https://<public>:<secret>@<ingest host>   (from `adt dsn create`)

then one line where your framework wants it (README): Django
(``"devbench.django"`` in INSTALLED_APPS), FastAPI/Starlette
(``app.add_middleware(DevbenchMiddleware)``), Flask (``Devbench(app)``),
Celery (``devbench.celery.install()``; automatic under Django).

What it does:

* accepts and forwards the correlation id     (the middlewares, trace_headers)
* reports unhandled exceptions                (the middlewares, Celery hooks,
                                               capture_exception)
* reports handled failures                    (report_handled)
* records who was affected                    (set_user)
* keeps recent log lines per trace            (capture_logs; a logging handler)

Nothing here raises into the application.
"""

from __future__ import annotations

from typing import Any, Dict, Optional

from . import _core, context, reporter
from .config import DSN, Configuration, InvalidDSN
from .identity import set_user
from .trace import HEADER as TRACE_HEADER
from .trace import Trace
from .version import PROTOCOL_VERSION, VERSION

__version__ = VERSION

__all__ = [
    "PROTOCOL_VERSION", "VERSION", "DSN", "Configuration", "InvalidDSN", "Trace", "TRACE_HEADER",
    "configure", "enabled", "set_user", "capture_exception", "report_handled", "ignore_exceptions",
    "trace_headers", "current_trace", "capture_logs", "flush", "test",
]


def configure(dsn: Optional[str] = None, service: Optional[str] = None, release: Optional[str] = None,
              enabled: Optional[bool] = None, flush_interval: Optional[float] = None) -> None:
    """Sets any of the environment's values from code (only those given)::

        devbench.configure(dsn=secrets["devbench_dsn"], service="billing-api")

    Takes effect from the next report. Never raises."""
    _core.configure(dsn=dsn, service=service, release=release, enabled=enabled, flush_interval=flush_interval)


def enabled() -> bool:
    """False when DEVBENCH_ENABLED=false (or configure(enabled=False))."""
    return _core.enabled()


def capture_exception(error: Optional[BaseException] = None, symbol: Optional[str] = None) -> None:
    """Reports an exception the application caught and wants seen, as
    Sentry's capture_exception does (the one being handled when ``error`` is
    omitted). Returns None and never raises::

        except requests.RequestException as e:
            devbench.capture_exception(e, symbol="billing.sync")
    """
    if error is None:
        import sys

        error = sys.exc_info()[1]
    reporter.capture(error, "explicit", True, symbol)


def report_handled(error: Any, symbol: str, reason: Optional[str] = None) -> None:
    """Records that a failure was handled (caught and not re-raised) here.
    Does not change behaviour. Repeats at the same site are counted::

        except IntegrityError as e:
            devbench.report_handled(e, symbol="customers.views.update", reason="uniqueness")
    """
    reporter.report_handled(error, symbol, reason)


def ignore_exceptions() -> list:
    """Class names (``module.QualName``) never reported; append your own."""
    return reporter.ignore_exceptions()


def current_trace() -> Optional[Trace]:
    """The x-adt-trace of the request or job being handled, or None."""
    return context.current_trace()


def trace_headers() -> Dict[str, str]:
    """Headers to add to an outbound HTTP call to another instrumented
    service: the active trace with its hop incremented, or {}::

        requests.post(url, json=body, headers={**headers, **devbench.trace_headers()})
    """
    trace = context.current_trace()
    if trace is None:
        return {}
    forwarded = trace.next_hop()
    return {TRACE_HEADER: str(forwarded)} if forwarded is not None else {}


def capture_logs() -> bool:
    """Keeps recent log lines per trace for triage (direct mode). The
    integrations call this; a plain WSGI/ASGI app may call it itself."""
    return _core.capture_logs()


def flush(timeout: float = 5.0) -> bool:
    """Sends whatever has been counted now, waiting at most ``timeout``
    seconds. Returns True when nothing is left unsent."""
    return _core.flush(timeout)


def test(out: Any = None) -> bool:
    """Checks DEVBENCH_DSN against ingest and prints the result; the same as
    ``python -m devbench test``."""
    return _core.self_test(out)
