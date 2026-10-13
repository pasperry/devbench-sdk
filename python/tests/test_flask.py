"""Flask, installed as the README says (Devbench(app)), over real HTTP."""

from __future__ import annotations

import json
import logging
import time
import unittest
from typing import Any

import devbench
from devbench import _core

from .helpers import FakeIngest, fresh, http, require, serve_wsgi
from .request_cases import CLOCK, USER, RequestSignalCases, restore_clocks

log = logging.getLogger("acme.flask")


class PaymentDeclined(Exception):
    pass


class FlaskTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        require("flask", unittest.TestCase())
        from flask import Flask, abort, request

        from devbench.flask import Devbench

        cls.ingest = FakeIngest()
        fresh(cls.ingest.dsn(), flush_interval=3600)
        logging.getLogger("acme").setLevel(logging.INFO)

        app = Flask("acme_flask")

        @app.before_request
        def who() -> None:
            devbench.set_user(email=request.headers.get("X-Demo-User"), account="acct-7")

        @app.get("/charge/<order>")
        def charge(order: str) -> str:
            log.info("charging %s for order %s", request.headers.get("X-Demo-User"), order)
            raise PaymentDeclined("declined for %s" % request.headers.get("X-Demo-User"))

        @app.get("/missing")
        def missing() -> str:
            abort(404)

        @app.get("/handled")
        def handled() -> str:
            devbench.report_handled(ValueError("dup"), symbol="flask.handled")
            return "ok"

        @app.get("/fine")
        def fine() -> str:
            return json.dumps({"trace": str(devbench.current_trace()), "user": devbench.context.current_user()})

        Devbench(app)
        Devbench(app)  # twice: installs once
        cls.app = app
        cls.base, cls.stop = serve_wsgi(app)

    @classmethod
    def tearDownClass(cls) -> None:
        cls.stop()
        cls.ingest.close()
        _core.reset()

    def setUp(self) -> None:
        devbench.flush()
        with self.ingest.lock:
            self.ingest.requests.clear()
        self.ingest.ask_evidence = False
        self.ingest.need_logs = []

    def test_installing_twice_wraps_once(self) -> None:
        from devbench.wsgi import DevbenchMiddleware

        self.assertIsInstance(self.app.wsgi_app, DevbenchMiddleware)
        self.assertNotIsInstance(self.app.wsgi_app.app, DevbenchMiddleware)

    def test_the_service_is_the_apps_import_name(self) -> None:
        self.assertEqual("acme_flask", _core.config().resolved_service())

    def test_an_unhandled_error_is_reported_with_trace_user_and_logs(self) -> None:
        self.ingest.ask_evidence = True
        self.ingest.need_logs = ["fl1/charge"]
        status, _, _ = http("GET", self.base + "/charge/1182", {"x-adt-trace": "v1/fl1/charge/0", "X-Demo-User": "Sam@Acme.test"})
        self.assertEqual(500, status)
        self.assertTrue(devbench.flush())
        counts = self.ingest.counts()
        self.assertEqual(1, len(counts), "reported once, though it passed the signal and the middleware")
        self.assertEqual([{"email": "sam@acme.test", "account": "acct-7"}], counts[0]["users"])
        self.assertTrue(self.ingest.wait_for(lambda: len(self.ingest.evidence()) == 1))
        self.assertEqual("tests.test_flask.PaymentDeclined at tests.test_flask.FlaskTest.setUpClass.<locals>.charge",
                         self.ingest.evidence()[0].json()["template"])
        lines = [line for s in self.ingest.slices() for line in s["lines"]]
        self.assertIn("[v1/fl1/charge/0] INFO charging <email> for order <num>", lines)

    def test_an_abort_is_not_a_failure(self) -> None:
        self.assertEqual(404, http("GET", self.base + "/missing")[0])
        devbench.flush()
        self.assertEqual([], self.ingest.counts())

    def test_a_handled_failure_marks_a_traced_response(self) -> None:
        _, headers, _ = http("GET", self.base + "/handled", {"x-adt-trace": "v1/fl2/h/0"})
        self.assertEqual("1", headers.get("x-adt-handled"))
        self.assertEqual("x-adt-handled", headers.get("access-control-expose-headers"))

    def test_identity_and_trace_end_with_the_request(self) -> None:
        _, _, body = http("GET", self.base + "/fine", {"x-adt-trace": "v1/fl3/f/0", "X-Demo-User": "a@b.test"})
        self.assertEqual({"trace": "v1/fl3/f/0", "user": {"email": "a@b.test", "account": "acct-7"}}, json.loads(body))
        _, _, body = http("GET", self.base + "/fine")
        self.assertEqual({"trace": "None", "user": {"account": "acct-7"}}, json.loads(body))


class FlaskRequestSignalsTest(RequestSignalCases, unittest.TestCase):
    routes = {"fail": "/fail/<int:order>", "boom": "/boom", "captured": "/captured", "handled": "/handled",
              "slow": "/slow/<int:ms>", "stream-fail": "/stream-fail", "real-slow": "/real-slow", "limited": "/limited"}

    @classmethod
    def setUpClass(cls) -> None:
        require("flask", unittest.TestCase())
        from flask import Flask, Response

        from devbench.flask import Devbench

        cls.ingest = FakeIngest()
        fresh(cls.ingest.dsn(), flush_interval=3600)
        app = Flask("acme_signals")

        @app.before_request
        def who() -> None:
            devbench.set_user(**USER)

        @app.get("/fail/<int:order>")
        def fail(order: int) -> Any:
            return "inventory unavailable", 503

        @app.get("/boom")
        def boom() -> Any:
            raise PaymentDeclined("boom")

        @app.get("/captured")
        def captured() -> Any:
            devbench.capture_exception(ValueError("upstream refused"))
            return "bad gateway", 502

        @app.get("/handled")
        def handled() -> Any:
            devbench.report_handled(ValueError("dup"), symbol="flask.signals.handled")
            return "unavailable", 503

        @app.get("/slow/<int:ms>")
        def slow(ms: int) -> Any:
            CLOCK.advance_ms(ms)
            return "ok"

        @app.get("/slow-captured")
        def slow_captured() -> Any:
            devbench.capture_exception(ValueError("cache miss"))
            CLOCK.advance_ms(4000)
            return "ok"

        @app.get("/stream")
        def stream() -> Any:
            CLOCK.advance_ms(60000)
            return Response("data: x\n\n", status=200, content_type="text/event-stream; charset=utf-8")

        @app.get("/stream-fail")
        def stream_fail() -> Any:
            return Response("data: x\n\n", status=503, content_type="text/event-stream")

        @app.get("/limited")
        def limited() -> Any:
            return "slow down", 429

        @app.get("/real-slow")
        def real_slow() -> Any:
            time.sleep(3.1)
            return "ok"

        Devbench(app)
        cls.base, cls.stop = serve_wsgi(app)

    @classmethod
    def tearDownClass(cls) -> None:
        restore_clocks()
        cls.stop()
        cls.ingest.close()
        _core.reset()

    def setUp(self) -> None:
        self.reset_signals()


if __name__ == "__main__":
    unittest.main()
