# frozen_string_literal: true

# Answering log slice requests (SERVER_SDK_SPEC "In-process log capture
# (direct mode)", Delivery): a real Logger, a real HTTP server standing in
# for ingest (FakeIngest answers need_logs and redaction and accepts
# POST /v1/logs). What is asserted is what arrived over the socket.

require 'minitest/autorun'
require 'json'
require 'logger'
require 'stringio'
require 'devbench'
require_relative 'ingest_helper'

class LogDeliveryTest < Minitest::Test
  SERVICE = 'rails-api'
  SECRET = 'adt_server_sliceskey'

  def setup
    Devbench::Current.clear
    @saved_buffer = Devbench::Logs.buffer
    Devbench::Logs.buffer = Devbench::Logs::Buffer.new
    @ingest = FakeIngest.new
    configure
    @logger = Logger.new(StringIO.new)
    assert Devbench.capture_logs(@logger), 'capture must be on in direct mode'
  end

  def teardown
    Devbench.reset!
    Devbench::Logs.buffer = @saved_buffer
    @ingest&.close
    Devbench::Current.clear
  end

  def configure(interval: 3600)
    Devbench.reset!
    Devbench.configure do |c|
      c.dsn = @ingest.pair_dsn('adt_client_slicespub', SECRET)
      c.service = SERVICE
      c.release = 'r1'
      c.enabled = true
      c.flush_interval = interval
    end
  end

  # A request that logs, then fails: what triage will ask about.
  def failing_request(trace, *lines)
    Devbench::Current.with(Devbench::Trace.parse(trace)) do
      lines.each { |l| @logger.info(l) }
      Devbench.capture_exception(RuntimeError.new('boom'), symbol: 'DealsController#update')
    end
  end

  def test_requested_lines_are_delivered_redacted_with_the_secret_key
    failing_request('v1/sessA/actA/0',
                    'Started PUT "/deals/9" for 10.1.2.254',
                    'Approving deal for pat.secret@example.com card 4111 1111 1111 1111',
                    'credit check returned no score')
    @ingest.need_logs = ['sessA/actA']

    assert Devbench.flush!
    deliveries = @ingest.log_deliveries
    assert_equal 1, deliveries.length, 'one POST /v1/logs'
    delivery = deliveries.first
    assert_equal SECRET, delivery.headers['x-adt-key']
    assert_equal 'application/json', delivery.headers['content-type']

    body = delivery.json
    assert_equal ['slices'], body.keys, 'ingest decodes with DisallowUnknownFields'
    assert_equal [%w[lines trace]], body['slices'].map { |s| s.keys.sort }
    assert_equal ['[v1/sessA/actA/0] INFO Started PUT <str> for <ip>',
                  '[v1/sessA/actA/0] INFO Approving deal for <email> card <num> <num> <num> <num>',
                  '[v1/sessA/actA/0] INFO credit check returned no score'], @ingest.slices['sessA/actA']
    refute_includes delivery.body, 'pat.secret'
    refute_includes delivery.body, '4111'
    refute_includes delivery.body, '10.1.2.254'
  end

  # Another process may hold the lines; an empty answer from this one must
  # not be sent (ingest ignores it from a server key anyway).
  def test_nothing_is_sent_for_a_key_with_no_lines
    failing_request('v1/sessB/actB/0', 'a line')
    @ingest.need_logs = ['sessZ/unknown', 'sessB/actB']

    assert Devbench.flush!
    assert_equal ['sessB/actB'], @ingest.slices.keys
    refute_includes @ingest.log_deliveries.first.body, 'sessZ'
  end

  def test_no_post_at_all_when_no_requested_key_has_lines
    failing_request('v1/sessC/actC/0', 'a line')
    @ingest.need_logs = ['sessZ/unknown']

    assert Devbench.flush!
    assert_empty @ingest.log_deliveries
  end

  def test_untraced_lines_are_never_sent
    @logger.info('untraced line')
    failing_request('v1/sessD/actD/0', 'traced line')
    @ingest.need_logs = ['sessD/actD']

    assert Devbench.flush!
    refute_includes @ingest.log_deliveries.first.body, 'untraced'
  end

  # Learned rules arrive on the same response as the request they apply to,
  # and apply to it — the first slice after a judgement must not leak.
  def test_learned_rules_on_the_same_response_apply
    failing_request('v1/sessE/actE/0',
                    'applicant license=D1234567 verified',
                    'escalated to Whitfield for approval')
    @ingest.need_logs = ['sessE/actE']
    @ingest.redaction = [{ 'kind' => 'field', 'target' => 'license' },
                         { 'kind' => 'term', 'target' => 'Whitfield' }]

    assert Devbench.flush!
    assert_equal ['[v1/sessE/actE/0] INFO applicant license=<redacted:field> verified',
                  '[v1/sessE/actE/0] INFO escalated to <redacted:term> for approval'], @ingest.slices['sessE/actE']
  end

  # A shape marked 'none' sends a placeholder naming the shape; its
  # neighbours still go.
  def test_a_withheld_shape_leaves_a_placeholder
    failing_request('v1/sessW/actW/0', 'Approved loan for Bob', 'Completed 500 Internal Server Error')
    shape = Devbench::Fingerprint.compute(kind: 'log_template', source: 'sidecar', service: SERVICE,
                                          message: 'INFO Approved loan for Bob')
    @ingest.need_logs = ['sessW/actW']
    @ingest.redaction = [{ 'fp' => shape, 'egress' => 'none' }]

    assert Devbench.flush!
    assert_equal ["[v1/sessW/actW/0] <withheld:shape:#{shape[0, 12]}>",
                  '[v1/sessW/actW/0] INFO Completed <num> Internal Server Error'], @ingest.slices['sessW/actW']
  end

  # Absent is not empty: a response without a rule set keeps the rules in
  # force; an empty set withdraws them.
  def test_an_absent_rule_set_keeps_rules_and_an_empty_one_withdraws_them
    failing_request('v1/sessF/actF/0', 'applicant license=D1234567 verified')
    @ingest.need_logs = ['sessF/actF']
    @ingest.redaction = [{ 'kind' => 'field', 'target' => 'license' }]
    assert Devbench.flush!

    @ingest.redaction = nil
    failing_request('v1/sessF/actF/0')
    assert Devbench.flush!

    @ingest.redaction = []
    failing_request('v1/sessF/actF/0')
    assert Devbench.flush!

    bodies = @ingest.log_deliveries.map { |d| d.json['slices'].first['lines'].first }
    assert_equal ['[v1/sessF/actF/0] INFO applicant license=<redacted:field> verified',
                  '[v1/sessF/actF/0] INFO applicant license=<redacted:field> verified',
                  '[v1/sessF/actF/0] INFO applicant license=<hex> verified'], bodies
  end

  # Lines are retained after delivery: a later request may ask again.
  def test_lines_are_kept_after_delivery
    failing_request('v1/sessG/actG/0', 'kept')
    @ingest.need_logs = ['sessG/actG']
    assert Devbench.flush!
    failing_request('v1/sessX/actX/0')
    assert Devbench.flush!

    assert_equal 2, @ingest.log_deliveries.length
  end

  def test_a_slice_is_bounded_to_the_most_recent_lines
    failing_request('v1/sessH/actH/0', *Array.new(250) { |i| "line #{i}" })
    @ingest.need_logs = ['sessH/actH']

    assert Devbench.flush!
    lines = @ingest.slices['sessH/actH']
    assert_equal Devbench::DirectTransport::MAX_SLICE_LINES, lines.length
    assert_equal '[v1/sessH/actH/0] INFO line <num>', lines.last
  end

  def test_a_slice_is_bounded_to_64_kib
    big = 'x' * 3000
    failing_request('v1/sessK/actK/0', *Array.new(40) { |i| "#{i.even? ? 'a' : 'b'}#{big}" })
    @ingest.need_logs = ['sessK/actK']

    assert Devbench.flush!
    lines = @ingest.slices['sessK/actK']
    assert_operator lines.sum(&:bytesize), :<=, 64 * 1024
    assert_operator lines.length, :>=, 20
  end

  # Ingest refuses more than 25 slices in one delivery and bodies over
  # 256 KiB: split.
  def test_many_slices_are_split_across_requests
    keys = Array.new(30) { |i| "sessM/act#{i}" }
    keys.each { |k| failing_request("v1/#{k}/0", 'y' * 3000, 'z' * 3000) }
    @ingest.need_logs = keys

    assert Devbench.flush!
    deliveries = @ingest.log_deliveries
    assert_operator deliveries.length, :>=, 2
    deliveries.each do |d|
      assert_operator d.json['slices'].length, :<=, 25
      assert_operator d.body.bytesize, :<=, 192 * 1024
    end
    assert_equal keys.sort, @ingest.slices.keys.sort
  end

  # Delivery runs on the background flush thread, never the caller's.
  def test_the_background_thread_answers
    configure(interval: 0.2)
    Devbench.capture_logs(@logger)
    @ingest.need_logs = ['sessT/actT']
    failing_request('v1/sessT/actT/0', 'from the request thread')

    found = @ingest.wait_for(1, timeout: 5) { |r| r.path == '/v1/logs' }
    refute_empty found, 'the background flush never delivered the slice'
    assert_equal ['[v1/sessT/actT/0] INFO from the request thread'], found.first.json['slices'].first['lines']
  end

  # need_logs only rides a flush response. A process that holds lines but
  # has nothing to report polls with an empty flush, so it still answers.
  def test_a_quiet_process_polls_and_answers
    configure(interval: 0.2)
    Devbench.capture_logs(@logger)
    @ingest.need_logs = ['sessQ/actQ']
    @ingest.need_logs_only_on_poll = true
    Devbench::Current.with(Devbench::Trace.parse('v1/sessQ/actQ/0')) { @logger.info('a quiet line') }

    found = @ingest.wait_for(1, timeout: 5) { |r| r.path == '/v1/logs' }
    refute_empty found, 'a quiet process holding the lines never answered'
    assert_equal ['[v1/sessQ/actQ/0] INFO a quiet line'], found.first.json['slices'].first['lines']
    poll = @ingest.flushes.first
    assert_equal [], poll.json['counts']
    assert_equal 'adt_server_sliceskey', poll.headers['x-adt-key']
  end

  # Holding nothing, a quiet process sends nothing at all.
  def test_a_process_holding_no_lines_does_not_poll
    @logger.info('untraced: not held')
    assert Devbench.flush!
    assert_empty @ingest.requests
  end

  def test_lines_that_aged_out_stop_the_poll
    now = 0.0
    Devbench::Logs.buffer = Devbench::Logs::Buffer.new(clock: -> { now })
    Devbench::Current.with(Devbench::Trace.parse('v1/sessO/actO/0')) { @logger.info('old') }
    assert Devbench.flush!
    assert_equal 1, @ingest.flushes.length, 'held a line: one poll'

    now += Devbench::Logs::MAX_AGE + 1
    assert Devbench.flush!
    assert_equal 1, @ingest.flushes.length, 'nothing held any more: no poll'
  end

  def test_a_refused_delivery_never_raises
    @ingest.logs_status = 500
    failing_request('v1/sessR/actR/0', 'a line')
    @ingest.need_logs = ['sessR/actR']

    assert Devbench.flush!, 'the counts were accepted'
    assert_equal 2, @ingest.log_deliveries.length, 'one retry on 5xx, then discard'
  end

  # A forked worker answers only for its own lines.
  def test_a_forked_child_answers_only_for_its_own_lines
    skip 'fork unavailable' unless Process.respond_to?(:fork)

    failing_request('v1/sessP/actP/0', 'parent line')
    @ingest.need_logs = ['sessP/actP']
    pid = fork do
      failing_request('v1/sessP/actP/0', 'child line')
      Devbench.flush!
      exit!(0)
    end
    Process.wait(pid)

    lines = @ingest.slices['sessP/actP']
    assert_equal ['[v1/sessP/actP/0] INFO child line'], lines
  end
end
