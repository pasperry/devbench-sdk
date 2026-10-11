# frozen_string_literal: true

require 'minitest/autorun'
require 'json'
require 'adt'

# The trace contract, shared with the Go and TypeScript SDKs.
#
# If Rails renders a value the browser wrote differently, or rejects what Go
# forwards, the request still succeeds and nothing logs — the evidence just
# cannot be joined, and both detectors quietly degrade. That is risk #9, and
# this is the part of it a test can hold.
class TraceVectorsTest < Minitest::Test
  # The vectors are shared by all three SDKs, so they live at the ADT repo's
  # root, outside this gem. A vendored copy of the gem does not have them:
  # skip there rather than fail. In ADT's own CI, ADT_REQUIRE_VECTORS turns a
  # missing file into a failure, so the contract can never be skipped silently.
  # DEVBENCH_TESTDATA: the public SDK repo's CI, whose layout differs.
  VECTORS_PATH = File.join(ENV['DEVBENCH_TESTDATA'] || File.expand_path('../../../../testdata', __dir__), 'trace_vectors.json')
  VECTORS = File.exist?(VECTORS_PATH) ? JSON.parse(File.read(VECTORS_PATH)) : nil

  def setup
    return if VECTORS
    flunk "missing #{VECTORS_PATH}" if ENV['ADT_REQUIRE_VECTORS'] == '1'
    skip 'shared trace vectors live in the ADT repo (testdata/), not in a vendored gem'
  end

  def test_accepts_every_valid_vector
    refute_empty VECTORS['valid'], 'vector file is empty'

    VECTORS['valid'].each do |v|
      t = ADT::Trace.parse(v['raw'])
      refute_nil t, "rejected a vector every implementation must accept: #{v['raw']}"
      assert_equal v['session'], t.session, v['raw']
      assert_equal v['intent'],  t.intent,  v['raw']
      assert_equal v['hop'],     t.hop,     v['raw']
      assert_equal v['key'],     t.key,     v['raw']
      assert_equal v['raw'],     t.to_s,    'render must round-trip'

      if v['next_hop'].empty?
        assert_nil t.next_hop, "next_hop for #{v['raw']}"
      else
        assert_equal v['next_hop'], t.next_hop&.to_s, "next_hop for #{v['raw']}"
      end
    end
  end

  def test_rejects_every_invalid_vector
    VECTORS['invalid'].each do |bad|
      assert_nil ADT::Trace.parse(bad),
                 "accepted a vector every implementation must reject: #{bad.inspect}"
    end
  end
end

# The protocol version, shared with every SDK and ingest
# (docs/SERVER_SDK_SPEC.md, "Protocol version"). The gem sends
# PROTOCOL_VERSION as `v` in every payload (the wire tests in direct_test,
# exceptions_test and adt_test assert that); this holds the constant to the
# vectors, so a gem speaking a version ingest refuses cannot pass.
class ProtocolVectorsTest < Minitest::Test
  VECTORS_PATH = File.join(ENV['DEVBENCH_TESTDATA'] || File.expand_path('../../../../testdata', __dir__),
                           'protocol_vectors.json')
  VECTORS = File.exist?(VECTORS_PATH) ? JSON.parse(File.read(VECTORS_PATH)) : nil

  def setup
    return if VECTORS
    flunk "missing #{VECTORS_PATH}" if ENV['ADT_REQUIRE_VECTORS'] == '1'
    skip 'shared protocol vectors live in the ADT repo (testdata/), not in a vendored gem'
  end

  def test_the_declared_version_is_the_current_one
    assert_kind_of Integer, Devbench::PROTOCOL_VERSION
    assert_equal VECTORS.fetch('current'), Devbench::PROTOCOL_VERSION
  end

  def test_the_declared_version_is_supported_and_not_refused
    assert_includes VECTORS.fetch('supported'), Devbench::PROTOCOL_VERSION
    refute_includes VECTORS.fetch('refused'), Devbench::PROTOCOL_VERSION
  end
end
