"""FastAPI, installed as the README says (app.add_middleware), served by
uvicorn over real HTTP: async and sync (thread-pool) endpoints."""

from __future__ import annotations

import logging
import time
import unittest
from typing import Any

import devbench
from devbench import _core

from devbench import _requests, reqsignal

from .helpers import FakeIngest, fresh, http, require, serve_asgi
from .request_cases import CLOCK, USER, RequestSignalCases, install_clocks, restore_clocks

try:  # module level: FastAPI resolves endpoint annotations in the module
    from fastapi import FastAPI, HTTPException, Request
except ImportError:  # require() below fails or skips the tests
    pass

log = logging.getLogger("acme.api")


class PaymentDeclined(Exception):
    pass


async def charge(order: str, request: "object") -> None:
    user = request.headers.get("x-demo-user")  # type: ignore[attr-defined]
    devbench.set_user(email=user, account="acct-9")
    log.info("charging %s for order %s", user, order)
    raise PaymentDeclined("declined for %s" % user)


def sync_charge(request: "object") -> None:
    # A sync endpoint runs in a thread pool, in a copy of the context.
    devbench.set_user(email=request.headers.get("x-demo-user"), account="acct-9")  # type: ignore[attr-defined]
    log.info("sync charge")
    raise PaymentDeclined("sync declined")


class FastAPITest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        require("fastapi", unittest.TestCase())
        require("uvicorn", unittest.TestCase())
        from devbench.fastapi import DevbenchMiddleware

        cls.ingest = FakeIngest()
        fresh(cls.ingest.dsn(), flush_interval=3600)
        logging.getLogger("acme").setLevel(logging.INFO)
        app = FastAPI()

        @app.get("/charge/{order}")
        async def charge_route(order: str, request: Request) -> None:
            await charge(order, request)

        @app.get("/sync")
        def sync_route(request: Request) -> None:
            sync_charge(request)

        @app.get("/missing")
        async def missing() -> None:
            raise HTTPException(status_code=404)

        @app.get("/handled")
        def handled() -> dict:
            devbench.report_handled(ValueError("dup"), symbol="api.handled")
            return {"ok": True}

        app.add_middleware(DevbenchMiddleware)
        cls.base, cls.stop = serve_asgi(app)

    @classmethod
    def tearDownClass(cls) -> None:
        cls.stop()
        cls.ingest.close()
        _core.reset()

    def setUp(self) -> None:
        devbench.flush()
        with self.ingest.lock:
            self.ingest.requests.clear()
        self.ingest.ask_evidence = True
        self.ingest.need_logs = []

    def test_an_unhandled_error_is_reported_with_trace_user_and_logs(self) -> None:
        self.ingest.need_logs = ["fa1/charge"]
        status, _, _ = http("GET", self.base + "/charge/77", {"x-adt-trace": "v1/fa1/charge/0", "X-Demo-User": "Lee@Acme.test"})
        self.assertEqual(500, status, "Starlette still answers its own 500")
        self.assertTrue(devbench.flush())
        self.assertEqual([{"email": "lee@acme.test", "account": "acct-9"}], self.ingest.counts()[0]["users"])
        self.assertTrue(self.ingest.wait_for(lambda: len(self.ingest.evidence()) == 1))
        self.assertEqual("tests.test_fastapi.PaymentDeclined at tests.test_fastapi.FastAPITest.setUpClass.<locals>.charge_route",
                         self.ingest.evidence()[0].json()["template"])
        self.assertIn("[v1/fa1/charge/0] INFO charging <email> for order <num>",
                      [line for s in self.ingest.slices() for line in s["lines"]])

    def test_a_sync_endpoint_keeps_its_user_and_trace(self) -> None:
        self.ingest.need_logs = ["fa2/sync"]
        status, _, _ = http("GET", self.base + "/sync", {"x-adt-trace": "v1/fa2/sync/0", "X-Demo-User": "t@x.test"})
        self.assertEqual(500, status)
        self.assertTrue(devbench.flush())
        self.assertEqual([{"email": "t@x.test", "account": "acct-9"}], self.ingest.counts()[0]["users"])
        self.assertIn("[v1/fa2/sync/0] INFO sync charge", [line for s in self.ingest.slices() for line in s["lines"]])

    def test_an_http_exception_is_not_a_failure(self) -> None:
        self.assertEqual(404, http("GET", self.base + "/missing")[0])
        devbench.flush()
        self.assertEqual([], self.ingest.counts())

    def test_a_handled_failure_marks_a_traced_response(self) -> None:
        _, headers, _ = http("GET", self.base + "/handled", {"x-adt-trace": "v1/fa3/h/0"})
        self.assertEqual("1", headers.get("x-adt-handled"))
        self.assertEqual("x-adt-handled", headers.get("access-control-expose-headers"))
        _, untraced, _ = http("GET", self.base + "/handled")
        self.assertNotIn("x-adt-handled", untraced)


