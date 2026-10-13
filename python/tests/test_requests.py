"""Capability 2b on bare WSGI and ASGI apps (route "*"), and the 429 spike
detector's lifecycle and thread safety, over real HTTP into the real-HTTP
ingest stand-in. The framework cases are in test_flask, test_fastapi and
test_django (tests/request_cases.py)."""

from __future__ import annotations

import sys
import threading
import unittest
from typing import Any, List

import devbench
from devbench import _core, _requests, reqsignal
from devbench.wsgi import DevbenchMiddleware

from .helpers import FakeIngest, fresh, http, require, serve_asgi, serve_wsgi
from .request_cases import CLOCK, WALL, WINDOW_START, install_clocks, restore_clocks


def wsgi_app(environ: dict, start_response: Any) -> Any:
    path = environ.get("PATH_INFO")
    if path.startswith("/fail/"):
        start_response("503 Service Unavailable", [("Content-Type", "text/plain")])
        return [b"unavailable"]
    if path == "/limited":
        start_response("429 Too Many Requests", [("Content-Type", "text/plain")])
        return [b"slow down"]
    if path == "/slow":
        CLOCK.advance_ms(3500)
        start_response("200 OK", [("Content-Type", "text/plain")])
        return [b"ok"]
    if path == "/bad-status":
        start_response("999 Odd", [("Content-Type", "text/plain")])
        return [b"odd"]
    if path == "/raise":
        raise RuntimeError("the app failed")
    if path == "/generator":
        def body() -> Any:
            # start_response on the first item, as a generator app does.
            start_response("500 Internal Server Error", [("Content-Type", "text/plain")])
            yield b"partial"
        return body()
    if path == "/generator-raises":
        def broken() -> Any:
            start_response("200 OK", [("Content-Type", "text/plain")])
            CLOCK.advance_ms(9000)
            raise RuntimeError("the body failed")
            yield b""  # pragma: no cover
        return broken()
    start_response("200 OK", [("Content-Type", "text/plain")])
    return [b"fine"]


class Base(unittest.TestCase):
    ingest: FakeIngest
    base: str

    def setUp(self) -> None:
        install_clocks()
        WALL.seconds = WINDOW_START + 1.0
        transport = _core.transport()
        transport.end_requests()
        devbench.flush()
        with self.ingest.lock:
            self.ingest.requests.clear()
            self.ingest._asked.clear()
        self.ingest.ask_evidence = False

    def get(self, path: str, evaluated: bool = True) -> "tuple[int, dict, bytes]":
        before = _requests.finished()
        answer = http("GET", self.base + path)
        if evaluated:
            self.assertTrue(self.ingest.wait_for(lambda: _requests.finished() > before), "request not evaluated")
        return answer

    def fp(self, signal: str, route: str, status: int = 200) -> str:
        fp = reqsignal.build(signal, "GET", route, status).fingerprint(_core.config().resolved_service())
        assert fp is not None
        return fp

    def flushed(self) -> List[dict]:
        self.assertTrue(devbench.flush())
        return self.ingest.counts()


