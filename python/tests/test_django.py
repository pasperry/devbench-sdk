"""Django, as an app installs it ("devbench.django" in INSTALLED_APPS and
nothing else), served over real HTTP: by Django's WSGI handler and by its
ASGI handler under uvicorn. Run on its own (Django is configured once per
process)."""

from __future__ import annotations

import logging
import os
import unittest

from .helpers import FakeIngest, http, require, serve_asgi, serve_wsgi
from .request_cases import USER, RequestSignalCases, restore_clocks

INGEST: FakeIngest
BASE = ""
ASGI_BASE = ""
STOPS: list = []


def setUpModule() -> None:
    global INGEST, BASE, ASGI_BASE
    case = unittest.TestCase()
    require("django", case)
    INGEST = FakeIngest()
    for name in list(os.environ):
        if name.startswith(("DEVBENCH_", "ADT_")):
            del os.environ[name]
    os.environ["DEVBENCH_DSN"] = INGEST.dsn()
    os.environ["DJANGO_SETTINGS_MODULE"] = "tests.django_app.settings"
    from devbench import _core

    _core.reset()
    logging.getLogger("acme").setLevel(logging.INFO)

    from django.core.asgi import get_asgi_application
    from django.core.wsgi import get_wsgi_application

    BASE, stop = serve_wsgi(get_wsgi_application())
    STOPS.append(stop)
    ASGI_BASE, stop = serve_asgi(get_asgi_application())
    STOPS.append(stop)


def tearDownModule() -> None:
    for stop in STOPS:
        stop()
    INGEST.close()


class DjangoTest(unittest.TestCase):
    def setUp(self) -> None:
        import devbench

        devbench.flush()
        with INGEST.lock:
            INGEST.requests.clear()
        INGEST.ask_evidence = False
        INGEST.need_logs = []

    def test_the_app_config_puts_the_middleware_first_once(self) -> None:
        from django.conf import settings

        from devbench.django.apps import MIDDLEWARE_PATH, insert_middleware

        self.assertEqual(MIDDLEWARE_PATH, settings.MIDDLEWARE[0])
        self.assertFalse(insert_middleware(settings), "never twice")
        self.assertEqual(1, settings.MIDDLEWARE.count(MIDDLEWARE_PATH))

    def test_the_service_is_the_django_project(self) -> None:
        from devbench import _core

        self.assertEqual("tests", _core.config().resolved_service())

    def test_an_unhandled_error_is_reported_with_trace_user_and_logs(self) -> None:
        import devbench

        INGEST.ask_evidence = True
        INGEST.need_logs = ["dj1/checkout"]
        status, _, _ = http("GET", BASE + "/checkout", {"x-adt-trace": "v1/dj1/checkout/0", "X-Demo-User": "Pat@Acme.test"})
        self.assertEqual(500, status, "Django still answers its own 500")
        self.assertTrue(devbench.flush())
        counts = INGEST.counts()
        self.assertEqual(1, len(counts))
        self.assertEqual([{"email": "pat@acme.test", "account": "acct-1182"}], counts[0]["users"])
        self.assertTrue(INGEST.wait_for(lambda: len(INGEST.evidence()) == 1))
        bundle = INGEST.evidence()[0].json()
        self.assertEqual("tests", bundle["service"])
        self.assertEqual("tests.django_app.views.PaymentDeclined at tests.django_app.views.checkout", bundle["template"])
        self.assertEqual(["tests.django_app.views.PaymentDeclined: card declined for <email> [request]"], bundle["exemplars"])
        lines = [line for s in INGEST.slices() for line in s["lines"]]
        self.assertIn("[v1/dj1/checkout/0] INFO Charging card for <email> on order <num>", lines)
        self.assertNotIn("pat@acme.test", INGEST.log_deliveries()[0].body.decode().lower())

    def test_the_async_handler_reports_too(self) -> None:
        import devbench

        INGEST.ask_evidence = True
        INGEST.need_logs = ["dj2/async"]
        status, _, _ = http("GET", ASGI_BASE + "/async-checkout", {"x-adt-trace": "v1/dj2/async/0", "X-Demo-User": "a@b.test"})
        self.assertEqual(500, status)
        self.assertTrue(devbench.flush())
        self.assertEqual([{"email": "a@b.test", "account": "acct-1182"}], INGEST.counts()[0]["users"])
        self.assertTrue(INGEST.wait_for(lambda: len(INGEST.evidence()) == 1))
        self.assertEqual("tests.django_app.views.PaymentDeclined at tests.django_app.views.async_checkout",
                         INGEST.evidence()[0].json()["template"])
        self.assertIn("[v1/dj2/async/0] INFO async charge for <email>", [l for s in INGEST.slices() for l in s["lines"]])

    def test_a_404_is_not_a_failure(self) -> None:
        import devbench

        status, _, _ = http("GET", BASE + "/missing")
        self.assertEqual(404, status)
        devbench.flush()
        self.assertEqual([], INGEST.counts())

    def test_a_handled_failure_marks_a_traced_response(self) -> None:
        status, headers, _ = http("GET", BASE + "/handled", {"x-adt-trace": "v1/dj3/h/0"})
        self.assertEqual(200, status)
        self.assertEqual("1", headers.get("x-adt-handled"))
        self.assertEqual("x-request-id, x-adt-handled", headers.get("access-control-expose-headers"))
        _, untraced, _ = http("GET", BASE + "/handled")
        self.assertNotIn("x-adt-handled", untraced)

    def test_the_trace_is_bound_for_the_request_and_forwarded(self) -> None:
        import json

        _, _, body = http("GET", BASE + "/fine", {"x-adt-trace": "v1/dj4/f/2"})
        data = json.loads(body)
        self.assertEqual("v1/dj4/f/2", data["trace"])
        self.assertEqual({"x-adt-trace": "v1/dj4/f/3"}, data["headers"])
        _, _, body = http("GET", BASE + "/fine", {"x-adt-trace": "garbage"})
        self.assertEqual("None", json.loads(body)["trace"], "a malformed header is ignored, not rejected")


