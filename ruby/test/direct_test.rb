# frozen_string_literal: true

require 'minitest/autorun'
require 'open3'
require 'rbconfig'
require 'stringio'
require 'devbench'
require_relative 'ingest_helper'
require_relative 'sidecar_helper'

# Direct mode against a real HTTP server (FakeIngest): what is asserted is
# what arrived over the socket. docs/SERVER_SDK_SPEC.md, "Direct mode".
module DirectSetup
  SERVICE = 'rails-api'

  def setup
    Devbench::Current.clear
    @ingest = FakeIngest.new
    configure(@ingest.dsn)
  end

  def teardown
    Devbench.reset!
    @ingest&.close
    Devbench::Current.clear
  end

  def configure(dsn, interval: 3600)
    Devbench.reset!
    Devbench.configure do |c|
      c.dsn = dsn
      c.service = SERVICE
      c.release = 'r1'
      c.enabled = true
      c.flush_interval = interval
    end
  end

  def raised(error)
    raise error
  rescue StandardError => e
    e
  end

  def exception_fp(error, symbol: nil)
    Devbench::Fingerprint.compute(
      kind: 'error', source: 'server', service: SERVICE, type: error.class.name,
      message: error.message, frames: Devbench::Backtrace.frames(error.backtrace, Dir.pwd)
    ).tap { |fp| refute_nil fp, "no fingerprint for #{symbol}" }
  end

  def handled_fp(type, symbol)
    Devbench::Fingerprint.compute(kind: 'handled_failure', source: 'server', service: SERVICE,
                                  type: type, message: symbol)
  end

  def counts(flush)
    flush.json['counts'].to_h { |c| [c['fp'], c] }
  end
end

