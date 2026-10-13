"""Capability 2b (failed and slow requests, 429 spikes) against a real app
under each framework's middleware, over real HTTP, into the real-HTTP ingest
stand-in (direct mode).

Each framework's test serves the same endpoints (its own route syntax) and
mixes in RequestSignalCases:

    GET /fail/<id>       503 without raising
    GET /boom            raises (the framework answers 500)
    GET /captured        capture_exception, then 502
    GET /handled         report_handled, then 503
    GET /slow/<ms>       the clock moves <ms>, then 200
    GET /slow-captured   capture_exception, the clock moves 4000 ms, then 200
    GET /stream          text/event-stream; charset=utf-8, 60 s, 200
    GET /stream-fail     text/event-stream, 503
    GET /limited         429
    GET /real-slow       sleeps 3.1 s for real, then 200

The one seam is the clock (devbench.reqsignal.monotonic_ns / wall), so a
duration can be exact to the millisecond; everything else is real.
"""

from __future__ import annotations

import threading
import time
import unittest
from typing import Any, Dict, List, Optional

import devbench
from devbench import _core, _requests, reqsignal

from .helpers import FakeIngest, http

USER = {"email": "pat@acme.test", "account": "acct-1"}
# A future minute, so no real-clock observation can be "later" than it.
WINDOW_START = (int(time.time()) // 60 + 24 * 60) * 60


class Clock:
    """Frozen monotonic nanoseconds that endpoints move on purpose."""

    def __init__(self) -> None:
        self.ns = 10 ** 15
        self.lock = threading.Lock()

    def __call__(self) -> int:
        with self.lock:
            return self.ns

    def advance_ms(self, ms: int) -> None:
        with self.lock:
            self.ns += int(ms) * 1000000


CLOCK = Clock()


class Wall:
    def __init__(self) -> None:
        self.seconds = WINDOW_START + 1.0

    def __call__(self) -> float:
        return self.seconds


WALL = Wall()


def install_clocks() -> None:
    reqsignal.monotonic_ns = CLOCK
    reqsignal.wall = WALL


def restore_clocks() -> None:
    reqsignal.monotonic_ns = time.monotonic_ns
    reqsignal.wall = time.time


class RequestSignalCases:
    """Mixed into a unittest.TestCase that sets ``base`` (the app's URL),
    ``ingest`` and ``routes`` (endpoint name -> the route pattern the
    framework reports)."""

    base: str
    ingest: FakeIngest
    routes: Dict[str, str]
    handled_path = "/handled"

    # ---- helpers

    def reset_signals(self) -> None:
        install_clocks()
        WALL.seconds = WINDOW_START + 1.0
        transport = _core.transport()
        transport.end_requests()
        devbench.flush()
        with self.ingest.lock:
            self.ingest.requests.clear()
            self.ingest._asked.clear()
        self.ingest.ask_evidence = False

    def get(self, path: str, headers: Optional[Dict[str, str]] = None, evaluated: bool = True) -> "tuple[int, Dict[str, str], bytes]":
        """A real request. ``evaluated``: wait until the middleware has
        evaluated it (an ASGI request is evaluated after its response
        reached the client)."""
        before = _requests.finished()
        answer = http("GET", self.base + path, headers)
        if evaluated:
            assert self.ingest.wait_for(lambda: _requests.finished() > before, 10), "request not evaluated"
        return answer

    def flushed(self) -> List[dict]:
        self.assertTrue(devbench.flush(), "flush")  # type: ignore[attr-defined]
        return self.ingest.counts()

    def fp(self, signal: str, route: str, status: int = 0) -> str:
        built = reqsignal.build(signal, "GET", route, status or 200, 0, 10, 10)
        fp = built.fingerprint(_core.config().resolved_service())
        assert fp is not None
        return fp

    def only(self, counts: List[dict]) -> dict:
        t: Any = self
        t.assertEqual(1, len(counts), counts)
        return counts[0]

    # ---- the cases

    def test_signals_a_503_without_raising_is_failed_once(self) -> None:
        t: Any = self
        self.ingest.ask_evidence = True
        status, _, _ = self.get("/fail/1182", {"x-adt-trace": "v1/rs1/fail/0"})
        t.assertEqual(503, status)
        entry = self.only(self.flushed())
        t.assertEqual(self.fp("failed", self.routes["fail"], 503), entry["fp"])
        t.assertEqual("error", entry["kind"])
        t.assertEqual(1, entry["n"])
        t.assertEqual([USER], entry["users"])
        t.assertTrue(self.ingest.wait_for(lambda: len(self.ingest.evidence()) == 1))
        bundle = self.ingest.evidence()[0].json()
        t.assertEqual("http_503 at GET %s" % self.routes["fail"], bundle["template"])
        t.assertEqual("error", bundle["kind"])
        t.assertEqual(1, len(bundle["exemplars"]))
        t.assertIn("GET %s in" % self.routes["fail"], bundle["exemplars"][0])
        t.assertNotIn("1182", bundle["exemplars"][0] + bundle["template"], "the raw path never leaves")

    def test_signals_a_raised_error_is_only_the_exception(self) -> None:
        t: Any = self
        status, _, _ = self.get("/boom", evaluated=False)
        t.assertEqual(500, status)
        entry = self.only(self.flushed())
        t.assertNotEqual(self.fp("failed", self.routes["boom"], 500), entry["fp"])
        t.assertEqual("error", entry["kind"])

    def test_signals_a_captured_exception_wins_over_its_5xx(self) -> None:
        t: Any = self
        t.assertEqual(502, self.get("/captured")[0])
        entry = self.only(self.flushed())
        t.assertNotEqual(self.fp("failed", self.routes["captured"], 502), entry["fp"])

    def test_signals_report_handled_then_503_is_still_failed(self) -> None:
        t: Any = self
        status, headers, _ = self.get(self.handled_path, {"x-adt-trace": "v1/rs2/handled/0"})
        t.assertEqual(503, status)
        t.assertEqual("1", headers.get("x-adt-handled"), "the header is unchanged")
        fps = {c["fp"]: c["kind"] for c in self.flushed()}
        t.assertEqual(2, len(fps), fps)
        t.assertEqual("error", fps.get(self.fp("failed", self.routes["handled"], 503)))
        t.assertIn("handled_failure", fps.values())

    def test_signals_slow_is_strictly_over_three_seconds(self) -> None:
        t: Any = self
        t.assertEqual(200, self.get("/slow/3000")[0])
        t.assertEqual([], self.flushed(), "exactly 3,000 ms is not slow")
        t.assertEqual(200, self.get("/slow/3001")[0])
        entry = self.only(self.flushed())
        t.assertEqual(self.fp("slow", self.routes["slow"]), entry["fp"])
        t.assertEqual("degradation", entry["kind"])
        t.assertEqual([USER], entry["users"])

    def test_signals_a_slow_request_after_a_captured_exception_is_still_slow(self) -> None:
        t: Any = self
        t.assertEqual(200, self.get("/slow-captured")[0])
        kinds = sorted(c["kind"] for c in self.flushed())
        t.assertEqual(["degradation", "error"], kinds)

    def test_signals_a_stream_is_never_slow_but_can_fail(self) -> None:
        t: Any = self
        t.assertEqual(200, self.get("/stream")[0])
        t.assertEqual([], self.flushed(), "an event stream lasting 60 s is not slow")
        t.assertEqual(503, self.get("/stream-fail")[0])
        entry = self.only(self.flushed())
        t.assertEqual(self.fp("failed", self.routes["stream-fail"], 503), entry["fp"])

    def test_signals_a_real_slow_request(self) -> None:
        t: Any = self
        restore_clocks()
        try:
            t.assertEqual(200, self.get("/real-slow")[0])
        finally:
            install_clocks()
        entry = self.only(self.flushed())
        t.assertEqual(self.fp("slow", self.routes["real-slow"]), entry["fp"])

    def test_signals_a_404_and_a_fast_200_are_nothing(self) -> None:
        t: Any = self
        t.assertEqual(404, self.get("/no/such/page")[0])
        t.assertEqual(200, self.get("/slow/5")[0])
        t.assertEqual([], self.flushed())

    def test_signals_ten_429s_are_one_spike_per_window(self) -> None:
        t: Any = self
        for _ in range(10):
            t.assertEqual(429, self.get("/limited", {"x-adt-trace": "v1/rs3/limited/0"})[0])
        t.assertEqual([], self.flushed(), "the window is still open")
        WALL.seconds = WINDOW_START + 60.0
        entry = self.only(self.flushed())
        t.assertEqual(self.fp("rate_limit_spike", self.routes["limited"]), entry["fp"])
        t.assertEqual("error", entry["kind"])
        t.assertEqual(1, entry["n"])
        t.assertNotIn("users", entry, "a spike carries no user")
        with self.ingest.lock:
            self.ingest.requests.clear()
        WALL.seconds = WINDOW_START + 125.0
        t.assertEqual([], self.flushed(), "one spike per window")