_DJANGO_ROUTES = {"fail": "fail/<int:order>", "boom": "boom", "captured": "captured", "handled": "sig-handled",
                  "slow": "slow/<int:ms>", "stream-fail": "stream-fail", "real-slow": "real-slow", "limited": "limited"}


class DjangoWSGIRequestSignalsTest(RequestSignalCases, unittest.TestCase):
    """Capability 2b under Django's WSGI handler: the route is
    resolver_match.route."""

    routes = _DJANGO_ROUTES
    handled_path = "/sig-handled"

    @classmethod
    def setUpClass(cls) -> None:
        cls.base = BASE
        cls.ingest = INGEST

    @classmethod
    def tearDownClass(cls) -> None:
        restore_clocks()

    def setUp(self) -> None:
        self.reset_signals()

    def test_an_unmatched_route_is_star_and_never_the_path(self) -> None:
        self.assertEqual(404, self.get("/fail/not-a-number")[0])
        self.assertEqual([], self.flushed(), "a fast 404 is nothing")

    def test_a_response_that_breaks_the_instrumentation_is_still_answered(self) -> None:
        status, _, body = self.get("/broken", evaluated=False)
        self.assertEqual(503, status)
        self.assertEqual(b"still answered", body)


class DjangoASGIRequestSignalsTest(DjangoWSGIRequestSignalsTest):
    """The same under Django's ASGI handler (sync views in its thread pool,
    async views on the loop), and a route joined across include()."""

    @classmethod
    def setUpClass(cls) -> None:
        cls.base = ASGI_BASE
        cls.ingest = INGEST

    def test_an_async_view_and_an_included_route(self) -> None:
        self.assertEqual(504, self.get("/v2/orders/ab-12/async-fail")[0])
        entry = self.only(self.flushed())
        self.assertEqual(self.fp("failed", "v2/orders/<slug:ref>/async-fail", 504), entry["fp"])
        self.assertEqual([USER], entry["users"])


if __name__ == "__main__":
    unittest.main()
