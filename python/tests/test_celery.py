"""Celery, with a real worker consuming real task messages (Celery's
in-thread test worker on the in-memory transport): the trace travels in the
message from the request that enqueued the task, a failed task is reported as
a job, and its log lines are held under that trace."""

from __future__ import annotations

import logging
import sys
import unittest

import devbench
from devbench import _core, context
from devbench.trace import Trace

from .helpers import FakeIngest, fresh, require

log = logging.getLogger("acme.jobs")


class ChargeFailed(Exception):
    pass


class CeleryTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        require("celery", unittest.TestCase())
        from celery import Celery
        from celery.contrib.testing.worker import start_worker

        import devbench.celery

        cls.ingest = FakeIngest()
        fresh(cls.ingest.dsn(), flush_interval=3600)
        logging.getLogger("acme").setLevel(logging.INFO)
        devbench.celery.install()
        devbench.celery.install()

        app = Celery("acme_jobs", broker="memory://", backend="cache+memory://")
        app.conf.task_always_eager = False

        @app.task(name="acme.charge")
        def charge(order: int) -> None:
            log.info("charging order %s in a job", order)
            raise ChargeFailed("declined for order %s" % order)

        @app.task(name="acme.seen")
        def seen() -> "object":
            return [str(devbench.current_trace()), context.current_user()]

        @app.task(name="acme.sets_user")
        def sets_user() -> None:
            devbench.set_user(email="job@x.test")

        cls.app = app
        cls.charge = charge
        cls.seen = seen
        cls.sets_user = sets_user
        cls.worker = start_worker(app, perform_ping_check=False, pool="solo")
        cls.worker.__enter__()

    @classmethod
    def tearDownClass(cls) -> None:
        cls.worker.__exit__(None, None, None)
        cls.ingest.close()
        _core.reset()

    def setUp(self) -> None:
        devbench.flush()
        with self.ingest.lock:
            self.ingest.requests.clear()
        self.ingest.ask_evidence = True
        self.ingest.need_logs = []

    def test_a_failed_task_is_a_job_failure_under_the_enqueuing_trace(self) -> None:
        self.ingest.need_logs = ["web1/charge"]
        with context.scoped(Trace.parse("v1/web1/charge/0")):
            result = self.charge.delay(1182)
        with self.assertRaises(ChargeFailed):
            result.get(timeout=10)
        self.assertTrue(devbench.flush())
        self.assertEqual(1, len(self.ingest.counts()))
        self.assertTrue(self.ingest.wait_for(lambda: len(self.ingest.evidence()) == 1))
        bundle = self.ingest.evidence()[0].json()
        self.assertEqual("tests.test_celery.ChargeFailed at acme.charge", bundle["template"])
        self.assertEqual(["tests.test_celery.ChargeFailed: declined for order <num> [job]"], bundle["exemplars"])
        lines = [line for s in self.ingest.slices() for line in s["lines"]]
        self.assertEqual("[v1/web1/charge/0] INFO charging order <num> in a job", lines[0])
        # Celery's own failure line (and its traceback) is the job's too.
        self.assertIn("[v1/web1/charge/0] ERROR Task acme.charge[<uuid>] raised unexpected: ChargeFailed(<str>)", lines)

    def test_a_task_runs_in_its_own_scope(self) -> None:
        with context.scoped(Trace.parse("v1/web2/seen/1")):
            devbench.set_user(email="web@x.test")
            traced = self.seen.delay().get(timeout=10)
        self.assertEqual(["v1/web2/seen/1", None], traced, "the trace travels; the web user does not")
        self.sets_user.delay().get(timeout=10)
        self.assertEqual(["None", None], self.seen.delay().get(timeout=10), "identity set in a task ends with it")

    def test_a_worker_command_line_is_a_worker(self) -> None:
        import devbench.celery as dc

        saved = sys.argv
        try:
            sys.argv = ["/venv/bin/celery", "-A", "proj", "worker", "-l", "info"]
            self.assertTrue(dc._in_worker())
            sys.argv = ["/venv/bin/celery", "-A", "proj", "beat"]
            self.assertFalse(dc._in_worker())
            sys.argv = ["gunicorn", "proj.wsgi"]
            self.assertFalse(dc._in_worker())
        finally:
            sys.argv = saved


if __name__ == "__main__":
    unittest.main()
