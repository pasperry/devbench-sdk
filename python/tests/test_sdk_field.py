"""Every flush request names the SDK that sent it:
``"sdk": {"name": "python", "version": devbench.VERSION}``
(docs/server-sdk/wire-protocol.md, "the SDK's name and version"; #310).
Checked on what actually arrives at FakeIngest's real socket."""

from __future__ import annotations

import logging
import os
import re
import unittest

import devbench
from devbench import _core, context
from devbench.direct import MAX_BODY_BYTES
from devbench.trace import Trace

from .helpers import ROOT, FakeIngest, Request, fresh, vectors


class SdkFieldWireTest(unittest.TestCase):
    def setUp(self) -> None:
        self.ingest = FakeIngest()
        self.addCleanup(self.ingest.close)
        self.addCleanup(_core.reset)
        fresh(self.ingest.dsn(), service="billing", release="r1", flush_interval=3600)

    def assert_sdk(self, flush: Request) -> None:
        body = flush.json()
        self.assertEqual({"name": "python", "version": devbench.VERSION}, body.get("sdk"),
                         "sdk in %s" % flush.body[:300])
        # After sensor_id and before counts, the wire-protocol example's order.
        keys = list(body)
        at = keys.index("sdk")
        self.assertEqual(["sensor_id", "sdk", "counts"], keys[at - 1:at + 2], keys)

    def test_a_counts_flush_carries_it(self) -> None:
        devbench.report_handled(ValueError("x"), symbol="billing.charge")
        self.assertTrue(devbench.flush())
        flushes = self.ingest.flushes()
        self.assertEqual(1, len(flushes))
        self.assert_sdk(flushes[0])
        # Nothing else changes: the keys the body had before #310, plus sdk.
        self.assertEqual(["v", "tenant", "source", "service", "release", "sensor_id", "sdk", "counts"],
                         list(flushes[0].json()))

    def test_every_request_of_a_split_window_carries_it(self) -> None:
        for i in range(60):  # ~8 KB a count: ~500 KB, several requests
            for u in range(20):
                with context.scoped(None):
                    devbench.set_user(email=("u%d-%s@example.com" % (u, "x" * 200)), account="a" * 200)
                    devbench.report_handled(ValueError("x"), symbol="site.n%d" % i)
        self.assertTrue(devbench.flush(timeout=20))
        flushes = self.ingest.flushes()
        self.assertGreater(len(flushes), 1, "expected the window to be split")
        for r in flushes:
            self.assert_sdk(r)
            self.assertLessEqual(len(r.body), MAX_BODY_BYTES)
        self.assertEqual(60, len(self.ingest.counts()))

    def test_the_empty_poll_carries_it(self) -> None:
        self.assertTrue(devbench.capture_logs())
        logger = logging.getLogger("tests.sdk_field")
        logger.setLevel(logging.INFO)
        self.addCleanup(logger.setLevel, logging.NOTSET)
        with context.scoped(Trace.parse("v1/sessS/actS/0")):
            logger.info("a quiet line")
        self.assertTrue(devbench.flush())
        flushes = self.ingest.flushes()
        self.assertEqual(1, len(flushes), "a process holding lines polls once")
        self.assertEqual([], flushes[0].json()["counts"])
        self.assert_sdk(flushes[0])


class SdkFieldValuesTest(unittest.TestCase):
    """The name is the one the shared vectors give this SDK's directory, both
    values pass ingest's patterns (else ingest drops the field), and the
    version is the package's own, not a second literal."""

    def test_the_name_is_the_vectors_and_both_values_pass_ingests_patterns(self) -> None:
        sdk = vectors("protocol_vectors.json", self)["sdk"]
        mine = [name for name, d in sdk["names"].items() if d == "sdk/server/python"]
        self.assertEqual([devbench.SDK_NAME], mine)
        # Whole-string matches (the spec: re.fullmatch, never a $ that
        # matches before a final newline).
        self.assertIsNotNone(re.fullmatch(sdk["name_pattern"].strip("^$"), devbench.SDK_NAME))
        self.assertIsNotNone(re.fullmatch(sdk["version_pattern"].strip("^$"), devbench.VERSION))

    def test_the_version_is_the_packages_version(self) -> None:
        # pyproject.toml takes the package version from devbench.version.VERSION,
        # so the version on the wire is the one pip installs and the release tags.
        with open(os.path.join(ROOT, "pyproject.toml"), encoding="utf-8") as f:
            pyproject = f.read()
        self.assertRegex(pyproject, r'(?m)^dynamic = \["version"\]$')
        self.assertRegex(pyproject, r'(?m)^version = \{ attr = "devbench\.version\.VERSION" \}$')
        self.assertNotRegex(pyproject, r'(?m)^version = "')


if __name__ == "__main__":
    unittest.main()
