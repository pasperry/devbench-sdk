# frozen_string_literal: true

require 'minitest/autorun'
require 'json'
require 'devbench'
require 'devbench/scrub'
require 'devbench/fingerprint'

# The redaction contract, shared with the sidecar and the browser
# (testdata/redaction_vectors.json). In direct mode nothing stands between
# this process and Dev Bench, so the exemplar is redacted here, with the
# sidecar's strict rules — a divergence means direct mode leaks what the
# sidecar would have removed.
#
# Same convention as the other vector tests: skip in a vendored copy, fail
# when ADT_REQUIRE_VECTORS=1 (CI).
class RedactionVectorsTest < Minitest::Test
  # DEVBENCH_TESTDATA: the public SDK repo's CI, whose layout differs.
  VECTORS_PATH = File.join(ENV['DEVBENCH_TESTDATA'] || File.expand_path('../../../../testdata', __dir__), 'redaction_vectors.json')
  VECTORS = File.exist?(VECTORS_PATH) ? JSON.parse(File.read(VECTORS_PATH)) : nil

  def setup
    return if VECTORS

    flunk "missing #{VECTORS_PATH}" if ENV['ADT_REQUIRE_VECTORS'] == '1'
    skip 'shared redaction vectors live in the repo (testdata/), not in a vendored gem'
  end

  def check(section, &transform)
    vectors = VECTORS.fetch(section)
    refute_empty vectors, "#{section} is empty; this test would prove nothing"
    vectors.each do |v|
      assert_equal v['out'], transform.call(v['in']), "#{section}: #{v['name']}"
    end
  end

  def test_shared_vectors_are_redacted
    check('shared') { |line| Devbench::Scrub.text(line) }
  end

  def test_server_only_credentials_are_redacted
    check('server_only') { |line| Devbench::Scrub.text(line) }
  end

  # As important as the rest: the line that explains a swallowed failure is
  # what triage reads.
  def test_diagnostic_content_survives
    check('must_survive') { |line| Devbench::Scrub.text(line) }
  end

  def test_prose_vectors
    check('prose') { |line| Devbench::Scrub.prose(line) }
  end

  # The card and phone rules (#186): a card passes Luhn; dates, UUIDs,
  # prefixed ids, decimals, timestamps and long ids are not phones. The same
  # transform as `shared`, and idempotent.
  def test_number_vectors
    check('numbers') { |line| Devbench::Scrub.text(line) }
    check('numbers') { |line| Devbench::Scrub.text(Devbench::Scrub.text(line)) }
  end

  # The email rule's left edge (#148): the local part stops at `=` and `:`,
  # so a key=value or JSON line keeps the key that says which field held the
  # address; connection-string credentials still go first. Idempotent too.
  def test_key_value_vectors
    check('key_value') { |line| Devbench::Scrub.text(line) }
    check('key_value') { |line| Devbench::Scrub.text(Devbench::Scrub.text(line)) }
  end
end

# The template is the message part of every fingerprint, so a templating
# divergence splits one problem in two (testdata/template_vectors.json,
# message in, template out). Each case pins one message, so a failure names
# the rule that drifted (#148: `customer=sam@example.com` ->
# `customer=<email>`).
class TemplateVectorsTest < Minitest::Test
  VECTORS_PATH = File.join(ENV['DEVBENCH_TESTDATA'] || File.expand_path('../../../../testdata', __dir__), 'template_vectors.json')
  VECTORS = File.exist?(VECTORS_PATH) ? JSON.parse(File.read(VECTORS_PATH)) : nil

  def setup
    return if VECTORS

    flunk "missing #{VECTORS_PATH}" if ENV['ADT_REQUIRE_VECTORS'] == '1'
    skip 'shared template vectors live in the repo (testdata/), not in a vendored gem'
  end

  def test_every_template_case_reproduces_exactly
    cases = VECTORS.fetch('template')
    refute_empty cases, 'template is empty; this test would prove nothing'
    cases.each do |v|
      assert_equal v['out'], Devbench::Fingerprint.template(v['in']), "template: #{v['name']}"
    end
  end
end

# The whole egress path for an exemplar — trace kept, then template, scrub,
# prose — against outputs produced by the Go sidecar's forEgress and
# answerEvidenceRequests in strict mode for the same input.
class EgressTest < Minitest::Test
  FROM_GO = {
    "NoMethodError: undefined method `name' for nil (customer pat.secret@example.com) [request]" =>
      "NoMethodError: undefined method `name' for nil (customer <email>) [request]",
    'ActiveRecord::RecordInvalid: Validation failed: Email has already been taken [explicit, handled] reason=validation' =>
      'ActiveRecord::RecordInvalid: Validation failed: Email has already been taken [explicit, handled] reason=validation',
    'KeyError: key not found: "api_key=sk_live_abcdef123456" reason=Approved for Alice Smith' =>
      'KeyError: key not found: <str> reason=Approved for <redacted:name>',
    'RuntimeError: charge 4111 1111 1111 1111 failed for v1/sessA/actB/2 from 10.1.2.254 [job]' =>
      '[v1/sessA/actB/2] RuntimeError: charge <num> <num> <num> <num> failed for <trace> from <ip> [job]',
    'Faraday::ConnectionFailed: Failed to open TCP connection to postgres://admin:s3cretpw@db.internal:5432/app ' \
    '(Authorization: Bearer abcdefghijkl) [explicit, handled]' =>
      # #148: the email local part stops at `:`, so the URL rule now takes the
      # whole connection string (it was `<email>:<num>/app`).
      'Faraday::ConnectionFailed: Failed to open TCP connection to <url> ' \
      '(Authorization: <redacted:authorization> [explicit, handled]'
  }.freeze

  TEMPLATES_FROM_GO = {
    'NoMethodError at DealsController#update' => 'NoMethodError at DealsController#update',
    'CustomersController#update' => 'CustomersController#update',
    'Pat Smith Error at Acme::Billing#run' => '<redacted:name> Error at Acme::Billing#run'
  }.freeze

  def test_exemplars_match_the_sidecar
    FROM_GO.each { |line, want| assert_equal want, Devbench::Scrub.egress(line), line }
  end

  def test_template_text_matches_the_sidecar
    TEMPLATES_FROM_GO.each { |text, want| assert_equal want, Devbench::Scrub.template_text(text), text }
  end

  # Go's \b is ASCII: a shape right after a non-ASCII letter is still at a
  # word boundary there, and must still be redacted here.
  def test_shapes_after_non_ascii_letters_match_go
    assert_equal 'é<redacted:ssn> ok', Devbench::Scrub.text('é123-45-6789 ok')
    assert_equal 'ñ<redacted:aws-key> x', Devbench::Scrub.text('ñAKIAIOSFODNN7EXAMPLE x')
  end

  # Framework vocabulary breaks a run of capitalised words (Go: unchanged).
  def test_vocabulary_breaks_a_run
    assert_equal 'Done Ok Fine', Devbench::Scrub.prose('Done Ok Fine')
  end

  def test_invalid_utf8_does_not_raise
    assert_kind_of String, Devbench::Scrub.egress("bad \xff pat@example.com".b)
    refute_includes Devbench::Scrub.egress("bad \xff pat@example.com".b), 'pat@example.com'
  end
end
