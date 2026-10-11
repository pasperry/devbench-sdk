"""The shared contracts (docs/SERVER_SDK_SPEC.md, "Conformance"): the
fingerprint, redaction, trace and protocol vectors in testdata/, and the
egress outputs of the Go sidecar itself. If this SDK disagreed with the
others by one byte, the same problem would be counted as two issues, or a
line another SDK redacts would leave this one readable.

The vector files live at the repo root, outside this package: a vendored copy
skips, and ADT_REQUIRE_VECTORS=1 (CI) turns a missing file into a failure.
"""

from __future__ import annotations

import unittest
from typing import Any, Callable

import devbench
from devbench import fingerprint as F
from devbench import scrub
from devbench.egress_policy import EgressPolicy
from devbench.trace import Trace

from .helpers import vectors


class FingerprintVectorsTest(unittest.TestCase):
    def test_every_vector_reproduces_exactly(self) -> None:
        cases = vectors("fingerprint_vectors.json", self)
        self.assertTrue(cases, "vector file is empty")
        for v in cases:
            s = v["signal"]
            got = F.compute(s["kind"], s["source"], s.get("service") or "", s.get("type") or "",
                            s.get("message") or "", s.get("frames") or [])
            self.assertEqual(v["expected_fp"], got, "fingerprint for %s" % v["name"])

    # The intermediate form too, so a mismatch says which part diverged.
    def test_every_normalized_form_matches(self) -> None:
        for v in vectors("fingerprint_vectors.json", self):
            s = v["signal"]
            norm = F.normalize(s["kind"], s["source"], s.get("service") or "", s.get("type") or "",
                               s.get("message") or "", s.get("frames") or [])
            want = v["normalized"]
            self.assertEqual(want["template"], norm["template"], "template for %s" % v["name"])
            self.assertEqual(want.get("frames") or [], norm["frames"], "frames for %s" % v["name"])
            self.assertEqual(want["type"], norm["type"], "type for %s" % v["name"])


class FingerprintSemanticsTest(unittest.TestCase):
    """Where Go's regexp and Python's differ, pinned against what the Go
    implementation computes, so the port cannot drift back to Python
    semantics."""

    # Go's \b is ASCII: "é" is not a word character, so "é1" has a boundary
    # before the digit. Python's \b would see none and leave the number.
    def test_word_boundaries_are_ascii_like_go(self) -> None:
        self.assertEqual("é<num> x2", F.template("é1 x2"))
        self.assertEqual("Ünïcödé <dur> µs <dur>", F.template("Ünïcödé 12ms µs 3µs"))

    # Go's \d is ASCII; Python's matches every Unicode digit.
    def test_non_ascii_digits_are_not_numbers(self) -> None:
        self.assertEqual("order ٣٤٥ <num>", F.template("order ٣٤٥ 345"))

    # Go's \s has no \v; a vertical tab is not collapsed.
    def test_vertical_tab_is_not_go_whitespace(self) -> None:
        self.assertEqual("a\vb c", F.template("a\vb \t c"))

    # strings.TrimSpace trims Unicode spaces.
    def test_unicode_spaces_are_trimmed_like_go(self) -> None:
        self.assertEqual("Boom", F.template(" Boom　"))

    def test_dependency_frames_and_line_numbers_do_not_count(self) -> None:
        app = [{"function": "A#b", "file": "/srv/app/app/models/a.rb", "line": 1}]
        with_gem = [{"function": "x", "file": "/usr/local/bundle/gems/rack-3/lib/rack.rb", "line": 9},
                    {"function": "A#b", "file": "/home/ci/build/app/models/a.rb", "line": 77}]
        self.assertEqual(F.compute("error", "server", type="E", frames=app),
                         F.compute("error", "server", type="E", frames=with_gem))

    def test_only_five_frames_count(self) -> None:
        frames = [{"function": "f%d" % i, "file": "app/x%d.py" % i, "line": i} for i in range(1, 8)]
        deeper = frames[:5] + [{"function": "other", "file": "app/other.py", "line": 1}]
        self.assertEqual(F.compute("error", "server", type="E", frames=frames),
                         F.compute("error", "server", type="E", frames=deeper))
        self.assertNotEqual(F.compute("error", "server", type="E", frames=frames),
                            F.compute("error", "server", type="E", frames=frames[:4]))

    def test_asset_digests_are_stripped(self) -> None:
        self.assertEqual("assets/application.js",
                         F.normalize_path("https://x.example.com/assets/application-3f9a2b0c4d5e6f70.js"))
        self.assertEqual("js/index.js", F.normalize_path("/js/index-BRxHn9vj.js"))
        self.assertEqual("js/feature-flags.js", F.normalize_path("/js/feature-flags.js"))

    def test_nothing_to_fingerprint_is_none(self) -> None:
        self.assertIsNone(F.compute("error", "server"))
        self.assertIsNone(F.compute("", "server", type="E"))

    # A lone surrogate (a surrogateescape'd byte from a path) is U+FFFD, as
    # Go reads the invalid byte, and never raises.
    def test_lone_surrogates_are_replacement_characters(self) -> None:
        self.assertEqual(F.compute("error", "server", type="E", message="bad � byte"),
                         F.compute("error", "server", type="E", message="bad \udcff byte"))