class FastAPIRequestSignalsTest(RequestSignalCases, unittest.TestCase):
    routes = {"fail": "/fail/{order}", "boom": "/boom", "captured": "/captured", "handled": "/handled",
              "slow": "/slow/{ms}", "stream-fail": "/stream-fail", "real-slow": "/real-slow", "limited": "/limited"}

    @classmethod
    def setUpClass(cls) -> None:
        require("fastapi", unittest.TestCase())
        require("uvicorn", unittest.TestCase())
        from fastapi.responses import PlainTextResponse

        from devbench.fastapi import DevbenchMiddleware

        cls.ingest = FakeIngest()
        fresh(cls.ingest.dsn(), flush_interval=3600)
        app = FastAPI()

        @app.get("/fail/{order}")
        async def fail(order: int) -> Any:
            devbench.set_user(**USER)
            return PlainTextResponse("inventory unavailable", status_code=503)

        @app.get("/unavailable")
        async def unavailable() -> Any:
            raise HTTPException(status_code=503)

        @app.get("/boom")
        async def boom() -> Any:
            raise PaymentDeclined("boom")

        @app.get("/captured")
        async def captured() -> Any:
            devbench.capture_exception(ValueError("upstream refused"))
            return PlainTextResponse("bad gateway", status_code=502)

        @app.get("/handled")
        def handled() -> Any:  # sync: runs in the thread pool
            devbench.report_handled(ValueError("dup"), symbol="api.signals.handled")
            return PlainTextResponse("unavailable", status_code=503)

        @app.get("/slow/{ms}")
        def slow(ms: int) -> Any:
            devbench.set_user(**USER)
            CLOCK.advance_ms(ms)
            return {"ok": True}

        @app.get("/slow-captured")
        async def slow_captured() -> Any:
            devbench.capture_exception(ValueError("cache miss"))
            CLOCK.advance_ms(4000)
            return {"ok": True}

        @app.get("/stream")
        async def stream() -> Any:
            CLOCK.advance_ms(60000)
            return PlainTextResponse("data: x\n\n", media_type="text/event-stream")

        @app.get("/stream-fail")
        async def stream_fail() -> Any:
            return PlainTextResponse("data: x\n\n", status_code=503, media_type="text/event-stream")

        @app.get("/limited")
        async def limited() -> Any:
            devbench.set_user(**USER)
            return PlainTextResponse("slow down", status_code=429)

        @app.get("/real-slow")
        def real_slow() -> Any:
            time.sleep(3.1)
            return {"ok": True}

        app.add_middleware(DevbenchMiddleware)
        cls.base, cls.stop = serve_asgi(app)

    @classmethod
    def tearDownClass(cls) -> None:
        restore_clocks()
        cls.stop()
        cls.ingest.close()
        _core.reset()

    def setUp(self) -> None:
        self.reset_signals()

    def test_an_http_exception_with_a_5xx_is_failed(self) -> None:
        self.assertEqual(503, self.get("/unavailable")[0])
        entry = self.only(self.flushed())
        self.assertEqual(self.fp("failed", "/unavailable", 503), entry["fp"])


class StarletteRequestSignalsTest(unittest.TestCase):
    """Plain Starlette: the route is the matched Route's path on every
    supported Starlette (newer ones record it in the scope, older ones only
    the endpoint); nothing matched is "*"."""

    @classmethod
    def setUpClass(cls) -> None:
        require("starlette", unittest.TestCase())
        require("uvicorn", unittest.TestCase())
        from starlette.applications import Starlette
        from starlette.middleware import Middleware
        from starlette.responses import PlainTextResponse
        from starlette.routing import Route

        from devbench.asgi import DevbenchMiddleware

        async def fail(request: Any) -> Any:
            return PlainTextResponse("no", status_code=500)

        async def slow(request: Any) -> Any:
            CLOCK.advance_ms(3500)
            return PlainTextResponse("ok")

        async def not_found(request: Any, exc: Any) -> Any:
            CLOCK.advance_ms(4000)
            return PlainTextResponse("missing", status_code=404)

        cls.ingest = FakeIngest()
        fresh(cls.ingest.dsn(), flush_interval=3600)
        app = Starlette(routes=[Route("/orders/{id:int}/fail", fail), Route("/slow", slow)],
                        middleware=[Middleware(DevbenchMiddleware)], exception_handlers={404: not_found})
        cls.base, cls.stop = serve_asgi(app)

    @classmethod
    def tearDownClass(cls) -> None:
        restore_clocks()
        cls.stop()
        cls.ingest.close()
        _core.reset()

    def setUp(self) -> None:
        install_clocks()
        devbench.flush()
        with self.ingest.lock:
            self.ingest.requests.clear()

    def fp(self, signal: str, route: str, status: int) -> Any:
        return reqsignal.build(signal, "GET", route, status).fingerprint(_core.config().resolved_service())

    def test_the_route_is_the_pattern_and_unmatched_is_star(self) -> None:
        before = _requests.finished()
        self.assertEqual(500, http("GET", self.base + "/orders/77/fail")[0])
        self.assertEqual(404, http("GET", self.base + "/nowhere/123")[0])
        self.assertTrue(self.ingest.wait_for(lambda: _requests.finished() >= before + 2))
        self.assertTrue(devbench.flush())
        got = sorted((c["fp"], c["kind"]) for c in self.ingest.counts())
        want = sorted([(self.fp("failed", "/orders/{id:int}/fail", 500), "error"),
                       (self.fp("slow", "*", 404), "degradation")])
        self.assertEqual(want, got)


if __name__ == "__main__":
    unittest.main()