class WSGIRequestSignalsTest(Base):
    @classmethod
    def setUpClass(cls) -> None:
        cls.ingest = FakeIngest()
        fresh(cls.ingest.dsn(), flush_interval=3600)
        cls.base, cls.stop = serve_wsgi(DevbenchMiddleware(wsgi_app))
        cls.nested_base, cls.nested_stop = serve_wsgi(DevbenchMiddleware(DevbenchMiddleware(wsgi_app)))

    @classmethod
    def tearDownClass(cls) -> None:
        restore_clocks()
        cls.stop()
        cls.nested_stop()
        cls.ingest.close()
        _core.reset()

    def test_a_bare_wsgi_503_is_failed_under_star_and_never_the_path(self) -> None:
        self.ingest.ask_evidence = True
        self.assertEqual(503, self.get("/fail/1182")[0])
        counts = self.flushed()
        self.assertEqual([(self.fp("failed", "*", 503), "error", 1)], [(c["fp"], c["kind"], c["n"]) for c in counts])
        self.assertTrue(self.ingest.wait_for(lambda: len(self.ingest.evidence()) == 1))
        bundle = self.ingest.evidence()[0].json()
        self.assertEqual("http_503 at GET *", bundle["template"])
        self.assertNotIn("1182", bundle["template"] + "".join(bundle["exemplars"]))

    def test_a_slow_bare_wsgi_request(self) -> None:
        self.assertEqual(200, self.get("/slow")[0])
        self.assertEqual([self.fp("slow", "*")], [c["fp"] for c in self.flushed()])

    def test_a_generator_app_is_evaluated_once_its_status_is_known(self) -> None:
        self.assertEqual(500, self.get("/generator")[0])
        self.assertEqual([self.fp("failed", "*", 500)], [c["fp"] for c in self.flushed()])

    def test_a_request_that_raised_out_is_only_the_exception(self) -> None:
        before = _requests.finished()
        self.assertEqual(500, http("GET", self.base + "/raise")[0])
        self.get("/")
        self.assertEqual(before + 1, _requests.finished(), "the raised request was not evaluated")
        counts = self.flushed()
        self.assertEqual(1, len(counts))
        self.assertNotIn(counts[0]["fp"], (self.fp("failed", "*", 500),))

    def test_a_body_that_raises_is_not_evaluated(self) -> None:
        before = _requests.finished()
        try:
            http("GET", self.base + "/generator-raises")
        except Exception:
            pass  # the server drops the connection mid-response
        self.get("/")  # a later request has been evaluated: the first had its chance
        self.assertEqual(before + 1, _requests.finished())
        self.assertEqual([], self.flushed())

    def test_a_status_outside_100_to_599_is_nothing_and_the_response_is_intact(self) -> None:
        status, _, body = self.get("/bad-status")
        self.assertEqual((999, b"odd"), (status, body))
        self.assertEqual([], self.flushed())

    def test_nested_middleware_observes_once(self) -> None:
        before = _requests.finished()
        self.assertEqual(503, http("GET", self.nested_base + "/fail/1")[0])
        self.assertEqual(before + 1, _requests.finished())
        self.assertEqual([1], [c["n"] for c in self.flushed()])

    def test_the_next_request_in_a_later_minute_closes_a_spike_window(self) -> None:
        for _ in range(10):
            self.assertEqual(429, self.get("/limited")[0])
        WALL.seconds = WINDOW_START + 61.0
        self.get("/")
        # Counted when the request closed the window, before any flush.
        self.assertEqual({self.fp("rate_limit_spike", "*", 429): 1}, _core.transport().pending())
        self.assertEqual([self.fp("rate_limit_spike", "*", 429)], [c["fp"] for c in self.flushed()])

    def test_nine_429s_and_under_ten_percent_are_not_spikes(self) -> None:
        for _ in range(9):
            self.get("/limited")
        WALL.seconds = WINDOW_START + 61.0
        self.assertEqual([], self.flushed())
        for _ in range(10):
            self.get("/limited")
        for _ in range(91):
            self.get("/")
        _core.transport().end_requests()
        self.assertEqual([], self.flushed())

    def test_process_exit_reports_the_open_window(self) -> None:
        for _ in range(10):
            self.get("/limited")
        _core._exit_flush()
        self.assertEqual([self.fp("rate_limit_spike", "*", 429)], [c["fp"] for c in self.ingest.counts()])

    def test_reconfiguring_ends_the_window_like_exit(self) -> None:
        for _ in range(10):
            self.get("/limited")
        service = _core.config().resolved_service()
        want = self.fp("rate_limit_spike", "*", 429)
        devbench.configure(release="r2")  # the old transport stops: last window, last flush
        self.assertEqual([want], [c["fp"] for c in self.ingest.counts()])
        self.assertEqual(service, _core.config().resolved_service())

    def test_concurrent_requests_and_flushes_count_one_exact_spike(self) -> None:
        errors: List[BaseException] = []
        stop = threading.Event()

        def flusher() -> None:
            while not stop.is_set():
                devbench.flush()

        def client() -> None:
            try:
                for _ in range(25):
                    status, _, _ = http("GET", self.base + "/limited")
                    if status != 429:
                        raise AssertionError(status)
            except BaseException as e:  # noqa: BLE001
                errors.append(e)

        before = _requests.finished()
        flushing = threading.Thread(target=flusher)
        flushing.start()
        clients = [threading.Thread(target=client) for _ in range(16)]
        for t in clients:
            t.start()
        for t in clients:
            t.join(60)
        stop.set()
        flushing.join(30)
        self.assertEqual([], errors)
        self.assertTrue(self.ingest.wait_for(lambda: _requests.finished() >= before + 400))
        self.assertEqual([], self.flushed(), "the window is still open")
        WALL.seconds = WINDOW_START + 60.0
        spike = self.fp("rate_limit_spike", "*", 429)
        counts = self.flushed()
        self.assertEqual([(spike, 1)], [(c["fp"], c["n"]) for c in counts])
        # The raw exemplar (numbers are templated at egress) has the exact
        # tallies: no observation was lost or counted twice.
        self.assertEqual("http_429_spike: GET * 400 of 400 requests answered 429 in one minute",
                         _core.transport()._details[spike].exemplar)


