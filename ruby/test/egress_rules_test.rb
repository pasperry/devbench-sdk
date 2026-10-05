# frozen_string_literal: true

require 'minitest/autorun'
require 'devbench'

# Learned redaction rules (the flush response's `redaction`) applied to a
# log line as the sidecar applies them: internal/sidecar/rules.go and
# Sidecar.forEgress/redactBody in strict mode. Every expected output below
# was produced by the Go sidecar itself — a Sidecar with service
# "rails-api", strict redaction, ApplyEgressRules(rules), forEgress([line])
# — for the cases in internal/sidecar/learned_test.go and a few more forms.
# Shape rules carry the fingerprint Go computed for the line's shape, so a
# Ruby shape fingerprint that drifted would match nothing and fail here.
class EgressRulesFromGoTest < Minitest::Test
  SERVICE = 'rails-api'

  FROM_GO = [
    ["shape none withholds the line",
     "[v1/sessL/act1/0] INFO Approved loan for Bob",
     [{ "fp" => "e39e47e7df201b7ef1198b84f7d7cd321ece2724576aed0f155608da10ac637d", "egress" => "none" }],
     "[v1/sessL/act1/0] <withheld:shape:e39e47e7df20>"],
    ["no rule: a single surname gets through",
     "[v1/sessL/act1/0] INFO Approved loan for Bob",
     [],
     "[v1/sessL/act1/0] [<trace>] INFO Approved loan for Bob"],
    ["the line next to a withheld one survives",
     "[v1/sessL/act1/0] INFO Completed 500 Internal Server Error in 31ms",
     [{ "fp" => "e39e47e7df201b7ef1198b84f7d7cd321ece2724576aed0f155608da10ac637d", "egress" => "none" }],
     "[v1/sessL/act1/0] [<trace>] INFO Completed <num> Internal Server Error in <dur>"],
    ["the heuristic masks a two-word phrase",
     "[v1/sessR/act1/0] INFO routing payment through Acme Ledger",
     [],
     "[v1/sessR/act1/0] [<trace>] INFO routing payment through <redacted:name>"],
    ["shape full relaxes the heuristic",
     "[v1/sessR/act1/0] INFO routing payment through Acme Ledger",
     [{ "fp" => "a67868ea8a45219f3630f72a851e7b55d0d51b34562c2d39e4b9f93433bbafbc", "egress" => "full" }],
     "[v1/sessR/act1/0] [<trace>] INFO routing payment through Acme Ledger"],
    ["shape masked keeps the heuristic",
     "[v1/sessR/act1/0] INFO routing payment through Acme Ledger",
     [{ "fp" => "a67868ea8a45219f3630f72a851e7b55d0d51b34562c2d39e4b9f93433bbafbc", "egress" => "masked" }],
     "[v1/sessR/act1/0] [<trace>] INFO routing payment through <redacted:name>"],
    ["full cannot lift base redaction",
     "[v1/sessS/act1/0] calling Acme Ledger with api_key=sk_live_abcdef123456",
     [{ "fp" => "6ca0d8309ae65a9907e32337b66734b6e9aa0e12a8a9afd11a878024ca6b35cb", "egress" => "full" }],
     "[v1/sessS/act1/0] [<trace>] calling Acme Ledger with api_key=<redacted:secret>"],
    ["full cannot lift base redaction",
     "[v1/sessS/act2/0] Acme Ledger ran SELECT * WHERE \"name\" = 'Dana Whitfield'",
     [{ "fp" => "cc7e60017410dfbb7c344accaf92b15939eadc428cc72264948aa7229e8b3b41", "egress" => "full" }],
     "[v1/sessS/act2/0] [<trace>] Acme Ledger ran SELECT * WHERE <str> = <str>"],
    ["full cannot lift base redaction",
     "[v1/sessS/act3/0] Acme Ledger notified alice@example.com",
     [{ "fp" => "c7b6c27f454a55a0ceaff75a96f8823856a39d8a1a0f3beb0a427a747fa7b866", "egress" => "full" }],
     "[v1/sessS/act3/0] [<trace>] Acme Ledger notified <email>"],
    ["field rule, form 0",
     "[v1/sessF0/act1/0] verified applicant license=D1234567 for review",
     [{ "kind" => "field", "target" => "license" }],
     "[v1/sessF0/act1/0] [<trace>] verified applicant license=<redacted:field> for review"],
    ["field rule, form 1",
     "[v1/sessF1/act1/0] verified applicant license: D1234567 for review",
     [{ "kind" => "field", "target" => "license" }],
     "[v1/sessF1/act1/0] [<trace>] verified applicant license: <redacted:field> for review"],
    ["field rule, form 2",
     "[v1/sessF2/act1/0] verified applicant \"license\": \"D1234567\" for review",
     [{ "kind" => "field", "target" => "license" }],
     "[v1/sessF2/act1/0] [<trace>] verified applicant <str>: <str> for review"],
    ["field rule, form 3",
     "[v1/sessF3/act1/0] verified applicant \"license\"=>\"D1234567\" for review",
     [{ "kind" => "field", "target" => "license" }],
     "[v1/sessF3/act1/0] [<trace>] verified applicant <str>=><str> for review"],
    ["field rule, form 4",
     "[v1/sessF4/act1/0] verified applicant :license => \"D1234567\" for review",
     [{ "kind" => "field", "target" => "license" }],
     "[v1/sessF4/act1/0] [<trace>] verified applicant :license => <redacted:field> for review"],
    ["field rule, form 5",
     "[v1/sessF5/act1/0] verified applicant License=D1234567 for review",
     [{ "kind" => "field", "target" => "license" }],
     "[v1/sessF5/act1/0] [<trace>] verified applicant License=<redacted:field> for review"],
    ["field rule, form 6",
     "[v1/sessF6/act1/0] verified applicant license='D1234567' for review",
     [{ "kind" => "field", "target" => "license" }],
     "[v1/sessF6/act1/0] [<trace>] verified applicant license=<redacted:field> for review"],
    ["field rule, form 7",
     "[v1/sessF7/act1/0] verified applicant {license: D1234567} for review",
     [{ "kind" => "field", "target" => "license" }],
     "[v1/sessF7/act1/0] [<trace>] verified applicant {license: <redacted:field>} for review"],
    ["field rule, form 8",
     "[v1/sessF8/act1/0] verified applicant license=D1234567;next=1 for review",
     [{ "kind" => "field", "target" => "license" }],
     "[v1/sessF8/act1/0] [<trace>] verified applicant license=<redacted:field>;next=<num> for review"],
    ["field rule matches only that field",
     "[v1/sessH/act1/0] licensed=true driver_license=K99 license_type=commercial",
     [{ "kind" => "field", "target" => "license" }],
     "[v1/sessH/act1/0] [<trace>] licensed=true driver_license=K99 license_type=commercial"],
    ["field rule at the start of the line",
     "license=D1234567 at start",
     [{ "kind" => "field", "target" => "license" }],
     "license=<redacted:field> at start"],
    ["field rule, mixed-case rule target",
     "[v1/sessH/act2/0] checked license=D1234567",
     [{ "kind" => "field", "target" => " License " }],
     "[v1/sessH/act2/0] [<trace>] checked license=<redacted:field>"],
    ["full on the shape does not lift a field rule",
     "[v1/sessI/act1/0] Acme Ledger checked license=D1234567",
     [{ "fp" => "52033b01630f00fc2ef69fbb05fc7ad72070d1f86f0157452b8619c5ed1decf2", "egress" => "full" }, { "kind" => "field", "target" => "license" }],
     "[v1/sessI/act1/0] [<trace>] Acme Ledger checked license=<redacted:field>"],
    ["term rule",
     "[v1/sessJ/act1/0] escalated to Whitfield for approval",
     [{ "kind" => "term", "target" => "Whitfield" }],
     "[v1/sessJ/act1/0] [<trace>] escalated to <redacted:term> for approval"],
    ["malformed rules are ignored",
     "[v1/sessK/act1/0] checked license=D1234567 ok",
     [{ "kind" => "field", "target" => ".*" }, { "kind" => "field", "target" => "lic(ense" }, { "kind" => "field" }, { "kind" => "term", "target" => "ok" }, { "kind" => "regex", "target" => ".*" }, { "kind" => "field", "target" => "license" }],
     "[v1/sessK/act1/0] [<trace>] checked license=<redacted:field> ok"],
    ["untraced line",
     "Approved loan for Alice Smith, license=D1234567",
     [{ "kind" => "field", "target" => "license" }],
     "Approved loan for <redacted:name> license=<redacted:field>"],
    ["several fields",
     "[v1/sessP/act1/0] token=abc vin=1HGCM82633A004352 plate=XYZ123",
     [{ "kind" => "field", "target" => "vin" }, { "kind" => "field", "target" => "plate" }],
     "[v1/sessP/act1/0] [<trace>] token=abc vin=<redacted:field> plate=<redacted:field>"]
  ].freeze

  def test_every_case_matches_the_go_sidecar
    refute_empty FROM_GO
    FROM_GO.each do |name, line, rules, want|
      policy = Devbench::EgressPolicy.compile(rules)
      assert_equal want, Devbench::Scrub.egress(line, policy, SERVICE), name
    end
  end

  # The shape rule only matches the shape it was learned for: the same
  # rule under another service name is another shape.
  def test_a_shape_rule_is_scoped_to_the_service
    name, line, rules, = FROM_GO.find { |c| c[0] == 'shape none withholds the line' }
    out = Devbench::Scrub.egress(line, Devbench::EgressPolicy.compile(rules), 'another-service')
    assert_includes out, 'Approved loan', name
  end

  def test_no_policy_is_the_strict_floor
    assert_equal Devbench::Scrub.egress('[v1/s/a/0] INFO paid by Alice Smith'),
                 Devbench::Scrub.egress('[v1/s/a/0] INFO paid by Alice Smith', Devbench::EgressPolicy.compile([]), SERVICE)
  end

  def test_compile_skips_what_it_cannot_enforce
    policy = Devbench::EgressPolicy.compile([nil, 'x', 7, { 'kind' => 'field', 'target' => 12 },
                                             { 'kind' => 'field', 'target' => 'vin' },
                                             { 'kind' => 'field', 'target' => 'VIN' },
                                             { 'kind' => 'term', 'target' => 'ab' },
                                             { 'fp' => '', 'egress' => 'none' }])
    refute policy.shapes?
    assert_empty policy.terms
    assert_equal 'vin=<redacted:field>', policy.mask('vin=123')
  end

  # A term is a literal, not a pattern, and its replacement is literal too.
  def test_a_term_is_literal
    policy = Devbench::EgressPolicy.compile([{ 'kind' => 'term', 'target' => 'a.c\\1' }])
    assert_equal 'abc <redacted:term>', policy.mask('abc a.c\\1')
  end
end
