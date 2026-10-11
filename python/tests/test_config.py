"""DSN parsing, configuration and transport selection."""

from __future__ import annotations

import io
import logging
import os
import tempfile
import unittest

import devbench
from devbench import _core
from devbench.config import DSN, RELEASE_FALLBACKS, Configuration, InvalidDSN
from devbench.direct import DirectTransport

from .helpers import FakeIngest, fresh


class DSNTest(unittest.TestCase):
    def test_https_dsn_gives_the_base_and_the_key(self) -> None:
        dsn = DSN.parse("https://adt_server_abc@ingest.example.com")
        self.assertEqual("https://ingest.example.com", dsn.base)
        self.assertEqual("adt_server_abc", dsn.key)
        self.assertEqual("https://ingest.example.com/v1/flush", dsn.flush_url)
        self.assertEqual("https://ingest.example.com/v1/logs", dsn.logs_url)
        self.assertEqual("https://ingest.example.com/v1/check", dsn.check_url)

    def test_a_port_is_kept_and_a_path_or_query_ignored(self) -> None:
        self.assertEqual("https://ingest.example.com:8443", DSN.parse("https://k@ingest.example.com:8443/x/y?z=1").base)

    def test_the_default_port_is_not_spelled_out(self) -> None:
        self.assertEqual("https://ingest.example.com", DSN.parse("https://k@ingest.example.com:443").base)

    def test_plain_http_is_accepted_only_for_loopback(self) -> None:
        for host in ("localhost", "127.0.0.1", "[::1]", "app.localhost"):
            self.assertTrue(DSN.parse("http://k@%s:9000" % host).base.startswith("http://"), host)
        with self.assertRaises(InvalidDSN) as caught:
            DSN.parse("http://k@ingest.example.com")
        self.assertIn("plaintext", str(caught.exception))

    def test_invalid_dsns_say_what_is_wrong_and_never_echo_the_key(self) -> None:
        cases = {
            "": "is empty",
            "   ": "is empty",
            "ftp://adt_server_SECRET@host": "must start with https://",
            "https://": "has no host",
            "https://ingest.example.com": "has no key",
            "https://adt_server_SECRET@host:notaport": "is not a URL",
        }
        for raw, want in cases.items():
            with self.assertRaises(InvalidDSN, msg=raw) as caught:
                DSN.parse(raw)
            self.assertIn(want, str(caught.exception), raw)
            self.assertNotIn("SECRET", str(caught.exception), raw)

    def test_the_keys_are_not_in_str_or_repr(self) -> None:
        dsn = DSN.parse("https://adt_client_pub:adt_server_SECRET@ingest.example.com")
        for text in (str(dsn), repr(dsn), "%s" % dsn):
            self.assertNotIn("SECRET", text)
            self.assertNotIn("adt_client_pub", text)

    def test_a_pair_dsn_authenticates_with_the_secret_and_offers_the_public_part(self) -> None:
        dsn = DSN.parse("https://adt_client_pub:adt_server_sec@ingest.example.com:8443")
        self.assertEqual("adt_server_sec", dsn.key)
        self.assertEqual("adt_client_pub", dsn.public_key)
        self.assertEqual("https://adt_client_pub@ingest.example.com:8443", dsn.browser_dsn)

    def test_a_single_key_dsn_has_no_browser_part(self) -> None:
        dsn = DSN.parse("https://adt_server_only@ingest.example.com")
        self.assertEqual("adt_server_only", dsn.key)
        self.assertIsNone(dsn.browser_dsn)

    def test_a_secret_with_no_public_part_has_no_browser_part(self) -> None:
        dsn = DSN.parse("https://:adt_server_sec@ingest.example.com")
        self.assertEqual("adt_server_sec", dsn.key)
        self.assertIsNone(dsn.browser_dsn)


class ConfigurationTest(unittest.TestCase):
    def test_dsn_falls_back_to_adt_dsn(self) -> None:
        self.assertEqual("https://a@b", Configuration({"ADT_DSN": "https://a@b"}).dsn)
        self.assertEqual("https://new@b", Configuration({"ADT_DSN": "https://a@b", "DEVBENCH_DSN": "https://new@b"}).dsn)

    def test_service_defaults_to_app_then_the_framework_then_explicit(self) -> None:
        cfg = Configuration({})
        self.assertEqual("app", cfg.resolved_service())
        cfg.framework_service = "acme_shop"
        self.assertEqual("acme_shop", cfg.resolved_service())
        cfg.worker = "celery"
        self.assertEqual("acme_shop-celery", cfg.resolved_service())
        self.assertEqual("billing", Configuration({"DEVBENCH_SERVICE": "billing"}).resolved_service())
        explicit = Configuration({"DEVBENCH_SERVICE": "billing"})
        explicit.worker = "celery"
        self.assertEqual("billing", explicit.resolved_service(), "an explicit service wins in a worker too")

    def test_release_precedence_through_every_source(self) -> None:
        # The order of the Ruby and Go SDKs (#23): explicit, DEVBENCH_RELEASE,
        # REVISION, then the deploy variables in order, else "".
        with tempfile.TemporaryDirectory() as root:
            env = {name: "from-" + name for name in RELEASE_FALLBACKS}
            with open(os.path.join(root, "REVISION"), "w") as f:
                f.write("cap123\nignored\n")

            def release(e: dict, revision: bool = True) -> str:
                cfg = Configuration(e)
                cfg.app_root = root if revision else os.path.join(root, "nowhere")
                return cfg.resolved_release()

            self.assertEqual("explicit", release(dict(env, DEVBENCH_RELEASE="explicit")))
            self.assertEqual("cap123", release(env))
            remaining = dict(env)
            for name in RELEASE_FALLBACKS:
                self.assertEqual("from-" + name, release(remaining, revision=False), name)
                del remaining[name]
            self.assertEqual("", release(remaining, revision=False))

    def test_release_from_configure_beats_everything(self) -> None:
        fresh()
        devbench.configure(release="code-1")
        self.assertEqual("code-1", _core.config().resolved_release())
        _core.reset()

    def test_enabled_unless_explicitly_false(self) -> None:
        for value in ("false", "0", "no", "off", " FALSE "):
            self.assertFalse(Configuration({"DEVBENCH_ENABLED": value}).enabled, value)
        for value in ("", "true", "1", "yes"):
            self.assertTrue(Configuration({"DEVBENCH_ENABLED": value}).enabled, value)


