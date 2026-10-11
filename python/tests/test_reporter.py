"""The reports themselves: what an exception becomes, once, and never in the
app's way."""

from __future__ import annotations

import asyncio
import os
import unittest

import devbench
from devbench import _core, context, reporter

from .helpers import FakeIngest, fresh


def deep(n: int) -> None:
    if n == 0:
        raise ValueError("bottom")
    deep(n - 1)


def caught(fn, *args):  # type: ignore[no-untyped-def]
    try:
        fn(*args)
    except Exception as e:  # noqa: BLE001
        return e
    raise AssertionError("did not raise")


class PayloadTest(unittest.TestCase):
    def tearDown(self) -> None:
        _core.reset()

    def test_an_exception_report_is_the_sidecars_message(self) -> None:
        fresh()
        error = caught(deep, 0)
        with context.scoped(devbench.Trace.parse("v1/s/a/3")):
            devbench.set_user(email="Pat@X.com", account="a1")
            p = reporter.exception_payload(error, "request", False, "shop.views.checkout")
        self.assertEqual(devbench.PROTOCOL_VERSION, p["v"])
        self.assertEqual("exception", p["kind"])
        self.assertEqual("request", p["context"])
        self.assertFalse(p["handled"])
        self.assertEqual("ValueError", p["error"])
        self.assertEqual("bottom", p["message"])
        self.assertEqual("shop.views.checkout", p["symbol"])
        self.assertEqual("v1/s/a/3", p["trace"])
        self.assertEqual({"email": "pat@x.com", "account": "a1"}, p["user"])
        self.assertEqual({"function": "deep", "file": "tests/test_reporter.py"},
                         {k: p["frames"][0][k] for k in ("function", "file")})

    def test_class_names_are_module_qualified_except_builtins(self) -> None:
        class Local(Exception):
            pass

        self.assertEqual("KeyError", reporter.class_name(KeyError))
        self.assertEqual("tests.test_reporter.PayloadTest.test_class_names_are_module_qualified_except_builtins.<locals>.Local",
                         reporter.class_name(Local))

    def test_frames_are_innermost_first_and_at_most_50(self) -> None:
        error = caught(deep, 80)
        frames = reporter.frames(error.__traceback__, os.getcwd())
        self.assertEqual(50, len(frames))
        self.assertEqual("deep", frames[0]["function"])
        self.assertTrue(all(f["line"] > 0 for f in frames))

    def test_files_outside_the_root_stay_absolute_and_a_prefix_is_not_a_parent(self) -> None:
        error = caught(deep, 0)
        here = os.path.dirname(os.path.abspath(__file__))
        self.assertEqual("test_reporter.py", reporter.frames(error.__traceback__, here)[0]["file"])
        self.assertTrue(reporter.frames(error.__traceback__, here[:-1])[0]["file"].startswith("/"))

    def test_message_is_at_most_2000_characters(self) -> None:
        p = reporter.exception_payload(ValueError("x" * 5000), "explicit", True, None)
        self.assertEqual(2000, len(p["message"]))
        self.assertTrue(p["message"].endswith("…"))

    def test_symbol_may_be_empty_and_user_is_omitted(self) -> None:
        with context.scoped(None):
            p = reporter.exception_payload(ValueError("x"), "explicit", True, None)
        self.assertEqual("", p["symbol"])
        self.assertNotIn("user", p)
        self.assertNotIn("trace", p)

    def test_an_unknown_context_is_explicit(self) -> None:
        self.assertEqual("explicit", reporter.exception_payload(ValueError(), "nonsense", True, None)["context"])


class OnceAndIgnoredTest(unittest.TestCase):
    def setUp(self) -> None:
        self.ingest = FakeIngest()
        self.addCleanup(self.ingest.close)
        self.addCleanup(_core.reset)
        fresh(self.ingest.dsn(), flush_interval=3600)

    def sent(self) -> int:
        devbench.flush()
        return sum(c["n"] for c in self.ingest.counts())

    def test_the_same_exception_is_reported_once(self) -> None:
        error = caught(deep, 0)
        reporter.capture(error, "request", False, None)
        reporter.capture(error, "job", False, None)
        devbench.capture_exception(error)
        self.assertEqual(1, self.sent())

    def test_an_exception_that_refuses_attributes_is_still_reported_once(self) -> None:
        class Slotted(Exception):
            def __setattr__(self, name: str, value: object) -> None:
                raise AttributeError("read-only")

        error = Slotted("x")
        devbench.capture_exception(error)
        devbench.capture_exception(error)
        self.assertEqual(1, self.sent())

    def test_distinct_exceptions_are_each_reported(self) -> None:
        devbench.capture_exception(caught(deep, 0))
        devbench.capture_exception(caught(deep, 0))
        self.assertEqual(2, self.sent())

    def test_the_skip_list_matches_class_and_bases_by_name_and_is_extendable(self) -> None:
        class NotAuthorized(Exception):
            pass

        class Sub(NotAuthorized):
            pass

        devbench.ignore_exceptions().append(reporter.class_name(NotAuthorized))
        self.addCleanup(devbench.ignore_exceptions().remove, reporter.class_name(NotAuthorized))
        devbench.capture_exception(Sub())
        self.assertEqual(0, self.sent())

    def test_process_control_is_not_reported(self) -> None:
        for error in (SystemExit(1), KeyboardInterrupt(), GeneratorExit(), asyncio.CancelledError()):
            devbench.capture_exception(error)
        self.assertEqual(0, self.sent())

    def test_report_handled_counts_even_when_reporting_is_broken(self) -> None:
        fresh("http://broken dsn")
        with context.scoped(devbench.Trace.parse("v1/s/a/0")) as scope:
            devbench.report_handled(ValueError("x"), symbol="s")
            devbench.report_handled(ValueError("x"), symbol="s")
            self.assertEqual(2, scope.handled)

    def test_set_user_never_raises_and_truncates(self) -> None:
        class Bad:
            def __str__(self) -> str:
                raise RuntimeError("no")

        with context.scoped(None):
            devbench.set_user(email=Bad(), account="a" * 300)
            self.assertEqual({"account": "a" * 255}, context.current_user())
            devbench.set_user()
            self.assertIsNone(context.current_user())

    def test_trace_headers_forward_with_the_hop_incremented(self) -> None:
        self.assertEqual({}, devbench.trace_headers())
        with context.scoped(devbench.Trace.parse("v1/s/a/4")):
            self.assertEqual({"x-adt-trace": "v1/s/a/5"}, devbench.trace_headers())
        with context.scoped(devbench.Trace.parse("v1/s/a/99")):
            self.assertEqual({}, devbench.trace_headers())


if __name__ == "__main__":
    unittest.main()