class SpikeDetectorRaceTest(unittest.TestCase):
    """Many threads observing one detector, with a tiny switch interval so
    threads interleave inside observe: every request is counted."""

    def test_no_observation_is_lost(self) -> None:
        detector = reqsignal.SpikeDetector()
        t_ms = WINDOW_START * 1000 + 1000
        previous = sys.getswitchinterval()
        sys.setswitchinterval(1e-6)
        try:
            def work(i: int) -> None:
                # 400 routes, every thread walking them in step: many
                # threads create the same route's tally at once.
                for n in range(2000):
                    detector.observe(t_ms, "GET", "/r%d" % (n % 400), 429 if n // 400 else 200)

            threads = [threading.Thread(target=work, args=(i,)) for i in range(16)]
            for t in threads:
                t.start()
            for t in threads:
                t.join()
        finally:
            sys.setswitchinterval(previous)
        spikes = detector.end()
        self.assertEqual(400, len(spikes))
        self.assertEqual(16 * 2000, sum(s.total for s in spikes))
        self.assertEqual(16 * 1600, sum(s.count for s in spikes))


class BackgroundFlushTest(unittest.TestCase):
    """With no request and no explicit flush, the background flush closes the
    window: the first 429 started it."""

    def test_the_background_flush_reports_a_spike(self) -> None:
        ingest = FakeIngest()
        try:
            fresh(ingest.dsn(), flush_interval=0.2)
            install_clocks()
            WALL.seconds = WINDOW_START + 1.0
            base, stop = serve_wsgi(DevbenchMiddleware(wsgi_app))
            try:
                for _ in range(10):
                    self.assertEqual(429, http("GET", base + "/limited")[0])
                WALL.seconds = WINDOW_START + 60.0
                want = reqsignal.build("rate_limit_spike", "GET", "*").fingerprint(_core.config().resolved_service())
                self.assertTrue(ingest.wait_for(lambda: [c["fp"] for c in ingest.counts()] == [want], 10),
                                ingest.counts())
            finally:
                stop()
        finally:
            restore_clocks()
            _core.reset()
            ingest.close()


class OffTest(unittest.TestCase):
    def test_nothing_is_observed_when_disabled_or_without_a_dsn(self) -> None:
        ingest = FakeIngest()
        base, stop = serve_wsgi(DevbenchMiddleware(wsgi_app))
        try:
            for config in ({"dsn": ingest.dsn(), "enabled": False}, {}):
                fresh(**config)
                before = _requests.finished()
                self.assertEqual(503, http("GET", base + "/fail/1")[0])
                self.assertEqual(before, _requests.finished(), config)
                self.assertTrue(devbench.flush())
            self.assertEqual([], ingest.counts())
        finally:
            stop()
            _core.reset()
            ingest.close()


async def asgi_app(scope: Any, receive: Any, send: Any) -> None:
    path = scope.get("path")
    if path == "/no-answer":
        return
    if path == "/sse":
        CLOCK.advance_ms(60000)
        await send({"type": "http.response.start", "status": 200,
                    "headers": [(b"content-type", b"text/event-stream; charset=utf-8")]})
        await send({"type": "http.response.body", "body": b"data: x\n\n"})
        return
    status = {"/fail/7": 502, "/slow": 200}.get(path, 200)
    if path == "/slow":
        CLOCK.advance_ms(3001)
    await send({"type": "http.response.start", "status": status, "headers": [(b"content-type", b"text/plain")]})
    await send({"type": "http.response.body", "body": b"x"})


class ASGIRequestSignalsTest(Base):
    @classmethod
    def setUpClass(cls) -> None:
        require("uvicorn", unittest.TestCase())
        from devbench.asgi import DevbenchMiddleware as ASGIMiddleware

        cls.ingest = FakeIngest()
        fresh(cls.ingest.dsn(), flush_interval=3600)
        cls.base, cls.stop = serve_asgi(ASGIMiddleware(ASGIMiddleware(asgi_app)))

    @classmethod
    def tearDownClass(cls) -> None:
        restore_clocks()
        cls.stop()
        cls.ingest.close()
        _core.reset()

    def test_bare_asgi_signals_under_star(self) -> None:
        self.assertEqual(502, self.get("/fail/7")[0])
        self.assertEqual(200, self.get("/slow")[0])
        self.assertEqual(200, self.get("/sse")[0])
        got = sorted((c["fp"], c["kind"], c["n"]) for c in self.flushed())
        want = sorted([(self.fp("failed", "*", 502), "error", 1), (self.fp("slow", "*"), "degradation", 1)])
        self.assertEqual(want, got)

    def test_an_app_that_never_answers_is_a_failed_500(self) -> None:
        self.assertEqual(500, self.get("/no-answer")[0])
        self.assertEqual([self.fp("failed", "*", 500)], [c["fp"] for c in self.flushed()])


if __name__ == "__main__":
    unittest.main()
