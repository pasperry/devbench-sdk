"""In-process log capture and delivery: the buffer's bounds, the logging
handler, and need_logs answered over real HTTP."""

from __future__ import annotations

import logging
import os
import subprocess
import sys
import textwrap
import threading
import unittest

import devbench
from devbench import _core, context, logs
from devbench.trace import Trace

from .helpers import ROOT, FakeIngest, fresh


class Clock:
    def __init__(self) -> None:
        self.now = 1000.0

    def __call__(self) -> float:
        return self.now


class BufferTest(unittest.TestCase):
    def test_lines_are_kept_per_key_in_order(self) -> None:
        b = logs.Buffer()
        b.push("s/a", "one")
        b.push("s/b", "other")
        b.push("s/a", "two")
        self.assertEqual(["one", "two"], b.lookup("s/a", 200))
        self.assertEqual(["other"], b.lookup("s/b", 200))
        self.assertEqual([], b.lookup("s/none", 200))

    def test_lookup_returns_the_most_recent_lines_up_to_the_limit(self) -> None:
        b = logs.Buffer()
        for i in range(10):
            b.push("k", str(i))
        self.assertEqual(["7", "8", "9"], b.lookup("k", 3))

    def test_the_line_count_bound_evicts_oldest_first(self) -> None:
        b = logs.Buffer(max_lines=3)
        for i in range(5):
            b.push("k%d" % (i % 2), str(i))
        self.assertEqual(3, len(b))
        self.assertEqual(["2", "4"], b.lookup("k0", 10))
        self.assertEqual(["3"], b.lookup("k1", 10))

    def test_the_byte_bound_evicts_oldest_first(self) -> None:
        b = logs.Buffer(max_bytes=10)
        b.push("k", "aaaa")
        b.push("k", "bbbb")
        b.push("k", "cccc")
        self.assertEqual(["bbbb", "cccc"], b.lookup("k", 10))
        self.assertLessEqual(b.bytes, 10)

    def test_nothing_older_than_the_age_bound_is_kept(self) -> None:
        clock = Clock()
        b = logs.Buffer(max_age=60, clock=clock)
        b.push("k", "old")
        clock.now += 30
        b.push("k", "new")
        clock.now += 31
        self.assertEqual(["new"], b.lookup("k", 10))
        self.assertTrue(b.any())
        clock.now += 60
        self.assertFalse(b.any())

    def test_the_default_bounds_are_the_specs(self) -> None:
        b = logs.Buffer()
        self.assertEqual((10_000, 4 * 1024 * 1024, 15 * 60), (b.max_lines, b.max_bytes, b.max_age))
        self.assertEqual(4096, logs.MAX_LINE_BYTES)

    def test_many_threads_push_without_losing_or_mixing_lines(self) -> None:
        b = logs.Buffer()

        def worker(n: int) -> None:
            for i in range(500):
                b.push("k%d" % n, "%d-%d" % (n, i))

        threads = [threading.Thread(target=worker, args=(n,)) for n in range(8)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        self.assertEqual(4000, len(b))
        for n in range(8):
            self.assertEqual(["%d-%d" % (n, i) for i in range(200, 500)], b.lookup("k%d" % n, 300))


class HandlerTest(unittest.TestCase):
    def setUp(self) -> None:
        self.ingest = FakeIngest()
        self.addCleanup(self.ingest.close)
        self.addCleanup(_core.reset)
        fresh(self.ingest.dsn(), flush_interval=3600)
        self.assertTrue(devbench.capture_logs())
        self.logger = logging.getLogger("tests.app")
        self.logger.setLevel(logging.INFO)
        self.addCleanup(self.logger.setLevel, logging.NOTSET)

    def held(self, key: str) -> list:
        return logs.lookup(key, 1000)

    def test_installing_twice_hooks_once(self) -> None:
        devbench.capture_logs()
        devbench.capture_logs()
        self.assertEqual(1, sum(isinstance(h, logs.LogHandler) for h in logging.getLogger().handlers))

    def test_traced_lines_are_kept_under_the_trace_and_untraced_are_not(self) -> None:
        self.logger.info("before any request")
        with context.scoped(Trace.parse("v1/sessA/act1/2")):
            self.logger.info("charging order %s", 1182)
        with context.scoped(None):
            self.logger.info("untraced request")
        self.assertEqual(["[v1/sessA/act1/2] INFO charging order 1182"], self.held("sessA/act1"))
        self.assertEqual(1, len(logs.buffer()))

    def test_every_hop_of_an_action_is_under_one_key(self) -> None:
        for hop in range(3):
            with context.scoped(Trace.parse("v1/s/a/%d" % hop)):
                self.logger.warning("hop %d", hop)
        self.assertEqual(3, len(self.held("s/a")))

    def test_the_apps_level_is_respected(self) -> None:
        with context.scoped(Trace.parse("v1/s/a/0")):
            self.logger.debug("not enabled by the app")
            self.logger.info("enabled")
        self.assertEqual(["[v1/s/a/0] INFO enabled"], self.held("s/a"))

    def test_the_apps_output_is_unchanged(self) -> None:
        with self.assertLogs(self.logger, level="INFO") as seen:
            with context.scoped(Trace.parse("v1/s/a/0")):
                self.logger.info("hello %s", "world")
        self.assertEqual(["INFO:tests.app:hello world"], seen.output)

    def test_a_long_line_is_cut_to_four_kib_of_valid_utf8(self) -> None:
        with context.scoped(Trace.parse("v1/s/a/0")):
            self.logger.info("é" * 5000)
        line = self.held("s/a")[0]
        self.assertLessEqual(len(line.encode("utf-8")), 4096)
        self.assertGreater(len(line.encode("utf-8")), 4090)

    def test_an_exceptions_traceback_is_held_one_line_each(self) -> None:
        with context.scoped(Trace.parse("v1/s/a/0")):
            try:
                raise KeyError("missing")
            except KeyError:
                self.logger.exception("lookup failed")
        held = self.held("s/a")
        self.assertEqual("[v1/s/a/0] ERROR lookup failed", held[0])
        self.assertEqual("[v1/s/a/0] ERROR Traceback (most recent call last):", held[1])
        self.assertEqual("[v1/s/a/0] ERROR KeyError: 'missing'", held[-1])
        self.assertTrue(all("\n" not in line for line in held))

    def test_a_long_multi_line_message_keeps_its_top(self) -> None:
        with context.scoped(Trace.parse("v1/s/a/0")):
            self.logger.info("\n".join("line %d" % i for i in range(120)))
        held = self.held("s/a")
        self.assertEqual(logs.MAX_MESSAGE_LINES, len(held))
        self.assertEqual("[v1/s/a/0] INFO line 0", held[0])
        self.assertEqual("[v1/s/a/0] INFO ... (71 more lines)", held[-1])

    def test_nothing_is_kept_while_capture_is_off(self) -> None:
        logs.deactivate()
        with context.scoped(Trace.parse("v1/s/a/0")):
            self.logger.info("off")
        self.assertEqual(0, len(logs.buffer()))

    def test_a_fault_in_capture_never_raises_into_the_logging_call(self) -> None:
        class Broken(logs.Buffer):
            def push(self, key: str, text: str) -> None:
                raise RuntimeError("buffer exploded")

        logs.set_buffer(Broken())
        with context.scoped(Trace.parse("v1/s/a/0")):
            self.logger.info("still fine")
            self.logger.info("%d", "not a number")  # the app's own bad call

    def test_a_record_from_another_thread_pool_keeps_its_trace(self) -> None:
        # contextvars.copy_context(): what a FastAPI sync endpoint runs in.
        import contextvars

        with context.scoped(Trace.parse("v1/s/a/0")):
            ctx = contextvars.copy_context()
        t = threading.Thread(target=ctx.run, args=(self.logger.info, "from the pool"))
        t.start()
        t.join()
        self.assertEqual(["[v1/s/a/0] INFO from the pool"], self.held("s/a"))


class DeliveryTest(unittest.TestCase):
    def setUp(self) -> None:
        self.ingest = FakeIngest()
        self.addCleanup(self.ingest.close)
        self.addCleanup(_core.reset)
        fresh(self.ingest.dsn(), service="billing", flush_interval=3600)
        devbench.capture_logs()
        self.logger = logging.getLogger("tests.delivery")
        self.logger.setLevel(logging.INFO)
        self.addCleanup(self.logger.setLevel, logging.NOTSET)

    def log(self, trace: str, message: str) -> None:
        with context.scoped(Trace.parse(trace)):
            self.logger.info(message)

    def test_requested_lines_are_delivered_redacted_with_the_secret_key(self) -> None:
        self.log("v1/sessA/act1/0", "charging card for pat@dealer.com on order 1182")
        self.log("v1/sessB/act1/0", "another action")
        self.ingest.need_logs = ["sessA/act1"]
        devbench.report_handled(ValueError("x"), symbol="s")
        self.assertTrue(devbench.flush())
        self.assertTrue(self.ingest.wait_for(lambda: len(self.ingest.log_deliveries()) == 1))
        delivery = self.ingest.log_deliveries()[0]
        self.assertEqual("adt_server_testsecret", delivery.headers["x-adt-key"])
        self.assertEqual([{"trace": "sessA/act1",
                           "lines": ["[v1/sessA/act1/0] INFO charging card for <email> on order <num>"]}],
                         delivery.json()["slices"])

    def test_nothing_is_sent_for_a_key_with_no_lines(self) -> None:
        self.log("v1/sessA/act1/0", "held")
        self.ingest.need_logs = ["sessA/act1", "other/key"]
        devbench.report_handled(ValueError("x"), symbol="s")
        self.assertTrue(devbench.flush())
        self.assertEqual(["sessA/act1"], [s["trace"] for s in self.ingest.slices()])

    def test_no_post_at_all_when_no_requested_key_has_lines(self) -> None:
        self.log("v1/sessA/act1/0", "held")
        self.ingest.need_logs = ["other/key"]
        devbench.report_handled(ValueError("x"), symbol="s")
        self.assertTrue(devbench.flush())
        self.assertEqual([], self.ingest.log_deliveries())

    def test_learned_rules_on_the_same_response_apply(self) -> None:
        self.log("v1/sessA/act1/0", "escalated to Whitfield license=D123")
        self.ingest.need_logs = ["sessA/act1"]
        self.ingest.redaction = [{"kind": "term", "target": "Whitfield"}, {"kind": "field", "target": "license"}]
        devbench.report_handled(ValueError("x"), symbol="s")
        self.assertTrue(devbench.flush())
        self.assertEqual(["[v1/sessA/act1/0] INFO escalated to <redacted:term> license=<redacted:field>"],
                         self.ingest.slices()[0]["lines"])

    def test_an_absent_rule_set_keeps_rules_and_an_empty_one_withdraws_them(self) -> None:
        self.log("v1/sessA/act1/0", "escalated to Whitfield")
        self.ingest.need_logs = ["sessA/act1"]
        self.ingest.redaction = [{"kind": "term", "target": "Whitfield"}]
        devbench.flush()
        self.ingest.redaction = None
        devbench.flush()
        self.ingest.redaction = []
        devbench.flush()
        lines = [s["lines"][0] for s in self.ingest.slices()]
        self.assertEqual(["[v1/sessA/act1/0] INFO escalated to <redacted:term>"] * 2
                         + ["[v1/sessA/act1/0] INFO escalated to Whitfield"], lines)

    def test_lines_are_kept_after_delivery(self) -> None:
        self.log("v1/sessA/act1/0", "held")
        self.ingest.need_logs = ["sessA/act1"]
        devbench.flush()
        devbench.flush()
        self.assertEqual(2, len(self.ingest.slices()))

    def test_a_slice_is_bounded_to_the_most_recent_lines(self) -> None:
        for i in range(250):
            self.log("v1/sessA/act1/0", "line %d" % i)
        self.ingest.need_logs = ["sessA/act1"]
        devbench.flush()
        lines = self.ingest.slices()[0]["lines"]
        self.assertEqual(200, len(lines))
        self.assertTrue(lines[-1].endswith("line <num>"))

    def test_a_slice_is_bounded_to_64_kib(self) -> None:
        for _ in range(100):
            self.log("v1/sessA/act1/0", "x" * 1000)
        self.ingest.need_logs = ["sessA/act1"]
        devbench.flush()
        lines = self.ingest.slices()[0]["lines"]
        self.assertLessEqual(sum(len(line.encode()) for line in lines), 64 * 1024)
        self.assertGreater(len(lines), 50)

    def test_many_slices_are_split_across_requests(self) -> None:
        keys = ["s%d/a" % i for i in range(30)]
        for key in keys:
            self.log("v1/%s/0" % key, "held")
        self.ingest.need_logs = keys
        devbench.flush()
        deliveries = self.ingest.log_deliveries()
        self.assertEqual([25, 5], [len(d.json()["slices"]) for d in deliveries])

    def test_a_quiet_process_polls_and_answers(self) -> None:
        # Nothing counted: the background thread still sends one empty flush
        # per interval while lines are held, and need_logs rides only on it.
        fresh(self.ingest.dsn(), flush_interval=0.2)
        devbench.capture_logs()
        self.ingest.need_logs = ["sessQ/act1"]
        self.ingest.need_logs_only_on_poll = True
        self.log("v1/sessQ/act1/0", "quiet worker line")
        self.assertTrue(self.ingest.wait_for(lambda: len(self.ingest.slices()) >= 1, timeout=5))
        poll = self.ingest.flushes()[0].json()
        self.assertEqual([], poll["counts"])
        self.assertEqual(["[v1/sessQ/act1/0] INFO quiet worker line"], self.ingest.slices()[0]["lines"])

    def test_a_process_holding_no_lines_does_not_poll(self) -> None:
        self.assertTrue(devbench.flush())
        self.assertEqual([], self.ingest.flushes())

    def test_lines_that_aged_out_stop_the_poll(self) -> None:
        clock = Clock()
        logs.set_buffer(logs.Buffer(clock=clock))
        self.log("v1/s/a/0", "held")
        devbench.flush()
        clock.now += 16 * 60
        devbench.flush()
        self.assertEqual(1, len(self.ingest.flushes()))

    def test_a_refused_delivery_never_raises(self) -> None:
        self.ingest.logs_status = 403
        self.log("v1/sessA/act1/0", "held")
        self.ingest.need_logs = ["sessA/act1"]
        with self.assertLogs("devbench", level="WARNING"):
            self.assertTrue(devbench.flush())

    @unittest.skipUnless(hasattr(os, "fork"), "needs fork")
    def test_a_forked_child_answers_only_for_its_own_lines(self) -> None:
        env = {k: v for k, v in os.environ.items() if not k.startswith(("DEVBENCH_", "ADT_"))}
        env["DEVBENCH_DSN"] = self.ingest.dsn()
        self.ingest.need_logs = ["parent/a", "child/a"]
        body = textwrap.dedent("""
            import logging, os, devbench
            from devbench import context
            from devbench.trace import Trace
            logging.getLogger().setLevel(logging.INFO)
            devbench.capture_logs()
            with context.scoped(Trace.parse("v1/parent/a/0")):
                logging.getLogger("x").info("parent line")
            pid = os.fork()
            if pid == 0:
                with context.scoped(Trace.parse("v1/child/a/0")):
                    logging.getLogger("x").info("child line")
                os._exit(0 if devbench.flush() else 3)
            os.waitpid(pid, 0)
            devbench.flush()
        """)
        done = subprocess.run([sys.executable, "-c", body], cwd=ROOT, env=env, capture_output=True, text=True, timeout=60)
        self.assertEqual(0, done.returncode, done.stderr)
        # The parent answers on its flush and again on its exit flush; the
        # child (one flush) answers for its own line only, never the
        # parent's it inherited.
        slices = [(s["trace"], tuple(s["lines"])) for s in self.ingest.slices()]
        self.assertEqual({("child/a", ("[v1/child/a/0] INFO child line",)),
                          ("parent/a", ("[v1/parent/a/0] INFO parent line",))}, set(slices))
        self.assertEqual(1, slices.count(("child/a", ("[v1/child/a/0] INFO child line",))))


if __name__ == "__main__":
    unittest.main()
