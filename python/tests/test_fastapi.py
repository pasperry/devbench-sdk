"""FastAPI, installed as the README says (app.add_middleware), served by
uvicorn over real HTTP: async and sync (thread-pool) endpoints."""

from __future__ import annotations

import logging
import unittest

import devbench
from devbench import _core

from .helpers import FakeIngest, fresh, http, require, serve_asgi

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


if __name__ == "__main__":
    unittest.main()