class DirectFlushTest < Minitest::Test
  include DirectSetup

  # DECISIONS #161: a pair DSN authenticates with its secret; the public
  # part is for the page and is never sent by the server.
  def test_a_pair_dsn_flushes_with_the_secret_key
    configure(@ingest.pair_dsn('adt_client_pub', 'adt_server_sec'))
    ADT.capture_exception(raised(ArgumentError.new('boom')), symbol: 'Billing#run')

    assert Devbench.flush!
    flush = @ingest.flushes.first
    refute_nil flush, 'no flush arrived'
    assert_equal 'adt_server_sec', flush.headers['x-adt-key']
    refute_includes flush.headers.values.join(' '), 'adt_client_pub'
  end

  def test_phase_one_body_carries_counts_and_nothing_else
    error = raised(ArgumentError.new('boom for pat@example.com'))
    ADT.capture_exception(error, symbol: 'Billing#run')

    assert Devbench.flush!
    flush = @ingest.flushes.first
    refute_nil flush, 'no flush arrived'
    assert_equal 'adt_server_testkey', flush.headers['x-adt-key']
    assert_equal 'application/json', flush.headers['content-type']

    body = flush.json
    assert_equal [1, '', 'server', SERVICE, 'r1'], body.values_at('v', 'tenant', 'source', 'service', 'release')
    assert_match(/\Arb-[0-9a-f]{20}\z/, body['sensor_id'])
    assert_equal 1, body['counts'].length

    count = body['counts'].first
    assert_equal exception_fp(error), count['fp']
    assert_equal ['error', 1], count.values_at('kind', 'n')
    assert_operator count['first'], :>, 0
    assert_equal count['first'], count['last']
    assert_equal %w[fp n first last kind].sort, count.keys.sort, 'no message, no frames, no users unless set'
    refute_includes flush.body, 'pat@example.com'
    refute_includes flush.body, 'boom'
  end

  def test_repeats_are_counted_not_resent
    3.times do
      ADT.report_handled(KeyError.new('k'), symbol: 'CustomersController#update', reason: 'validation')
    end
    ADT.report_handled(KeyError.new('k'), symbol: 'Other#site')

    assert Devbench.flush!
    assert_equal 1, @ingest.flushes.length
    got = counts(@ingest.flushes.first)
    assert_equal 3, got.fetch(handled_fp('KeyError', 'CustomersController#update'))['n']
    assert_equal 1, got.fetch(handled_fp('KeyError', 'Other#site'))['n']
    assert_equal ['handled_failure'], got.values.map { |c| c['kind'] }.uniq
  end

  # The browser-facing half of report_handled is untouched by direct mode.
  def test_report_handled_still_marks_the_response_first
    app = lambda do |_env|
      ADT.report_handled(KeyError.new('k'), symbol: 'S#x')
      [200, {}, ['ok']]
    end
    _, headers, = Devbench::Middleware.new(app).call('HTTP_X_ADT_TRACE' => 'v1/s/a/0')
    assert_equal '1', headers['x-adt-handled']
  end

  def test_an_empty_window_sends_nothing
    assert Devbench.flush!
    ADT.report_handled(KeyError.new('k'), symbol: 'S#x')
    assert Devbench.flush!
    assert Devbench.flush!
    assert_equal 1, @ingest.flushes.length
  end

  def test_users_are_normalized_deduplicated_and_capped_at_twenty
    25.times do |i|
      Devbench::Current.with(nil) do
        ADT.set_user(email: " User#{i}@Example.COM ", account: 'acct-1')
        ADT.report_handled(KeyError.new('k'), symbol: 'S#x')
      end
    end
    Devbench::Current.with(nil) do
      ADT.set_user(email: 'user0@example.com', account: 'acct-1') # a repeat
      ADT.report_handled(KeyError.new('k'), symbol: 'S#x')
    end

    assert Devbench.flush!
    count = @ingest.flushes.first.json['counts'].first
    assert_equal 26, count['n']
    assert_equal 20, count['users'].length
    assert_equal 5, count['users_overflow']
    assert_equal({ 'email' => 'user0@example.com', 'account' => 'acct-1' }, count['users'].first)
  end

  # Identity is per window, like the count.
  def test_users_do_not_carry_into_the_next_window
    Devbench::Current.with(nil) do
      ADT.set_user(email: 'pat@example.com')
      ADT.report_handled(KeyError.new('k'), symbol: 'S#x')
    end
    Devbench.flush!
    ADT.report_handled(KeyError.new('k'), symbol: 'S#x')
    Devbench.flush!

    first, second = @ingest.flushes.map { |f| f.json['counts'].first }
    assert_equal [{ 'email' => 'pat@example.com' }], first['users']
    refute second.key?('users')
  end

  # The table is the buffer: bounded, and what does not fit is counted.
  def test_the_table_is_bounded_and_overflow_is_reported
    600.times { |i| ADT.report_handled(KeyError.new('k'), symbol: "Site#{i}#x") }
    assert Devbench.flush!

    bodies = @ingest.flushes.map(&:json)
    assert_equal 512, bodies.sum { |b| b['counts'].length }
    assert_equal 88, bodies.first['overflowed']
  end

  # Identity makes big batches ordinary: split under 192 KiB, every count
  # sent, overflow on the first request only.
  def test_large_windows_are_split_under_192_kib
    long = 'x' * 240
    512.times do |site|
      20.times do |u|
        Devbench::Current.with(nil) do
          ADT.set_user(email: "#{long}#{u}@d.com", account: "#{long}#{u}")
          ADT.report_handled(KeyError.new('k'), symbol: "Site#{site}#x")
        end
      end
    end
    ADT.report_handled(KeyError.new('k'), symbol: 'one-too-many')
    assert Devbench.flush!(timeout: 20)

    flushes = @ingest.flushes
    assert_operator flushes.length, :>, 10
    flushes.each { |f| assert_operator f.body.bytesize, :<, 192 * 1024 }
    assert_equal 512, flushes.sum { |f| f.json['counts'].length }
    assert_equal [1], flushes.map { |f| f.json['overflowed'] }.compact
    assert_equal 1, flushes.first.json['overflowed']
  end
end

class DirectEvidenceTest < Minitest::Test
  include DirectSetup

  def test_evidence_is_uploaded_when_asked_in_the_sidecars_bundle_shape
    @ingest.ask_evidence = true
    error = raised(NoMethodError.new("undefined method `name' for nil (customer pat.secret@example.com, card 4111 1111 1111 1111)"))
    Devbench::Current.with(nil) do
      ADT.set_user(email: 'Pat@Example.com', account: 'acct-1182')
      Devbench::Reporter.capture(error, context: 'request', handled: false, symbol: 'DealsController#update')
    end

    assert Devbench.flush!
    put = @ingest.wait_for { |r| r.method == 'PUT' }.first
    refute_nil put, 'evidence was asked for and never uploaded'
    assert_equal "/evidence/#{exception_fp(error)}", put.path
    assert_equal 'application/json', put.headers['content-type']
    assert_nil put.headers['x-adt-key'], 'the ingest key must not go to the storage host'

    bundle = put.json
    assert_equal %w[exemplars fp frames kind service template v], bundle.keys.sort
    assert_equal [1, exception_fp(error), 'error', SERVICE],
                 bundle.values_at('v', 'fp', 'kind', 'service')
    assert_equal 'NoMethodError at DealsController#update', bundle['template']
    assert_equal ["NoMethodError: undefined method `name' for nil (customer <email>, card <num> <num> <num> <num>) [request]"],
                 bundle['exemplars']
    assert_equal Devbench::Backtrace.frames(error.backtrace, Dir.pwd).map { |f| f.transform_keys(&:to_s) },
                 bundle['frames']

    text = put.body.downcase
    %w[pat.secret@example.com pat@example.com acct-1182 4111].each do |leak|
      refute_includes text, leak, 'PII or identity reached the evidence bundle'
    end
  end

  def test_handled_failure_evidence
    @ingest.ask_evidence = true
    ADT.report_handled(KeyError.new('key not found: "Alice Smith"'), symbol: 'Twilio#hold', reason: 'timeout')
    Devbench.flush!

    bundle = @ingest.wait_for { |r| r.method == 'PUT' }.first.json
    assert_equal ['handled_failure', 'Twilio#hold'], bundle.values_at('kind', 'template')
    assert_equal ['KeyError: key not found: <str> reason=timeout'], bundle['exemplars']
    refute bundle.key?('frames')
  end

  # Detail outlives the window that reported it, so a later ask is answered.
  def test_a_later_ask_is_still_answered
    ADT.report_handled(KeyError.new('k'), symbol: 'S#x')
    Devbench.flush!
    assert_empty @ingest.evidence

    @ingest.ask_evidence = true
    ADT.report_handled(KeyError.new('k'), symbol: 'S#x')
    Devbench.flush!
    refute_empty @ingest.wait_for { |r| r.method == 'PUT' }
  end
