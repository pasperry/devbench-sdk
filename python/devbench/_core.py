"""Configuration and transport selection.

One transport per process, decided at the first report:

* ``DEVBENCH_ENABLED=false``: none.
* ``DEVBENCH_DSN`` (or ``ADT_DSN``): direct to Dev Bench. A DSN that does not
  parse logs one warning and reports nothing.
* neither: none. (The Ruby and Go SDKs fall back to a local sidecar socket;
  this SDK is direct-only, so an environment without a DSN, such as
  development or CI, sends nothing and says nothing.)
"""

from __future__ import annotations

import atexit
import logging
import sys
import threading
from typing import IO, Any, Dict, Optional

from . import logs
from .config import DSN, Configuration, InvalidDSN
from .direct import EXIT_TIMEOUT, DirectTransport

_LOGGER = logging.getLogger("devbench")
_lock = threading.RLock()
_config: Optional[Configuration] = None
_transport: Any = None
_warned: Dict[str, bool] = {}
_exit_flush_installed = False


class _NullTransport:
    """Reporting is off: everything is accepted and dropped."""

    def deliver(self, payload: Any) -> None:
        pass

    def flush(self, timeout: float = 0) -> bool:
        return True

    def start(self) -> None:
        pass

    def stop(self) -> None:
        pass


NULL = _NullTransport()


def config() -> Configuration:
    """The configuration, read from the environment on first use."""
    global _config
    current = _config
    if current is not None:
        return current
    with _lock:
        if _config is None:
            _config = Configuration()
        return _config


def transport() -> Any:
    """The active transport. Built once, on first use, so a DSN configured
    from code before the first report is seen."""
    global _transport
    current = _transport
    if current is not None:
        return current
    with _lock:
        if _transport is None:
            _transport = _build_transport()
        return _transport


def enabled() -> bool:
    try:
        return config().enabled
    except Exception:
        return False


def direct() -> bool:
    """True when this process reports to Dev Bench (a DSN is set and
    parses)."""
    try:
        return isinstance(transport(), DirectTransport)
    except Exception:
        return False


def configure(dsn: Optional[str] = None, service: Optional[str] = None, release: Optional[str] = None,
              enabled: Optional[bool] = None, flush_interval: Optional[float] = None) -> None:
    """Configures from code instead of (or on top of) the environment. Only
    the fields given change. Takes effect from the next report. Never raises
    into the caller."""
    try:
        cfg = config()
        if dsn is not None:
            cfg.dsn = dsn
        if service is not None:
            cfg.service = service
        if release is not None:
            cfg.release = release
        if enabled is not None:
            cfg.enabled = bool(enabled)
        if flush_interval is not None:
            cfg.flush_interval = float(flush_interval)
        _reset_transport()
        # Capture already in place follows the new configuration.
        if logs.active() or _capture_requested:
            _apply_capture()
    except Exception as e:
        warn_once("configure", "devbench.configure raised %s; reporting is unchanged" % type(e).__name__)


_capture_requested = False


def capture_logs() -> bool:
    """Keeps recent log lines, per trace, for triage to ask for (direct mode
    only; elsewhere a no-op): a handler on the root logger. Every
    integration calls this; a plain WSGI/ASGI app may call it itself.
    Returns True when capture is on. Never raises."""
    global _capture_requested
    try:
        _capture_requested = True
        return _apply_capture()
    except Exception:
        return False


def _apply_capture() -> bool:
    if enabled() and direct():
        logs.install()
        logs.activate()
        return True
    logs.deactivate()
    return False


def flush(timeout: float = 5.0) -> bool:
    """Sends whatever has been counted, now, waiting at most ``timeout``
    seconds. Returns True when nothing is left unsent. Never raises."""
    try:
        return bool(transport().flush(timeout=timeout))
    except Exception:
        return False


def self_test(out: Optional[IO[str]] = None) -> bool:
    """Checks the DSN against ingest and prints the result
    (``python -m devbench test``). Returns True when ingest accepted the key.
    Never raises."""
    out = out if out is not None else sys.stdout
    try:
        cfg = config()
        if not cfg.enabled:
            out.write("Dev Bench is disabled (DEVBENCH_ENABLED=false); nothing was sent.\n")
            return False
        if not (cfg.dsn or "").strip():
            out.write("DEVBENCH_DSN is not set. Set it to the DSN Dev Bench gave you for this environment "
                      "(https://<public>:<secret>@<host>, from `adt dsn create`), or call devbench.configure(dsn=...).\n")
            return False
        try:
            dsn = DSN.parse(cfg.dsn)
        except InvalidDSN as e:
            out.write("DEVBENCH_DSN %s.\n" % e)
            return False
        client = DirectTransport(dsn, cfg.resolved_service(), cfg.resolved_release())
        return client.self_test(out)
    except Exception as e:
        out.write("Dev Bench test failed: %s: %s\n" % (type(e).__name__, e))
        return False


def reset() -> None:
    """Drops the active transport (stopping a direct client's thread) and the
    configuration, so both are rebuilt from scratch. For tests."""
    global _config, _capture_requested
    _reset_transport()
    logs.deactivate()
    with _lock:
        _config = None
        _capture_requested = False
        _warned.clear()


def warn_once(kind: str, message: str) -> None:
    """Logs a problem with Dev Bench itself, once per kind per process, on
    the ``devbench`` logger. Never raises."""
    try:
        with _lock:
            if _warned.get(kind):
                return
            _warned[kind] = True
        _LOGGER.warning("[devbench] %s", message)
    except Exception:
        pass


def rebuild_transport() -> None:
    """Drops a transport built before the integrations said what the app is
    (its counts are flushed first), so the next use builds it with the right
    service and root. Nothing to do when none was built."""
    if _transport is not None:
        _reset_transport()


def _reset_transport() -> None:
    global _transport
    with _lock:
        old, _transport = _transport, None
    stop = getattr(old, "stop", None)
    if stop is not None:
        stop()


def _build_transport() -> Any:
    try:
        cfg = config()
        if not cfg.enabled or not (cfg.dsn or "").strip():
            return NULL
        try:
            dsn = DSN.parse(cfg.dsn)
        except InvalidDSN as e:
            warn_once("dsn", "DEVBENCH_DSN %s; nothing will be reported" % e)
            return NULL
        _install_exit_flush()
        return DirectTransport(dsn, cfg.resolved_service(), cfg.resolved_release(),
                               interval=cfg.flush_interval, warn=warn_once)
    except Exception:
        return NULL


def _install_exit_flush() -> None:
    """One last flush when the process exits, bounded to 2 s (spec)."""
    global _exit_flush_installed
    if _exit_flush_installed:
        return
    _exit_flush_installed = True
    atexit.register(_exit_flush)


def _exit_flush() -> None:
    current = _transport
    if isinstance(current, DirectTransport):
        try:
            # The open 429 spike window is evaluated as it stands at exit.
            current.end_requests()
            current.flush(timeout=EXIT_TIMEOUT)
        except Exception:
            pass
