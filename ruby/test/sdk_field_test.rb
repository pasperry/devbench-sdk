# frozen_string_literal: true

# Every flush request names the SDK that sent it:
# "sdk": {"name": "ruby", "version": Devbench::VERSION}
# (docs/server-sdk/wire-protocol.md, "the SDK's name and version"; #308).
# Checked on what actually arrives at FakeIngest's real socket.

require 'minitest/autorun'
require 'json'
require 'logger'
require 'stringio'
require 'devbench'
require_relative 'ingest_helper'

class SdkFieldWireTest < Minitest::Test
  def setup
    Devbench::Current.clear
    @saved_buffer = Devbench::Logs.buffer
    Devbench::Logs.buffer = Devbench::Logs::Buffer.new
    @ingest = FakeIngest.new
    Devbench.reset!
    Devbench.configure do |c|
      c.dsn = @ingest.dsn
      c.service = 'rails-api'
      c.release = 'r1'
      c.enabled = true
      c.flush_interval = 3600
    end
  end

  def teardown
    Devbench.reset!
    Devbench::Logs.buffer = @saved_buffer
    @ingest&.close
    Devbench::Current.clear
  end

  def assert_sdk(flush)
    body = flush.json
    assert_equal({ 'name' => 'ruby', 'version' => Devbench::VERSION }, body['sdk'], "sdk in #{flush.body[0, 300]}")
    # After sensor_id and before counts, the wire-protocol example's order.
    keys = body.keys
    at = keys.index('sdk')
    refute_nil at, "no sdk in #{keys}"
    assert_equal %w[sensor_id sdk counts], keys[at - 1, 3], "keys: #{keys}"
  end

  def test_a_counts_flush_carries_it
    ADT.report_handled(KeyError.new('k'), symbol: 'Billing#run')
    assert Devbench.flush!

    flush = @ingest.flushes.first
    refute_nil flush, 'no flush arrived'
    assert_sdk(flush)
    # Nothing else changes: the keys the body had before #308, plus sdk.
    assert_equal %w[v tenant source service release sensor_id sdk counts], flush.json.keys
  end

  def test_every_request_of_a_split_window_carries_it
    long = 'x' * 240
    60.times do |site| # ~10 KB a count: ~600 KB, several requests
      20.times do |u|
        Devbench::Current.with(nil) do
          ADT.set_user(email: "#{long}#{u}@d.com", account: "#{long}#{u}")
          ADT.report_handled(KeyError.new('k'), symbol: "Site#{site}#x")
        end
      end
    end
    assert Devbench.flush!(timeout: 20)

    flushes = @ingest.flushes
    assert_operator flushes.length, :>, 1, 'expected the window to be split'
    flushes.each do |f|
      assert_sdk(f)
      assert_operator f.body.bytesize, :<, 192 * 1024
    end
    assert_equal 60, flushes.sum { |f| f.json['counts'].length }
  end

  def test_the_empty_poll_carries_it
    logger = Logger.new(StringIO.new)
    assert Devbench.capture_logs(logger)
    Devbench::Current.with(Devbench::Trace.parse('v1/sessS/actS/0')) { logger.info('a quiet line') }
    assert Devbench.flush!

    poll = @ingest.flushes.first
    refute_nil poll, 'a process holding lines did not poll'
    assert_equal [], poll.json['counts']
    assert_sdk(poll)
  end
end

# The name is the one the shared vectors give this SDK's directory, both
# values pass ingest's patterns (else ingest drops the field), and the
# version is the gem's own (the gemspec's), not a second literal.
class SdkFieldValuesTest < Minitest::Test
  VECTORS_PATH = File.join(ENV['DEVBENCH_TESTDATA'] || File.expand_path('../../../../testdata', __dir__),
                           'protocol_vectors.json')

  def vectors
    unless File.exist?(VECTORS_PATH)
      flunk "missing #{VECTORS_PATH}" if ENV['ADT_REQUIRE_VECTORS'] == '1'
      skip 'shared protocol vectors live in the ADT repo (testdata/), not in a vendored gem'
    end
    JSON.parse(File.read(VECTORS_PATH)).fetch('sdk')
  end

  def test_the_name_is_the_vectors_and_both_values_pass_ingests_patterns
    sdk = vectors
    mine = sdk.fetch('names').select { |_, dir| dir == 'sdk/server/ruby' }.keys
    assert_equal [Devbench::SDK_NAME], mine
    # Go-syntax patterns; these use nothing Ruby reads differently except the
    # anchors, which must hold the whole string (\A...\z).
    name_re = Regexp.new("\\A#{sdk.fetch('name_pattern').delete_prefix('^').delete_suffix('$')}\\z")
    version_re = Regexp.new("\\A#{sdk.fetch('version_pattern').delete_prefix('^').delete_suffix('$')}\\z")
    assert_match name_re, Devbench::SDK_NAME
    assert_match version_re, Devbench::VERSION
  end

  def test_the_version_is_the_gems_version
    spec = Gem::Specification.load(File.expand_path('../devbench.gemspec', __dir__))
    refute_nil spec, 'devbench.gemspec did not load'
    assert_equal spec.version.to_s, Devbench::VERSION
  end
end
