"""Celery: failed tasks, and the trace from the request that enqueued them::

    # where your Celery app is created (tasks.py, celery.py, ...)
    import devbench.celery
    devbench.celery.install()

Under Django this is automatic (``"devbench.django"`` installs it when
Celery is loaded). Signals only: nothing wraps your tasks.

* ``before_task_publish``: the active trace travels in the task message's
  headers, so a task's failure and its log lines join the request that
  enqueued it.
* ``task_prerun`` / ``task_postrun``: the task runs in its own scope (its
  trace; identity set inside it ends with it).
* ``task_failure``: reported as a ``job`` exception, symbol = the task name.
  A retry (``self.retry``) is not a failure; the last attempt's failure is.
* ``after_setup_logger`` / ``after_setup_task_logger``: log capture is put
  back after the worker replaces the root logger's handlers.
* ``worker_process_shutdown``: a prefork child flushes before it exits (the
  pool ends children without running atexit).

A worker process reports as "<service>-celery" unless DEVBENCH_SERVICE is
set, so an app's web and worker processes are told apart.
"""

from __future__ import annotations

import logging
import threading
from typing import Any, Dict, Optional

from celery import signals

from . import _core, _integration, context, logs, reporter
from .trace import Trace

# The task-message header the trace travels in.
HEADER = "adt_trace"
WORKER_SUFFIX = "celery"

_installed = False
_lock = threading.Lock()
_tokens: Dict[str, Any] = {}


def install() -> bool:
    """Connects the hooks. Safe to call more than once. Returns True when
    installed."""
    global _installed
    with _lock:
        if _installed:
            return True
        try:
            signals.before_task_publish.connect(_before_publish, weak=False, dispatch_uid="devbench.publish")
            signals.task_prerun.connect(_prerun, weak=False, dispatch_uid="devbench.prerun")
            signals.task_postrun.connect(_postrun, weak=False, dispatch_uid="devbench.postrun")
            signals.task_failure.connect(_failure, weak=False, dispatch_uid="devbench.failure")
            signals.worker_init.connect(_worker_init, weak=False, dispatch_uid="devbench.worker_init")
            signals.worker_process_shutdown.connect(_worker_shutdown, weak=False, dispatch_uid="devbench.shutdown")
            signals.after_setup_logger.connect(_after_logging, weak=False, dispatch_uid="devbench.logger")
            signals.after_setup_task_logger.connect(_after_logging, weak=False, dispatch_uid="devbench.task_logger")
            _installed = True
        except Exception:
            return False
    if _in_worker():
        _mark_worker()
    _integration.setup()
    return True


def _in_worker() -> bool:
    return _integration.celery_worker_command()


def _mark_worker() -> None:
    try:
        cfg = _core.config()
        if not cfg.worker:
            cfg.worker = WORKER_SUFFIX
    except Exception:
        pass


def _worker_init(**_: Any) -> None:
    _mark_worker()


def _after_logging(logger: Any = None, **_: Any) -> None:
    """A worker replaces the root logger's handlers when it sets up logging
    (worker_hijack_root_logger), and its task logger does not propagate:
    capture goes back on both."""
    try:
        if _core.capture_logs() and isinstance(logger, logging.Logger):
            logs.install(logger)
    except Exception:
        pass


def _before_publish(headers: Optional[dict] = None, **_: Any) -> None:
    try:
        trace = context.current_trace()
        if trace is not None and isinstance(headers, dict) and HEADER not in headers:
            headers[HEADER] = str(trace)
    except Exception:
        pass


def _prerun(task_id: Optional[str] = None, task: Any = None, **_: Any) -> None:
    try:
        trace = Trace.parse(_header(task))
        _scope, token = context.enter(trace)
        if task_id:
            _tokens[task_id] = token
    except Exception:
        pass


def _postrun(task_id: Optional[str] = None, **_: Any) -> None:
    try:
        token = _tokens.pop(task_id, None) if task_id else None
        if token is not None:
            context.leave(token)
    except Exception:
        pass


def _failure(sender: Any = None, exception: Optional[BaseException] = None, **_: Any) -> None:
    try:
        reporter.capture(exception, "job", False, _task_name(sender))
    except Exception:
        pass


def _worker_shutdown(**_: Any) -> None:
    _core.flush(timeout=2.0)


def _header(task: Any) -> Optional[str]:
    request = getattr(task, "request", None)
    if request is None:
        return None
    value = getattr(request, HEADER, None)
    if value is None:
        headers = getattr(request, "headers", None)
        if isinstance(headers, dict):
            value = headers.get(HEADER)
    return value if isinstance(value, str) else None


def _task_name(task: Any) -> Optional[str]:
    name = getattr(task, "name", None)
    return name if isinstance(name, str) and name else None
