"""Direct mode over real HTTP: what reaches ingest, and that nothing the
transport does can harm the app."""

from __future__ import annotations

import json
import os
import subprocess
import sys
import textwrap
import threading
import time
import unittest

import devbench
from devbench import _core, context, fingerprint, reporter
from devbench.direct import MAX_BODY_BYTES, MAX_FINGERPRINTS, DirectTransport
from devbench.trace import Trace

from .helpers import ROOT, FakeIngest, dead_port, fresh


class Declined(Exception):
    pass


def raise_declined(message: str) -> Declined:
    try:
        raise Declined(message)
    except Declined as e:
        return e


class DirectTestCase(unittest.TestCase):
    def setUp(self) -> None:
        self.ingest = FakeIngest()
        self.addCleanup(self.ingest.close)
        self.addCleanup(_core.reset)
        fresh(self.ingest.dsn(), service="billing", release="r1", flush_interval=3600)

    def transport(self) -> DirectTransport:
        t = _core.transport()
        assert isinstance(t, DirectTransport)
        return t


class FlushTest(DirectTestCase):
    def test_a_pair_dsn_flushes_with_the_secret_key(self) -> None:
        devbench.capture_exception(raise_declined("card declined"))
        self.assertTrue(devbench.flush())
        flushes = self.ingest.flushes()
        self.assertEqual(1, len(flushes))
        self.assertEqual("adt_server_testsecret", flushes[0].headers["x-adt-key"])
        self.assertTrue(flushes[0].headers["user-agent"].startswith("devbench-python/"))

    def test_phase_one_body_carries_counts_and_nothing_else(self) -> None:
        error = raise_declined("card declined for pat@dealer.com on order 1182")
        with context.scoped(Trace.parse("v1/s1/a1/0")):
            devbench.set_user(email=" Pat@Dealer.com ", account=1182)
            devbench.capture_exception(error, symbol="billing.charge")
        self.assertTrue(devbench.flush())
        body = self.ingest.flushes()[0].json()
        self.assertEqual(devbench.PROTOCOL_VERSION, body["v"])
        self.assertEqual("server", body["source"])
        self.assertEqual("billing", body["service"])
        self.assertEqual("r1", body["release"])
        self.assertTrue(body["sensor_id"].startswith("py-"))
        self.assertEqual(1, len(body["counts"]))
        count = body["counts"][0]
        self.assertEqual({"fp", "n", "first", "last", "kind", "users"}, set(count))
        self.assertEqual(1, count["n"])
        self.assertEqual("error", count["kind"])
        self.assertEqual([{"email": "pat@dealer.com", "account": "1182"}], count["users"])
        # The fingerprint is the shared algorithm's over exactly the
        # sidecar's signal: type, message, frames (innermost first).
        frames = reporter.frames(error.__traceback__, os.getcwd())
        want = fingerprint.compute("error", "server", "billing", "tests.test_direct.Declined", str(error), frames)
        self.assertEqual(want, count["fp"])
        raw = self.ingest.flushes()[0].body.decode()
        self.assertNotIn("card declined", raw, "no message bodies in phase 1")
        self.assertNotIn("v1/s1/a1/0", raw)

    def test_repeats_are_counted_not_resent(self) -> None:
        for _ in range(3):
            devbench.capture_exception(raise_declined("card declined"))
        self.assertEqual([3], list(self.transport().pending().values()))
        self.assertTrue(devbench.flush())
        self.assertEqual([3], [c["n"] for c in self.ingest.counts()])

    def test_an_empty_window_sends_nothing(self) -> None:
        self.assertTrue(devbench.flush())
        devbench.capture_exception(raise_declined("x"))
        self.assertTrue(devbench.flush())
        self.assertTrue(devbench.flush())
        self.assertEqual(1, len(self.ingest.flushes()))

    def test_users_are_normalized_deduplicated_and_capped_at_twenty(self) -> None:
        def site(i: int) -> None:
            with context.scoped(None):
                devbench.set_user(email="User%d@Example.com" % (i % 25), account="acct")
                devbench.report_handled(ValueError("x"), symbol="billing.retry")

        for i in range(60):
            site(i)
        self.assertTrue(devbench.flush())
        count = self.ingest.counts()[0]
        self.assertEqual(60, count["n"])
        self.assertEqual(20, len(count["users"]))
        # Occurrences by users past the twentieth, as the Go and Ruby SDKs
        # count them: users 20-24, twice each.
        self.assertEqual(10, count["users_overflow"])
        self.assertIn({"email": "user0@example.com", "account": "acct"}, count["users"])

    def test_users_do_not_carry_into_the_next_window(self) -> None:
        with context.scoped(None):
            devbench.set_user(email="a@x.com")
            devbench.report_handled(ValueError("x"), symbol="s")
        self.assertTrue(devbench.flush())
        with context.scoped(None):
            devbench.report_handled(ValueError("x"), symbol="s")
        self.assertTrue(devbench.flush())
        second = self.ingest.flushes()[1].json()["counts"][0]
        self.assertNotIn("users", second)

    def test_the_table_is_bounded_and_overflow_is_reported(self) -> None:
        for i in range(MAX_FINGERPRINTS + 7):
            devbench.report_handled(ValueError("x"), symbol="site.n%d" % i)
        self.assertTrue(devbench.flush())
        bodies = [r.json() for r in self.ingest.flushes()]
        self.assertEqual(MAX_FINGERPRINTS, sum(len(b["counts"]) for b in bodies))
        self.assertEqual(7, bodies[0]["overflowed"])

    def test_large_windows_are_split_under_192_kib(self) -> None:
        for i in range(MAX_FINGERPRINTS):
            for u in range(20):
                with context.scoped(None):
                    devbench.set_user(email=("u%d-%s@example.com" % (u, "x" * 200)), account="a" * 200)
                    devbench.report_handled(ValueError("x"), symbol="site.n%d" % i)
        self.assertTrue(devbench.flush(timeout=20))
        flushes = self.ingest.flushes()
        self.assertGreater(len(flushes), 1)
        for r in flushes:
            self.assertLessEqual(len(r.body), MAX_BODY_BYTES)
        self.assertEqual(MAX_FINGERPRINTS, len(self.ingest.counts()))


