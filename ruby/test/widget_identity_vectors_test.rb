# frozen_string_literal: true

require 'minitest/autorun'
require 'json'
require 'devbench'

# The chat widget's signed identity (docs/server-sdk/chat-widget.md, Capability 6),
# held to testdata/widget_identity_vectors.json: every `sign` token byte for
# byte, every `refuse` input refused. The Go signer and the chat service's
# verifier pass the same file; a divergence is a widget that never renders.
#
# Same convention as the other vector tests: skip in a vendored copy, fail
# when ADT_REQUIRE_VECTORS=1 (CI, and the public SDK repo's CI).
class WidgetIdentityVectorsTest < Minitest::Test
  VECTORS_PATH = File.join(ENV['DEVBENCH_TESTDATA'] || File.expand_path('../../../../testdata', __dir__),
                           'widget_identity_vectors.json')
  VECTORS = File.exist?(VECTORS_PATH) ? JSON.parse(File.read(VECTORS_PATH)) : nil

  def setup
    return if VECTORS

    flunk "missing #{VECTORS_PATH}" if ENV['ADT_REQUIRE_VECTORS'] == '1'
    skip 'shared widget identity vectors live in the repo (testdata/), not in a vendored gem'
  end

  def sign(vector, issued_at)
    Devbench::WidgetIdentity.sign(vector['secret'], email: vector['email'], account: vector['account'],
                                                    role: vector['role'], chat: vector['chat'], issued_at: issued_at)
  end

  def test_sign_vectors_are_reproduced_exactly
    vectors = VECTORS.fetch('sign')
    refute_empty vectors, 'sign is empty; this test would prove nothing'
    vectors.each do |v|
      assert_equal v['token'], sign(v, v['issued_at']), "sign: #{v['name']}"
    end
  end

  # Symbols are what a Rails app passes (chat: [:report, :howto]).
  def test_chat_capabilities_may_be_symbols
    v = VECTORS.fetch('sign').find { |s| s['chat'].is_a?(Array) && !s['chat'].empty? }
    refute_nil v
    token = Devbench::WidgetIdentity.sign(v['secret'], email: v['email'], account: v['account'], role: v['role'],
                                                       chat: v['chat'].map(&:to_sym), issued_at: v['issued_at'])
    assert_equal v['token'], token, v['name']
  end

  def test_refuse_vectors_are_refused
    vectors = VECTORS.fetch('refuse')
    refute_empty vectors, 'refuse is empty; this test would prove nothing'
    vectors.each do |v|
      assert_nil sign(v, 1_791_460_740), "refuse: #{v['name']}"
    end
  end

  def test_the_capability_list_is_the_contracts
    assert_equal VECTORS.fetch('capabilities'), Devbench::WidgetIdentity::CAPABILITIES
  end

  def test_the_format_is_dbw1
    assert VECTORS.fetch('format').start_with?("#{Devbench::WidgetIdentity::PREFIX}."), VECTORS['format']
  end
end