class TransportSelectionTest(unittest.TestCase):
    def tearDown(self) -> None:
        _core.reset()

    def test_no_dsn_reports_nothing(self) -> None:
        fresh()
        os.environ.pop("DEVBENCH_DSN", None)
        os.environ.pop("ADT_DSN", None)
        self.assertIs(_core.NULL, _core.transport())
        self.assertFalse(_core.direct())

    def test_disabled_is_no_transport(self) -> None:
        fresh("https://k@ingest.example.com", enabled=False)
        self.assertIs(_core.NULL, _core.transport())

    def test_a_broken_dsn_warns_once_and_disables_reporting(self) -> None:
        fresh()
        logger = logging.getLogger("devbench")
        with self.assertLogs(logger, level="WARNING") as logs:
            devbench.configure(dsn="http://adt_server_SECRET@ingest.example.com")
            self.assertIs(_core.NULL, _core.transport())
            devbench.configure(dsn="http://adt_server_SECRET@ingest.example.com")
            self.assertIs(_core.NULL, _core.transport())
            devbench.capture_exception(RuntimeError("x"))
        self.assertEqual(1, len(logs.output), logs.output)
        self.assertIn("plaintext", logs.output[0])
        self.assertNotIn("SECRET", logs.output[0])

    def test_configure_from_code_replaces_the_environment(self) -> None:
        ingest = FakeIngest()
        self.addCleanup(ingest.close)
        fresh(ingest.dsn(), service="billing", release="r9")
        t = _core.transport()
        self.assertIsInstance(t, DirectTransport)
        self.assertEqual("billing", t.service)
        self.assertEqual("r9", t.release)
        self.assertEqual("adt_server_testsecret", t.dsn.key)


class SelfTestTest(unittest.TestCase):
    """python -m devbench test: asks ingest whether the key is valid; creates
    nothing."""

    def setUp(self) -> None:
        self.ingest = FakeIngest()
        self.addCleanup(self.ingest.close)
        self.addCleanup(_core.reset)

    def run_test(self) -> "tuple[bool, str]":
        out = io.StringIO()
        ok = devbench.test(out)
        return ok, out.getvalue()

    def test_a_working_dsn(self) -> None:
        fresh(self.ingest.dsn(), service="acme_shop")
        ok, text = self.run_test()
        self.assertTrue(ok, text)
        self.assertIn("HTTP 200: accepted", text)
        self.assertIn('service "acme_shop"', text)
        self.assertIn('tenant "acme"', text)
        self.assertNotIn("adt_server_testsecret", text)
        checks = [r for r in self.ingest.all() if r.path == "/v1/check"]
        self.assertEqual(1, len(checks))
        self.assertEqual("adt_server_testsecret", checks[0].headers["x-adt-key"])
        self.assertEqual([], self.ingest.flushes(), "the self-test records nothing")

    def test_a_rejected_key(self) -> None:
        self.ingest.check_status = 401
        fresh(self.ingest.dsn())
        ok, text = self.run_test()
        self.assertFalse(ok)
        self.assertIn("HTTP 401: the key in DEVBENCH_DSN was rejected", text)

    def test_an_unreachable_host(self) -> None:
        from .helpers import dead_port

        fresh("http://k@127.0.0.1:%d" % dead_port())
        ok, text = self.run_test()
        self.assertFalse(ok)
        self.assertIn("could not reach http://127.0.0.1", text)

    def test_no_dsn(self) -> None:
        fresh()
        _core.config().dsn = None
        ok, text = self.run_test()
        self.assertFalse(ok)
        self.assertIn("DEVBENCH_DSN is not set", text)

    def test_a_bad_dsn(self) -> None:
        fresh()
        _core.config().dsn = "ftp://x@y"
        ok, text = self.run_test()
        self.assertFalse(ok)
        self.assertIn("DEVBENCH_DSN must start with https://", text)

    def test_the_command_exits_non_zero_on_failure(self) -> None:
        import subprocess
        import sys

        from .helpers import ROOT

        env = {k: v for k, v in os.environ.items() if not k.startswith(("DEVBENCH_", "ADT_"))}
        env["DEVBENCH_DSN"] = self.ingest.dsn()
        ok = subprocess.run([sys.executable, "-m", "devbench", "test"], cwd=ROOT, env=env,
                            capture_output=True, text=True, timeout=30)
        self.assertEqual(0, ok.returncode, ok.stdout + ok.stderr)
        self.assertIn("HTTP 200: accepted", ok.stdout)
        env["DEVBENCH_DSN"] = "http://k@ingest.example.com"
        bad = subprocess.run([sys.executable, "-m", "devbench", "test"], cwd=ROOT, env=env,
                             capture_output=True, text=True, timeout=30)
        self.assertEqual(1, bad.returncode)
        self.assertIn("plaintext", bad.stdout)


if __name__ == "__main__":
    unittest.main()
