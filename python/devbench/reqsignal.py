"""Request signals (docs/server-sdk/failed-and-slow-requests.md, "Capability
2b"; DECISIONS #171): what a request does without raising.

* ``failed``: the application answered 500-599 and no exception was
  reported during the request;
* ``slow``: it answered 100-499, not streaming, after more than 3,000 ms;
* ``rate_limit_spike``: one route's 429s in one unix minute met the spike
  rule (at least ten, and at least 10% of that route's requests).

A port of the backend's internal/reqsignal (the Go SDK carries the same
copy). A copy can drift, and a drifted copy reports a different set of
problems by language, so it is held to the shared vectors
(testdata/request_signal_vectors.json). Change nothing here without
changing internal/reqsignal in lockstep.
"""

from __future__ import annotations

import threading
import time
from typing import Dict, List, NamedTuple, Optional, Tuple

from . import fingerprint

# Constants, not configuration: every SDK must apply the same numbers, or
# the same app reports differently by language. Tenant-configurable
# thresholds are a later issue (DECISIONS #171).
SLOW_THRESHOLD_MS = 3000
SPIKE_WINDOW_SECONDS = 60
SPIKE_MIN_COUNT = 10
SPIKE_MIN_PERCENT = 10
MAX_ROUTES_PER_WINDOW = 512
MAX_ROUTE_CHARS = 255

FAILED = "failed"
SLOW = "slow"
RATE_LIMIT_SPIKE = "rate_limit_spike"

# A request matched to no route pattern: one literal, never the raw path
# (a path carries ids and slugs).
UNKNOWN_ROUTE = "*"
# Any method outside the standard set, so invented methods cannot mint
# fingerprints.
OTHER_METHOD = "OTHER"

# The clocks the middlewares and the flushes read: wall time for spike
# windows, the monotonic clock (integer nanoseconds, so a duration is
# truncated to whole milliseconds exactly) for durations. Module attributes
# only so a test can pin a duration to the nanosecond; never changed outside
# tests.
wall = time.time
monotonic_ns = time.monotonic_ns


def now_ms() -> int:
    return int(wall() * 1000)