class EvidenceTest(DirectTestCase):
    def test_evidence_is_uploaded_when_asked_in_the_sidecars_bundle_shape(self) -> None:
        self.ingest.ask_evidence = True
        with context.scoped(Trace.parse("v1/s1/a1/0")):
            devbench.set_user(email="pat@dealer.com", account="dealer-1182")
            devbench.capture_exception(raise_declined("card declined for pat@dealer.com, order 1182"), symbol="billing.charge")
        self.assertTrue(devbench.flush())
        self.assertTrue(self.ingest.wait_for(lambda: len(self.ingest.evidence()) == 1))
        put = self.ingest.evidence()[0]
        fp = self.ingest.counts()[0]["fp"]
        self.assertEqual("/evidence/" + fp, put.path)
        self.assertNotIn("x-adt-key", put.headers, "the presigned URL carries its own authorization")
        bundle = put.json()
        self.assertEqual(devbench.PROTOCOL_VERSION, bundle["v"])
        self.assertEqual(fp, bundle["fp"])
        self.assertEqual("error", bundle["kind"])
        self.assertEqual("billing", bundle["service"])
        self.assertEqual("tests.test_direct.Declined at billing.charge", bundle["template"])
        self.assertEqual(["tests.test_direct.Declined: card declined for <email>, order <num> [explicit, handled]"],
                         bundle["exemplars"])
        self.assertEqual("raise_declined", bundle["frames"][0]["function"])
        self.assertEqual("tests/test_direct.py", bundle["frames"][0]["file"])
        raw = put.body.decode()
        self.assertNotIn("pat@dealer.com", raw)
        self.assertNotIn("dealer-1182", raw, "identity never enters a bundle")

    def test_handled_failure_evidence(self) -> None:
        self.ingest.ask_evidence = True
        devbench.report_handled(ValueError("token=abc123 rejected"), symbol="customers.update", reason="Approved for Alice Smith")
        self.assertTrue(devbench.flush())
        self.assertTrue(self.ingest.wait_for(lambda: len(self.ingest.evidence()) == 1))
        bundle = self.ingest.evidence()[0].json()
        self.assertEqual("handled_failure", bundle["kind"])
        self.assertEqual("customers.update", bundle["template"])
        self.assertEqual(["ValueError: token=abc123 rejected reason=Approved for <redacted:name>"], bundle["exemplars"])
        want = fingerprint.compute("handled_failure", "server", "billing", "ValueError", "customers.update")
        self.assertEqual(want, bundle["fp"], "a handled failure is fingerprinted from its symbol and type only")

    def test_a_later_ask_is_still_answered(self) -> None:
        devbench.capture_exception(raise_declined("x"))
        self.assertTrue(devbench.flush())
        self.ingest.ask_evidence = True
        devbench.capture_exception(raise_declined("x"))
        self.assertTrue(devbench.flush())
        self.assertTrue(self.ingest.wait_for(lambda: len(self.ingest.evidence()) == 1))