end

class DirectFailureTest < Minitest::Test
  include DirectSetup

  def test_a_dead_endpoint_never_raises_and_gives_up_quickly
    @ingest.close
    started = Process.clock_gettime(Process::CLOCK_MONOTONIC)
    ADT.capture_exception(raised(RuntimeError.new('x')))
    ADT.report_handled(KeyError.new('k'), symbol: 'S#x')
    refute Devbench.flush!(timeout: 3)
    assert_operator Process.clock_gettime(Process::CLOCK_MONOTONIC) - started, :<, 3.5

    # Discarded, not retried forever: the next flush has nothing to send.
    assert Devbench.flush!
  end

  def test_a_server_error_is_retried_once_then_discarded
    @ingest.flush_status = 500
    ADT.report_handled(KeyError.new('k'), symbol: 'S#x')
    refute Devbench.flush!
    assert_equal 2, @ingest.flushes.length

    @ingest.flush_status = 200
    assert Devbench.flush!
    assert_equal 2, @ingest.flushes.length, 'the discarded window was sent again'
  end

  def test_one_retry_recovers_a_transient_error
    calls = 0
    @ingest.flush_status = ->(_r) { (calls += 1) == 1 ? 503 : 200 }
    ADT.report_handled(KeyError.new('k'), symbol: 'S#x')
    assert Devbench.flush!
    assert_equal 2, @ingest.flushes.length
  end

  def test_a_rejected_key_is_not_retried
    @ingest.flush_status = 401
    ADT.report_handled(KeyError.new('k'), symbol: 'S#x')
    refute Devbench.flush!
    assert_equal 1, @ingest.flushes.length
  end

  # The application's thread never waits on the network: a flush stuck on a
  # slow ingest holds no lock a report needs.
  def test_reporting_does_not_wait_for_a_slow_ingest
    @ingest.flush_status = ->(_r) { sleep 1.5; 200 }
    ADT.report_handled(KeyError.new('k'), symbol: 'S#x')
    flusher = Thread.new { Devbench.flush! }
    sleep 0.2 # let the flush reach the slow ingest

    started = Process.clock_gettime(Process::CLOCK_MONOTONIC)
    50.times { |i| ADT.report_handled(KeyError.new('k'), symbol: "S#{i}#x") }
    elapsed = Process.clock_gettime(Process::CLOCK_MONOTONIC) - started
    assert_operator elapsed, :<, 0.5, "50 reports took #{elapsed}s while a flush was in flight"
    flusher.join
  end

  # Exactly one transport: with a DSN, the sidecar socket gets nothing.
  def test_direct_mode_never_also_writes_to_the_sidecar
    extend SidecarHelper
    start_sidecar
    configure(@ingest.dsn)
    ADT.report_handled(KeyError.new('k'), symbol: 'S#x')
    ADT.capture_exception(raised(RuntimeError.new('x')))
    assert Devbench.flush!
    assert_equal 2, @ingest.flushes.first.json['counts'].length
    assert_empty sidecar_messages
  ensure
    stop_sidecar
  end
end