_METHODS = frozenset(["GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "TRACE", "CONNECT"])


def classify(status: int, duration_ms: int, exception_reported: bool, streaming: bool) -> str:
    """The signal one finished request produces: FAILED, SLOW or "" for
    none. At most one per request. A 429 is not classified here; it feeds
    the spike detector."""
    if status < 100 or status > 599:
        return ""
    if status >= 500:
        # The exception is the report.
        return "" if exception_reported else FAILED
    if streaming or status == 101:
        return ""
    if duration_ms > SLOW_THRESHOLD_MS:
        return SLOW
    return ""


def _go_upper(text: str) -> str:
    # Go's strings.ToUpper maps one code point to one; Python's full
    # mapping turns "ﬆ" into "ST", which would make "poﬆ" a POST.
    return "".join(u if len(u) == 1 else c for c, u in ((c, c.upper()) for c in text))


def normalize_method(method: object) -> str:
    """Uppercased and trimmed; anything outside the standard set is OTHER."""
    m = _go_upper(fingerprint.trim(fingerprint.utf8(method)))
    return m if m in _METHODS else OTHER_METHOD


def normalize_route(route: object) -> str:
    """Trimmed, at most MAX_ROUTE_CHARS code points; empty is UNKNOWN_ROUTE."""
    r = fingerprint.trim(fingerprint.utf8(route))
    if not r:
        return UNKNOWN_ROUTE
    return r[:MAX_ROUTE_CHARS]


class Built(NamedTuple):
    """Everything the direct transport needs to count a request signal."""

    kind: str  # the count's kind: error or degradation
    type: str  # the fingerprint's type
    message: str  # "<METHOD> <route>", the fingerprint's message
    text: str  # template text kept for evidence
    exemplar: str  # raw evidence line, scrubbed at egress

    def fingerprint(self, service: str) -> Optional[str]:
        return fingerprint.compute(self.kind, "server", service, self.type, self.message, [])


class InvalidSignal(ValueError):
    pass


def build(signal: str, method: object, route: object, status: int = 0, duration_ms: int = 0,
          count: int = 0, total: int = 0) -> Built:
    """A request signal as the sidecar builds it from the ``request``
    control message. Raises InvalidSignal for a signal/status pair a sensor
    must refuse."""
    message = "%s %s" % (normalize_method(method), normalize_route(route))
    status = _int(status)
    if signal == FAILED:
        if status < 500 or status > 599:
            raise InvalidSignal("failed with status %d" % status)
        kind, type_ = "error", "http_%d" % status
        exemplar = "%s: %s in %d ms" % (type_, message, _non_negative(duration_ms))
    elif signal == SLOW:
        if status < 100 or status > 499:
            raise InvalidSignal("slow with status %d" % status)
        kind, type_ = "degradation", "slow_request"
        exemplar = "%s: %s in %d ms (status %d)" % (type_, message, _non_negative(duration_ms), status)
    elif signal == RATE_LIMIT_SPIKE:
        kind, type_ = "error", "http_429_spike"
        exemplar = "%s: %s %d of %d requests answered 429 in one minute" % (
            type_, message, _non_negative(count), _non_negative(total))
    else:
        raise InvalidSignal("signal %r" % (signal,))
    return Built(kind, type_, message, "%s at %s" % (type_, message), exemplar)


class Spike(NamedTuple):
    method: str
    route: str
    window: int  # the window's start, unix seconds
    count: int  # its 429s
    total: int  # its requests


def is_spike(count: int, total: int) -> bool:
    """The rule, in integer arithmetic."""
    return count >= SPIKE_MIN_COUNT and count * 100 >= total * SPIKE_MIN_PERCENT


def window_of(t_ms: int) -> int:
    """The unix-minute index of a time in milliseconds (floored, so a
    pre-1970 clock still maps one window per minute)."""
    return int(t_ms) // (SPIKE_WINDOW_SECONDS * 1000)


class SpikeDetector:
    """The 429 spike rule over one process's requests. Thread-safe: every
    request's thread observes, the flush thread ticks.

    One window is open at a time. An observation in a later window closes
    the open one first; one in an earlier window (a clock step back) counts
    in the open window. Bounded by MAX_ROUTES_PER_WINDOW; nothing is kept
    past the open window."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._window: Optional[int] = None
        self._routes: Dict[Tuple[str, str], List[int]] = {}
        self.dropped = 0

    def observe(self, t_ms: int, method: object, route: object, status: int) -> List[Spike]:
        """Records one finished request; returns the spikes of any window it
        closed."""
        w = window_of(t_ms)
        key = (normalize_method(method), normalize_route(route))
        with self._lock:
            closed: List[Spike] = []
            if self._window is None:
                self._start(w)
            elif w > self._window:
                closed = self._evaluate()
                self._start(w)
            tally = self._routes.get(key)
            if tally is None:
                if len(self._routes) >= MAX_ROUTES_PER_WINDOW:
                    self.dropped += 1
                    return closed
                tally = self._routes[key] = [0, 0]
            tally[1] += 1
            if status == 429:
                tally[0] += 1
            return closed

    def tick(self, now_ms: int) -> List[Spike]:
        """Closes the open window when ``now_ms`` is past it (each flush)."""
        with self._lock:
            if self._window is None or window_of(now_ms) <= self._window:
                return []
            return self._close()

    def end(self) -> List[Spike]:
        """Evaluates the open window as it stands (process exit) and closes
        it."""
        with self._lock:
            if self._window is None:
                return []
            return self._close()

    # Caller holds self._lock.

    def _start(self, w: int) -> None:
        self._window = w
        self._routes = {}

    def _close(self) -> List[Spike]:
        out = self._evaluate()
        self._window = None
        self._routes = {}
        return out

    def _evaluate(self) -> List[Spike]:
        start = (self._window or 0) * SPIKE_WINDOW_SECONDS
        out = [Spike(m, r, start, c, t) for (m, r), (c, t) in self._routes.items() if is_spike(c, t)]
        out.sort(key=lambda s: (s.method.encode("utf-8", "replace"), s.route.encode("utf-8", "replace")))
        return out


def _int(value: object) -> int:
    try:
        return int(value)  # type: ignore[arg-type]
    except Exception:
        return 0


def _non_negative(value: object) -> int:
    n = _int(value)
    return n if n > 0 else 0