class RedactionVectorsTest(unittest.TestCase):
    def check(self, section: str, transform: Callable[[str], str]) -> None:
        cases = vectors("redaction_vectors.json", self)[section]
        self.assertTrue(cases, "%s is empty; this test would prove nothing" % section)
        for v in cases:
            self.assertEqual(v["out"], transform(v["in"]), "%s: %s" % (section, v["name"]))

    def test_shared_vectors_are_redacted(self) -> None:
        self.check("shared", scrub.text)

    def test_server_only_credentials_are_redacted(self) -> None:
        self.check("server_only", scrub.text)

    # As important as the rest: the line that explains a swallowed failure is
    # what triage reads.
    def test_diagnostic_content_survives(self) -> None:
        self.check("must_survive", scrub.text)

    def test_prose_vectors(self) -> None:
        self.check("prose", scrub.prose)

    # The card and phone rules (#186): a card passes Luhn; dates, UUIDs,
    # prefixed ids, decimals, timestamps and long ids are not phones. The
    # same transform as `shared`, and idempotent.
    def test_number_vectors(self) -> None:
        self.check("numbers", scrub.text)
        self.check("numbers", lambda line: scrub.text(scrub.text(line)))


class EgressFromGoTest(unittest.TestCase):
    """The whole egress path for an exemplar against outputs produced by the
    Go sidecar's forEgress and answerEvidenceRequests in strict mode (the
    table the Ruby SDK holds itself to)."""

    FROM_GO = {
        "NoMethodError: undefined method `name' for nil (customer pat.secret@example.com) [request]":
            "NoMethodError: undefined method `name' for nil (customer <email>) [request]",
        "ActiveRecord::RecordInvalid: Validation failed: Email has already been taken [explicit, handled] reason=validation":
            "ActiveRecord::RecordInvalid: Validation failed: Email has already been taken [explicit, handled] reason=validation",
        'KeyError: key not found: "api_key=sk_live_abcdef123456" reason=Approved for Alice Smith':
            "KeyError: key not found: <str> reason=Approved for <redacted:name>",
        "RuntimeError: charge 4111 1111 1111 1111 failed for v1/sessA/actB/2 from 10.1.2.254 [job]":
            "[v1/sessA/actB/2] RuntimeError: charge <num> <num> <num> <num> failed for <trace> from <ip> [job]",
        "Faraday::ConnectionFailed: Failed to open TCP connection to postgres://admin:s3cretpw@db.internal:5432/app "
        "(Authorization: Bearer abcdefghijkl) [explicit, handled]":
            "Faraday::ConnectionFailed: Failed to open TCP connection to <email>:<num>/app "
            "(Authorization: <redacted:authorization> [explicit, handled]",
    }

    TEMPLATES_FROM_GO = {
        "NoMethodError at DealsController#update": "NoMethodError at DealsController#update",
        "CustomersController#update": "CustomersController#update",
        "Pat Smith Error at Acme::Billing#run": "<redacted:name> Error at Acme::Billing#run",
    }

    def test_exemplars_match_the_sidecar(self) -> None:
        for line, want in self.FROM_GO.items():
            self.assertEqual(want, scrub.egress(line), line)

    def test_template_text_matches_the_sidecar(self) -> None:
        for value, want in self.TEMPLATES_FROM_GO.items():
            self.assertEqual(want, scrub.template_text(value), value)

    def test_shapes_after_non_ascii_letters_match_go(self) -> None:
        self.assertEqual("é<redacted:ssn> ok", scrub.text("é123-45-6789 ok"))
        self.assertEqual("ñ<redacted:aws-key> x", scrub.text("ñAKIAIOSFODNN7EXAMPLE x"))

    def test_vocabulary_breaks_a_run(self) -> None:
        self.assertEqual("Done Ok Fine", scrub.prose("Done Ok Fine"))

    def test_lone_surrogates_do_not_raise(self) -> None:
        self.assertNotIn("pat@example.com", scrub.egress("bad \udcff pat@example.com"))