class DirectSelfTest < Minitest::Test
  include DirectSetup

  def run_test!
    io = StringIO.new
    [Devbench.test!(io: io), io.string]
  end

  def test_a_working_dsn
    ok, out = run_test!
    assert ok, out
    assert_match(/HTTP 200: accepted — tenant "acme", environment "test"/, out)
    refute_includes out, 'adt_server_testkey', 'the key must never be printed'

    checks = @ingest.requests.select { |r| r.path == '/v1/check' }
    assert_equal 1, checks.size, 'one check request'
    assert_empty @ingest.flushes, 'a self-test must create nothing: no flush, so no fingerprint or issue'
    assert_empty @ingest.evidence
  end

  def test_a_rejected_key
    @ingest.flush_status = 401
    ok, out = run_test!
    refute ok
    assert_match(/HTTP 401: the key in DEVBENCH_DSN was rejected/, out)
  end

  def test_an_unreachable_host
    @ingest.close
    ok, out = run_test!
    refute ok
    assert_match(/could not reach http:\/\/127\.0\.0\.1:\d+: Errno::ECONNREFUSED/, out)
  end

  def test_no_dsn
    configure(nil)
    ok, out = run_test!
    refute ok
    assert_match(/DEVBENCH_DSN is not set/, out)
  end

  def test_a_bad_dsn
    configure('https://ingest.example.com')
    ok, out = run_test!
    refute ok
    assert_match(/DEVBENCH_DSN has no key/, out)
  end
end

# Process lifecycle, in real child processes: the background thread, the
# exit flush, and forking servers.
class DirectProcessTest < Minitest::Test
  LIB = File.expand_path('../lib', __dir__)

  def setup
    @ingest = FakeIngest.new
  end

  def teardown
    @ingest.close
  end

  def ruby(script, env = {})
    env = { 'DEVBENCH_DSN' => @ingest.dsn, 'DEVBENCH_SERVICE' => 'svc', 'DEVBENCH_ENABLED' => nil }.merge(env)
    out, err, status = Open3.capture3(env, RbConfig.ruby, '-w', '-I', LIB, '-e', script)
    assert status.success?, "subprocess failed:\n#{out}\n#{err}"
    assert_empty err.lines.grep(/#{Regexp.escape(LIB)}/), "warnings from the gem:\n#{err}"
    out
  end

  def test_the_exit_flush_sends_what_is_left
    ruby('require "devbench"; ADT.report_handled(KeyError.new("k"), symbol: "Exit#x")')
    assert_equal 1, @ingest.flushes.length
    assert_equal 'svc', @ingest.flushes.first.json['service']
  end

  def test_the_background_thread_flushes_on_its_interval
    ruby(<<~'RUBY')
      require "devbench"
      Devbench.configure { |c| c.flush_interval = 0.2 }
      ADT.report_handled(KeyError.new("k"), symbol: "Tick#x")
      sleep 1
      exit!(0) # skip the exit flush: only the thread can have sent it
    RUBY
    assert_equal 1, @ingest.flushes.length
  end

  # Puma/Unicorn/Sidekiq fork after boot. The child must report on its own
  # thread with its own sensor id, and must not re-send the parent's counts.
  def test_a_forked_child_reports_on_its_own
    out = ruby(<<~'RUBY')
      require "devbench"
      Devbench.configure { |c| c.flush_interval = 0.3 }
      ADT.report_handled(KeyError.new("k"), symbol: "Parent#before_fork")

      thread_child = fork do
        ADT.report_handled(KeyError.new("k"), symbol: "Child#thread")
        sleep 1.5
        exit!(0) # only the child's own background thread can have sent it
      end
      exit_child = fork do
        ADT.report_handled(KeyError.new("k"), symbol: "Child#exit")
        # a normal exit: the inherited at_exit flushes the child's counts
      end
      Process.wait(thread_child)
      Process.wait(exit_child)
      ADT.report_handled(KeyError.new("k"), symbol: "Parent#after_fork")
      print Devbench.flush!
    RUBY
    assert_equal 'true', out

    fp = lambda do |symbol|
      Devbench::Fingerprint.compute(kind: 'handled_failure', source: 'server', service: 'svc',
                                    type: 'KeyError', message: symbol)
    end
    by_fp = Hash.new { |h, k| h[k] = [] }
    @ingest.flushes.each do |f|
      f.json['counts'].each { |c| by_fp[c['fp']] << [f.json['sensor_id'], c['n']] }
    end

    %w[Parent#before_fork Child#thread Child#exit Parent#after_fork].each do |symbol|
      assert_equal 1, by_fp[fp.call(symbol)].length, "#{symbol} should arrive exactly once: #{by_fp.inspect}"
    end
    sensor = ->(symbol) { by_fp[fp.call(symbol)].first.first }
    assert_equal sensor.call('Parent#before_fork'), sensor.call('Parent#after_fork')
    refute_equal sensor.call('Parent#before_fork'), sensor.call('Child#thread')
    refute_equal sensor.call('Child#thread'), sensor.call('Child#exit')
  end
end
