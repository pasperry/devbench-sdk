"""Per-request (and per-job) state: the active trace, who the request is for,
and how many failures were handled during it.

One mutable ``Scope`` object per request, held in a ContextVar. Mutable on
purpose: frameworks run parts of a request in a *copy* of the context (a
FastAPI/Starlette sync endpoint runs in a thread pool; asgiref's
sync_to_async copies too), and a ``ContextVar.set`` made in a copy never
reaches the middleware that reports the request's failure. Setting a field on
the shared Scope does.

Scopes nest and restore rather than clear: servers reuse threads, and
clearing would leak one request's absence into the next one's presence;
inheriting would attribute one user's failure to whoever this thread served
before.
"""

from __future__ import annotations

import contextlib
import contextvars
from typing import Any, Iterator, Optional

from .trace import Trace


class Scope:
    __slots__ = ("trace", "user", "handled", "request", "__weakref__")

    def __init__(self, trace: Optional[Trace] = None) -> None:
        self.trace = trace
        # A frozen dict {"email": ..., "account": ...} (empty fields dropped)
        # or None. Normalised before it gets here (identity.normalize).
        self.user: Optional[dict] = None
        # Failures handled during this request: the middleware turns a
        # non-zero count into the x-adt-handled response header.
        self.handled = 0
        # The request middleware's observation (devbench._requests.Request)
        # while a request is being observed; None in a job or a script.
        self.request: Optional[Any] = None


_SCOPE: "contextvars.ContextVar[Optional[Scope]]" = contextvars.ContextVar("devbench_scope", default=None)


def current() -> Optional[Scope]:
    return _SCOPE.get()


def current_trace() -> Optional[Trace]:
    scope = _SCOPE.get()
    return scope.trace if scope is not None else None


def current_user() -> Optional[dict]:
    scope = _SCOPE.get()
    return scope.user if scope is not None else None


def ensure() -> Scope:
    """The current scope, creating one (outside any request: a script, a
    shell) when there is none."""
    scope = _SCOPE.get()
    if scope is None:
        scope = Scope()
        _SCOPE.set(scope)
    return scope


@contextlib.contextmanager
def scoped(trace: Optional[Trace]) -> Iterator[Scope]:
    """Runs the block with a fresh scope for ``trace`` (which may be None),
    restoring whatever was there before."""
    scope = Scope(trace)
    token = _SCOPE.set(scope)
    try:
        yield scope
    finally:
        try:
            _SCOPE.reset(token)
        except ValueError:
            # Reset from another context (a generator finalised elsewhere):
            # fall back to clearing, never raising into the app.
            _SCOPE.set(None)


@contextlib.contextmanager
def within(scope: Scope) -> Iterator[Scope]:
    """Runs the block in an existing scope (a WSGI body closed after the
    middleware returned), restoring whatever was there before."""
    token = _SCOPE.set(scope)
    try:
        yield scope
    finally:
        try:
            _SCOPE.reset(token)
        except ValueError:
            _SCOPE.set(None)


def enter(trace: Optional[Trace]) -> "tuple[Scope, contextvars.Token]":
    """For hooks whose start and end are separate calls (Celery's prerun and
    postrun): returns the new scope and the token ``leave`` needs."""
    scope = Scope(trace)
    return scope, _SCOPE.set(scope)


def leave(token: "contextvars.Token") -> None:
    try:
        _SCOPE.reset(token)
    except (ValueError, RuntimeError):
        _SCOPE.set(None)