# Learned rules applied to a log line as the sidecar applies them. Every
# expected output was produced by the Go sidecar itself (service "rails-api",
# strict redaction, ApplyEgressRules(rules), forEgress([line])): the table in
# the Ruby SDK's egress_rules_test.rb. Shape rules carry the fingerprint Go
# computed for the line's shape, so a Python shape fingerprint that drifted
# would match nothing and fail here.
EGRESS_RULES_FROM_GO: Any = [
    ("shape none withholds the line",
     "[v1/sessL/act1/0] INFO Approved loan for Bob",
     [{"fp": "e39e47e7df201b7ef1198b84f7d7cd321ece2724576aed0f155608da10ac637d", "egress": "none"}],
     "[v1/sessL/act1/0] <withheld:shape:e39e47e7df20>"),
    ("no rule: a single surname gets through",
     "[v1/sessL/act1/0] INFO Approved loan for Bob",
     [],
     "[v1/sessL/act1/0] [<trace>] INFO Approved loan for Bob"),
    ("the line next to a withheld one survives",
     "[v1/sessL/act1/0] INFO Completed 500 Internal Server Error in 31ms",
     [{"fp": "e39e47e7df201b7ef1198b84f7d7cd321ece2724576aed0f155608da10ac637d", "egress": "none"}],
     "[v1/sessL/act1/0] [<trace>] INFO Completed <num> Internal Server Error in <dur>"),
    ("the heuristic masks a two-word phrase",
     "[v1/sessR/act1/0] INFO routing payment through Acme Ledger",
     [],
     "[v1/sessR/act1/0] [<trace>] INFO routing payment through <redacted:name>"),
    ("shape full relaxes the heuristic",
     "[v1/sessR/act1/0] INFO routing payment through Acme Ledger",
     [{"fp": "a67868ea8a45219f3630f72a851e7b55d0d51b34562c2d39e4b9f93433bbafbc", "egress": "full"}],
     "[v1/sessR/act1/0] [<trace>] INFO routing payment through Acme Ledger"),
    ("shape masked keeps the heuristic",
     "[v1/sessR/act1/0] INFO routing payment through Acme Ledger",
     [{"fp": "a67868ea8a45219f3630f72a851e7b55d0d51b34562c2d39e4b9f93433bbafbc", "egress": "masked"}],
     "[v1/sessR/act1/0] [<trace>] INFO routing payment through <redacted:name>"),
    ("full cannot lift base redaction",
     "[v1/sessS/act1/0] calling Acme Ledger with api_key=sk_live_abcdef123456",
     [{"fp": "6ca0d8309ae65a9907e32337b66734b6e9aa0e12a8a9afd11a878024ca6b35cb", "egress": "full"}],
     "[v1/sessS/act1/0] [<trace>] calling Acme Ledger with api_key=<redacted:secret>"),
    ("full cannot lift base redaction",
     "[v1/sessS/act2/0] Acme Ledger ran SELECT * WHERE \"name\" = 'Dana Whitfield'",
     [{"fp": "cc7e60017410dfbb7c344accaf92b15939eadc428cc72264948aa7229e8b3b41", "egress": "full"}],
     "[v1/sessS/act2/0] [<trace>] Acme Ledger ran SELECT * WHERE <str> = <str>"),
    ("full cannot lift base redaction",
     "[v1/sessS/act3/0] Acme Ledger notified alice@example.com",
     [{"fp": "c7b6c27f454a55a0ceaff75a96f8823856a39d8a1a0f3beb0a427a747fa7b866", "egress": "full"}],
     "[v1/sessS/act3/0] [<trace>] Acme Ledger notified <email>"),
    ("field rule, form 0",
     "[v1/sessF0/act1/0] verified applicant license=D1234567 for review",
     [{"kind": "field", "target": "license"}],
     "[v1/sessF0/act1/0] [<trace>] verified applicant license=<redacted:field> for review"),
    ("field rule, form 1",
     "[v1/sessF1/act1/0] verified applicant license: D1234567 for review",
     [{"kind": "field", "target": "license"}],
     "[v1/sessF1/act1/0] [<trace>] verified applicant license: <redacted:field> for review"),
    ("field rule, form 2",
     "[v1/sessF2/act1/0] verified applicant \"license\": \"D1234567\" for review",
     [{"kind": "field", "target": "license"}],
     "[v1/sessF2/act1/0] [<trace>] verified applicant <str>: <str> for review"),
    ("field rule, form 3",
     "[v1/sessF3/act1/0] verified applicant \"license\":\"D1234567\" for review",
     [{"kind": "field", "target": "license"}],
     "[v1/sessF3/act1/0] [<trace>] verified applicant <str>:<str> for review"),
    ("field rule, form 4",
     "[v1/sessF4/act1/0] verified applicant :license : \"D1234567\" for review",
     [{"kind": "field", "target": "license"}],
     "[v1/sessF4/act1/0] [<trace>] verified applicant :license : <redacted:field> for review"),
    ("field rule, form 5",
     "[v1/sessF5/act1/0] verified applicant License=D1234567 for review",
     [{"kind": "field", "target": "license"}],
     "[v1/sessF5/act1/0] [<trace>] verified applicant License=<redacted:field> for review"),
    ("field rule, form 6",
     "[v1/sessF6/act1/0] verified applicant license='D1234567' for review",
     [{"kind": "field", "target": "license"}],
     "[v1/sessF6/act1/0] [<trace>] verified applicant license=<redacted:field> for review"),
    ("field rule, form 7",
     "[v1/sessF7/act1/0] verified applicant {license: D1234567} for review",
     [{"kind": "field", "target": "license"}],
     "[v1/sessF7/act1/0] [<trace>] verified applicant {license: <redacted:field>} for review"),
    ("field rule, form 8",
     "[v1/sessF8/act1/0] verified applicant license=D1234567;next=1 for review",
     [{"kind": "field", "target": "license"}],
     "[v1/sessF8/act1/0] [<trace>] verified applicant license=<redacted:field>;next=<num> for review"),
    ("field rule matches only that field",
     "[v1/sessH/act1/0] licensed=true driver_license=K99 license_type=commercial",
     [{"kind": "field", "target": "license"}],
     "[v1/sessH/act1/0] [<trace>] licensed=true driver_license=K99 license_type=commercial"),
    ("field rule at the start of the line",
     "license=D1234567 at start",
     [{"kind": "field", "target": "license"}],
     "license=<redacted:field> at start"),
    ("field rule, mixed-case rule target",
     "[v1/sessH/act2/0] checked license=D1234567",
     [{"kind": "field", "target": " License "}],
     "[v1/sessH/act2/0] [<trace>] checked license=<redacted:field>"),
    ("full on the shape does not lift a field rule",
     "[v1/sessI/act1/0] Acme Ledger checked license=D1234567",
     [{"fp": "52033b01630f00fc2ef69fbb05fc7ad72070d1f86f0157452b8619c5ed1decf2", "egress": "full"}, {"kind": "field", "target": "license"}],
     "[v1/sessI/act1/0] [<trace>] Acme Ledger checked license=<redacted:field>"),
    ("term rule",
     "[v1/sessJ/act1/0] escalated to Whitfield for approval",
     [{"kind": "term", "target": "Whitfield"}],
     "[v1/sessJ/act1/0] [<trace>] escalated to <redacted:term> for approval"),
    ("malformed rules are ignored",
     "[v1/sessK/act1/0] checked license=D1234567 ok",
     [{"kind": "field", "target": ".*"}, {"kind": "field", "target": "lic(ense"}, {"kind": "field"}, {"kind": "term", "target": "ok"}, {"kind": "regex", "target": ".*"}, {"kind": "field", "target": "license"}],
     "[v1/sessK/act1/0] [<trace>] checked license=<redacted:field> ok"),
    ("untraced line",
     "Approved loan for Alice Smith, license=D1234567",
     [{"kind": "field", "target": "license"}],
     "Approved loan for <redacted:name> license=<redacted:field>"),
    ("several fields",
     "[v1/sessP/act1/0] token=abc vin=1HGCM82633A004352 plate=XYZ123",
     [{"kind": "field", "target": "vin"}, {"kind": "field", "target": "plate"}],
     "[v1/sessP/act1/0] [<trace>] token=abc vin=<redacted:field> plate=<redacted:field>"),
]