class FailureTest(DirectTestCase):
    def test_a_dead_endpoint_never_raises_and_gives_up_quickly(self) -> None:
        fresh("http://k@127.0.0.1:%d" % dead_port(), flush_interval=3600)
        devbench.capture_exception(raise_declined("x"))
        started = time.monotonic()
        self.assertFalse(devbench.flush(timeout=3))
        self.assertLess(time.monotonic() - started, 3.5)
        self.assertEqual({}, _core.transport().pending(), "the batch is discarded, never queued")

    def test_a_server_error_is_retried_once_then_discarded(self) -> None:
        self.ingest.flush_status = 503
        devbench.capture_exception(raise_declined("x"))
        self.assertFalse(devbench.flush())
        self.assertEqual(2, len(self.ingest.flushes()))
        self.ingest.flush_status = 200
        self.assertTrue(devbench.flush())
        self.assertEqual(2, len(self.ingest.flushes()), "nothing was kept for later")

    def test_one_retry_recovers_a_transient_error(self) -> None:
        answers = iter([502, 200])
        self.ingest.flush_status = lambda req: next(answers)
        devbench.capture_exception(raise_declined("x"))
        self.assertTrue(devbench.flush())
        self.assertEqual(2, len(self.ingest.flushes()))

    def test_a_rejected_key_is_not_retried(self) -> None:
        self.ingest.flush_status = 401
        devbench.capture_exception(raise_declined("x"))
        with self.assertLogs("devbench", level="WARNING") as logs:
            self.assertFalse(devbench.flush())
        self.assertEqual(1, len(self.ingest.flushes()))
        self.assertIn("HTTP 401", logs.output[0])
        self.assertNotIn("adt_server_testsecret", "".join(logs.output))

    def test_reporting_does_not_wait_for_a_slow_ingest(self) -> None:
        self.ingest.delay = 2.0
        devbench.capture_exception(raise_declined("first"))
        flusher = threading.Thread(target=devbench.flush, kwargs={"timeout": 5})
        flusher.start()
        time.sleep(0.2)
        started = time.monotonic()
        for i in range(200):
            devbench.report_handled(ValueError("x"), symbol="site.%d" % i)
        self.assertLess(time.monotonic() - started, 0.5, "a report never waits on the network")
        flusher.join()

    def test_capture_never_raises_whatever_the_error_does(self) -> None:
        class Hostile(Exception):
            def __str__(self) -> str:
                raise RuntimeError("no message for you")

        devbench.capture_exception(Hostile())
        devbench.report_handled(Hostile(), symbol="hostile.site")
        devbench.capture_exception("not an exception")  # type: ignore[arg-type]
        self.assertTrue(devbench.flush())
        self.assertEqual(2, len(self.ingest.counts()), "class and site are reported without the message")


class LifecycleTest(unittest.TestCase):
    def setUp(self) -> None:
        self.ingest = FakeIngest()
        self.addCleanup(self.ingest.close)
        self.addCleanup(_core.reset)

    def script(self, body: str) -> subprocess.CompletedProcess:
        env = {k: v for k, v in os.environ.items() if not k.startswith(("DEVBENCH_", "ADT_"))}
        env["DEVBENCH_DSN"] = self.ingest.dsn()
        return subprocess.run([sys.executable, "-c", textwrap.dedent(body)], cwd=ROOT, env=env,
                              capture_output=True, text=True, timeout=60)

    def test_the_exit_flush_sends_what_is_left(self) -> None:
        done = self.script("""
            import devbench
            try:
                raise KeyError("left at exit")
            except KeyError as e:
                devbench.capture_exception(e)
        """)
        self.assertEqual(0, done.returncode, done.stderr)
        self.assertEqual(1, len(self.ingest.counts()), "atexit flushed the last window")

    def test_the_background_thread_flushes_on_its_interval(self) -> None:
        fresh(self.ingest.dsn(), flush_interval=0.2)
        devbench.capture_exception(raise_declined("x"))
        self.assertTrue(self.ingest.wait_for(lambda: len(self.ingest.counts()) == 1, timeout=5))

    @unittest.skipUnless(hasattr(os, "fork"), "needs fork")
    def test_a_forked_child_reports_on_its_own(self) -> None:
        done = self.script("""
            import os, devbench
            from devbench import _core
            devbench.report_handled(ValueError("x"), symbol="parent.site")
            parent_sensor = _core.transport()._sensor_id
            pid = os.fork()
            if pid == 0:
                devbench.report_handled(ValueError("x"), symbol="child.site")
                assert _core.transport()._sensor_id != parent_sensor
                ok = devbench.flush()
                os._exit(0 if ok else 3)
            _, status = os.waitpid(pid, 0)
            assert os.waitstatus_to_exitcode(status) == 0, status
            assert devbench.flush()
        """)
        self.assertEqual(0, done.returncode, done.stderr)
        bodies = [r.json() for r in self.ingest.flushes()]
        self.assertEqual(2, len(bodies))
        self.assertNotEqual(bodies[0]["sensor_id"], bodies[1]["sensor_id"])
        self.assertEqual([1, 1], [len(b["counts"]) for b in bodies], "the child never resends the parent's counts")


if __name__ == "__main__":
    unittest.main()