class EgressRulesFromGoTest(unittest.TestCase):
    SERVICE = "rails-api"

    def test_every_case_matches_the_go_sidecar(self) -> None:
        self.assertTrue(EGRESS_RULES_FROM_GO)
        for name, line, rules, want in EGRESS_RULES_FROM_GO:
            self.assertEqual(want, scrub.egress(line, EgressPolicy.compile(rules), self.SERVICE), name)

    # The same rule under another service name is another shape.
    def test_a_shape_rule_is_scoped_to_the_service(self) -> None:
        name, line, rules, _ = next(c for c in EGRESS_RULES_FROM_GO if c[0] == "shape none withholds the line")
        self.assertIn("Approved loan", scrub.egress(line, EgressPolicy.compile(rules), "another-service"), name)

    def test_no_policy_is_the_strict_floor(self) -> None:
        line = "[v1/s/a/0] INFO paid by Alice Smith"
        self.assertEqual(scrub.egress(line), scrub.egress(line, EgressPolicy.compile([]), self.SERVICE))

    def test_compile_skips_what_it_cannot_enforce(self) -> None:
        policy = EgressPolicy.compile([None, "x", 7, {"kind": "field", "target": 12},
                                       {"kind": "field", "target": "vin"}, {"kind": "field", "target": "VIN"},
                                       {"kind": "term", "target": "ab"}, {"fp": "", "egress": "none"}])
        self.assertFalse(policy.has_shapes())
        self.assertEqual((), policy.terms)
        self.assertEqual("vin=<redacted:field>", policy.mask("vin=123"))

    # A term is a literal, not a pattern, and its replacement is literal too.
    def test_a_term_is_literal(self) -> None:
        policy = EgressPolicy.compile([{"kind": "term", "target": "a.c\\1"}])
        self.assertEqual("abc <redacted:term>", policy.mask("abc a.c\\1"))


class TraceVectorsTest(unittest.TestCase):
    def test_accepts_every_valid_vector(self) -> None:
        cases = vectors("trace_vectors.json", self)["valid"]
        self.assertTrue(cases, "vector file is empty")
        for v in cases:
            t = Trace.parse(v["raw"])
            self.assertIsNotNone(t, "rejected a vector every implementation must accept: %s" % v["raw"])
            assert t is not None
            self.assertEqual(v["session"], t.session, v["raw"])
            self.assertEqual(v["intent"], t.intent, v["raw"])
            self.assertEqual(v["hop"], t.hop, v["raw"])
            self.assertEqual(v["key"], t.key, v["raw"])
            self.assertEqual(v["raw"], str(t), "render must round-trip")
            nxt = t.next_hop()
            if v["next_hop"] == "":
                self.assertIsNone(nxt, "next_hop for %s" % v["raw"])
            else:
                self.assertEqual(v["next_hop"], str(nxt), "next_hop for %s" % v["raw"])

    def test_rejects_every_invalid_vector(self) -> None:
        cases = vectors("trace_vectors.json", self)["invalid"]
        self.assertTrue(cases)
        for bad in cases:
            self.assertIsNone(Trace.parse(bad), "accepted a vector every implementation must reject: %r" % bad)


class ProtocolVectorsTest(unittest.TestCase):
    """Every payload sends devbench.PROTOCOL_VERSION as `v` (the wire tests in
    test_direct assert that); this holds the constant to the vectors, so a
    release speaking a version ingest refuses cannot pass."""

    def test_the_declared_version_is_the_current_one(self) -> None:
        v = vectors("protocol_vectors.json", self)
        self.assertIsInstance(devbench.PROTOCOL_VERSION, int)
        self.assertEqual(v["current"], devbench.PROTOCOL_VERSION)

    def test_the_declared_version_is_supported_and_not_refused(self) -> None:
        v = vectors("protocol_vectors.json", self)
        self.assertIn(devbench.PROTOCOL_VERSION, v["supported"])
        self.assertNotIn(devbench.PROTOCOL_VERSION, v["refused"])


if __name__ == "__main__":
    unittest.main()
