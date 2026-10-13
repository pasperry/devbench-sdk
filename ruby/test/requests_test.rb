# frozen_string_literal: true

# Capability 2b (docs/server-sdk/failed-and-slow-requests.md) under a plain
# Rack app, no Rails: real HTTP over a real TCP socket (RackServer) into
# the real Devbench::Middleware, reports over a real unix socket
# (SidecarHelper) or to the real-HTTP ingest stand-in (FakeIngest). The
# only seam is RequestSignals.clock, pinned (TestClock) so a duration is
# exact; one test uses the real clock and a real 3.1 s sleep.
#
# Plain Rack has no route pattern, so every route here is `*`
# (test/rails_requests_test.rb has Controller#action).

require 'minitest/autorun'
require 'json'
require 'open3'
require 'rbconfig'
require 'devbench'
require_relative 'sidecar_helper'
require_relative 'ingest_helper'
require_relative 'rack_server_helper'

module RequestsTestApp
  CLOCK = TestClock.new

  # Each path is one behaviour; ?ms= advances the pinned clock by that much
  # inside the handler (the request "took" exactly that long).
  APP = lambda do |env|
    params = env['QUERY_STRING'].split('&').to_h { |kv| kv.split('=', 2) }
    CLOCK.advance(params['ms'].to_f) if params['ms']
    ADT.set_user(email: 'pat@example.com', account: 'dealer-1') if params['user']

    case env['PATH_INFO']
    when '/status'
      [params.fetch('code', '200').to_i, { 'content-type' => 'text/plain' }, ['ok']]
    when '/raise'
      raise ArgumentError, 'boom'
    when '/capture'
      ADT.capture_exception(ArgumentError.new('captured'), symbol: 'Plain#capture')
      [params.fetch('code', '503').to_i, {}, ['captured']]
    when '/handled'
      ADT.report_handled(KeyError.new('handled'), symbol: 'Plain#handled', reason: 'test')
      [params.fetch('code', '503').to_i, {}, ['handled']]
    when '/sse'
      [params.fetch('code', '200').to_i, { 'content-type' => 'Text/Event-Stream; charset=utf-8' },
       ["data: one\n\n", "data: two\n\n"]]
    when '/upgrade'
      [101, { 'upgrade' => 'websocket', 'connection' => 'upgrade' }, []]
    when '/partial-hijack'
      [200, { 'rack.hijack' => lambda { |io|
        io.write('hijacked')
        io.close
      } }, []]
    when '/full-hijack'
      io = env['rack.hijack'].call
      io.write("HTTP/1.1 200 OK\r\nconnection: close\r\n\r\nfull")
      io.close
      [200, {}, []]
    when '/sleep'
      sleep params['s'].to_f
      [200, {}, ['slept']]
    else
      [404, {}, ['not found']]
    end
  end
end

module RequestsTestSetup
  include SidecarHelper
  RS = Devbench::RequestSignals
  CLOCK = RequestsTestApp::CLOCK
  MINUTE = 1_758_799_980_000 # the start of a unix minute

  def setup
    Devbench.reset!
    Devbench::Current.clear
    start_sidecar
    CLOCK.wall_ms = MINUTE + 1000
    RS.clock = CLOCK
    @server = RackServer.new(stack)
  end

  def teardown
    @server&.close
    RS.clock = nil
    Devbench.reset!
    stop_sidecar
    Devbench::Current.clear
  end

  def stack
    Devbench::Middleware.new(RequestsTestApp::APP)
  end

  def get(path, headers: {})
    @server.request(:get, path, headers: headers)
  end

  def requests(messages)
    messages.select { |m| m['kind'] == 'request' }
  end
end

class RequestSignalsSidecarTest < Minitest::Test
  include RequestsTestSetup

  def test_a_503_without_raising_is_reported_once_as_failed
    res = @server.request(:post, '/status?code=503&ms=2004&user=1',
                          headers: { 'x-adt-trace' => 'v1/sessA/actA/0', 'content-type' => 'text/plain' }, body: '')

    assert_equal '503', res.code
    assert_equal 'ok', res.body, 'the response is untouched'
    msg = sidecar_message
    assert_equal({ 'v' => Devbench::PROTOCOL_VERSION, 'kind' => 'request', 'signal' => 'failed', 'method' => 'POST',
                   'route' => '*', 'status' => 503, 'duration_ms' => 2004, 'trace' => 'v1/sessA/actA/0',
                   'user' => { 'email' => 'pat@example.com', 'account' => 'dealer-1' } }, msg)
  end

  def test_an_exception_is_the_only_report_of_its_request
    assert_equal '500', get('/raise').code
    assert_equal '503', get('/capture').code

    messages = sidecar_messages
    assert_equal %w[exception exception], messages.map { |m| m['kind'] }, messages.inspect
  end

  # A slow request after a captured exception is still slow: slowness is a
  # different problem.
  def test_a_slow_2xx_after_a_captured_exception_is_still_slow
    get('/capture?code=200&ms=4000')

    messages = sidecar_messages
    assert_equal %w[exception request], messages.map { |m| m['kind'] }
    assert_equal 'slow', messages.last['signal']
  end

  # report_handled changes nothing here: its 5xx is still failed, its slow
  # request still slow, and the header is what it always was.
  def test_report_handled_changes_nothing
    res = get('/handled?code=503', headers: { 'x-adt-trace' => 'v1/sessH/actH/0' })
    assert_equal '1', res['x-adt-handled']
    get('/handled?code=200&ms=4000')

    messages = sidecar_messages
    assert_equal %w[handled_failure request handled_failure request], messages.map { |m| m['kind'] }
    assert_equal %w[failed slow], requests(messages).map { |m| m['signal'] }
  end

  def test_three_seconds_exactly_is_not_slow_and_one_ms_more_is
    get('/status?ms=3000')
    get('/status?ms=3001&code=201')
    get('/status?ms=3500&code=302')
    get('/status?ms=5000&code=404')

    msgs = requests(sidecar_messages)
    assert_equal [[201, 3001], [302, 3500], [404, 5000]], msgs.map { |m| [m['status'], m['duration_ms']] }
    assert(msgs.all? { |m| m['signal'] == 'slow' })
  end

  def test_fast_4xx_and_2xx_report_nothing
    get('/status?code=404&ms=5')
    get('/status?code=400&ms=5')
    get('/status?code=429&ms=5')
    get('/status?code=200&ms=5')
    get('/nowhere')

    assert_empty sidecar_messages
  end

  def test_a_streaming_response_is_never_slow_but_a_streaming_5xx_is_failed
    res = get('/sse?ms=60000')
    assert_equal "data: one\n\ndata: two\n\n", res.body
    get('/sse?ms=60000&code=500')

    msgs = sidecar_messages
    assert_equal 1, msgs.length, msgs.inspect
    assert_equal ['failed', 500, 60_000], [msgs[0]['signal'], msgs[0]['status'], msgs[0]['duration_ms']]
  end

  def test_upgrades_and_hijacked_connections_are_never_slow
    raw = @server.raw('GET', '/upgrade?ms=600000')
    assert_match %r{\AHTTP/1.1 101}, raw
    assert_match(/hijacked\z/, @server.raw('GET', '/partial-hijack?ms=600000'))
    assert_match(/full\z/, @server.raw('GET', '/full-hijack?ms=600000'))

    assert_empty sidecar_messages
  end

  # The real clock, no seam: a handler that really sleeps 3.1 s.
  def test_a_real_slow_request_is_slow
    RS.clock = nil
    get('/sleep?s=3.1')

    msg = sidecar_message
    assert_equal 'slow', msg['signal']
    assert_operator msg['duration_ms'], :>=, 3100
  end

  # Two instances in the stack (an app that inserted it twice): one
  # observation, and an exception captured inside the inner one still
  # suppresses the outer's failed.
  def test_nested_middleware_observes_once
    @server.close
    @server = RackServer.new(Devbench::Middleware.new(stack))
    get('/status?code=503')
    get('/capture?code=503')

    assert_equal %w[request exception], sidecar_messages.map { |m| m['kind'] }
  end

  def test_disabled_observes_nothing
    Devbench.configure { |c| c.enabled = false }
    11.times { get('/status?code=429') }
    get('/status?code=503')

    assert_empty sidecar_messages
    assert_empty RS.detector.finish, 'nothing was tallied'
  end

  # ---- 429 spikes

  def test_a_429_spike_is_reported_once_when_the_next_minute_starts
    10.times { get('/status?code=429&user=1', headers: { 'x-adt-trace' => 'v1/sessS/actS/0' }) }
    assert_empty sidecar_messages, 'nothing until the window closes'

    # The closing request has a trace and a user of its own; neither may
    # ride on the spike.
    CLOCK.wall_ms = MINUTE + 60_000
    get('/status?code=200&user=1', headers: { 'x-adt-trace' => 'v1/sessT/actT/0' })
    get('/status?code=200')

    msg = sidecar_message
    assert_equal({ 'v' => Devbench::PROTOCOL_VERSION, 'kind' => 'request', 'signal' => 'rate_limit_spike',
                   'method' => 'GET', 'route' => '*', 'status' => 429, 'count' => 10, 'total' => 10 }, msg,
                 'no trace and no user on an aggregate')
  end

  def test_nine_429s_or_under_ten_percent_is_no_spike
    9.times { get('/status?code=429') }
    CLOCK.wall_ms = MINUTE + 60_000
    10.times { get('/status?code=429') }
    91.times { get('/status?code=200') }
    CLOCK.wall_ms = MINUTE + 120_000
    get('/status')

    assert_empty sidecar_messages
  end

  # Sidecar mode has no flush thread: Devbench.flush! closes a window the
  # clock has left, and a new configuration ends the open one.
  def test_flush_and_configure_close_the_window
    10.times { get('/status?code=429') }
    assert Devbench.flush!
    assert_empty sidecar_messages, 'a flush inside the window reports nothing'

    CLOCK.wall_ms = MINUTE + 60_000
    Devbench.flush!
    assert_equal [10], sidecar_messages.map { |m| m['count'] }

    12.times { get('/status?code=429') }
    Devbench.configure { |c| c.service = 'other' }
    assert_equal [12], sidecar_messages.map { |m| m['count'] }
  end

  # Every Puma thread observes; the counts must still be exact.
  def test_concurrent_requests_count_exactly
    threads = 8.times.map do
      Thread.new { 25.times { get('/status?code=429') } }
    end
    flusher = Thread.new { 50.times { Devbench.flush! } }
    threads.each(&:join)
    flusher.join

    CLOCK.wall_ms = MINUTE + 60_000
    Devbench.flush!
    msgs = sidecar_messages
    assert_equal 1, msgs.length, msgs.inspect
    assert_equal [200, 200], [msgs[0]['count'], msgs[0]['total']]
  end

  # ---- Never harms the host

  class ExplodingClock
    def monotonic_ms
      1.0
    end

    def wall_ms
      raise 'clock broke'
    end
  end

  def test_an_instrumentation_failure_never_reaches_the_app
    RS.clock = ExplodingClock.new
    res = get('/status?code=503')
    assert_equal ['503', 'ok'], [res.code, res.body]

    res = @server.request(:get, '/status?code=200', headers: { 'x-adt-trace' => 'not a trace' })
    assert_equal '200', res.code
  end
end

# The open window is evaluated at process exit, in both transports: a real
# child process, its real at_exit.
class RequestSignalsExitTest < Minitest::Test
  include SidecarHelper

  LIB = File.expand_path('../lib', __dir__)

  def child(env)
    script = <<~RUBY
      require 'devbench'
      app = Devbench::Middleware.new(->(_env) { [429, {}, ['slow down']] })
      11.times { app.call('REQUEST_METHOD' => 'DELETE', 'PATH_INFO' => '/r/7', 'QUERY_STRING' => '') }
      app.call('REQUEST_METHOD' => 'DELETE', 'PATH_INFO' => '/r/8', 'QUERY_STRING' => '')
    RUBY
    _out, err, status = Open3.capture3(env, RbConfig.ruby, '-I', LIB, '-e', script)
    assert status.success?, err
  end

  def test_sidecar_mode_reports_the_open_window_at_exit
    start_sidecar
    child('ADT_SIDECAR_SOCKET' => @sidecar_path, 'DEVBENCH_DSN' => nil, 'ADT_DSN' => nil)

    msg = sidecar_message
    assert_equal ['rate_limit_spike', 'DELETE', '*', 12, 12], msg.values_at('signal', 'method', 'route', 'count', 'total')
  ensure
    stop_sidecar
  end

  def test_direct_mode_flushes_the_open_window_at_exit
    ingest = FakeIngest.new
    child('DEVBENCH_DSN' => ingest.dsn, 'DEVBENCH_SERVICE' => 'inventory', 'DEVBENCH_FLUSH_INTERVAL' => nil)

    fp = Devbench::Fingerprint.compute(kind: 'error', source: 'server', service: 'inventory',
                                       type: 'http_429_spike', message: 'DELETE *')
    counts = ingest.flushes.flat_map { |f| f.json['counts'] }
    assert_equal [[fp, 1, 'error']], counts.map { |c| c.values_at('fp', 'n', 'kind') }
  ensure
    ingest&.close
  end
end

# Direct mode: the same signals counted in-process with the vectors'
# fingerprints, kinds, template text and exemplar.
class RequestSignalsDirectTest < Minitest::Test
  include RequestsTestSetup
  SERVICE = 'app'

  def setup
    super
    @ingest = FakeIngest.new
    configure
  end

  def teardown
    super
    @ingest&.close
  end

  def configure(interval: 3600)
    Devbench.configure do |c|
      c.dsn = @ingest.dsn
      c.service = SERVICE
      c.flush_interval = interval
    end
  end

  def egress(line)
    Devbench::Scrub.egress(line, nil, SERVICE)
  end

  def fp(kind, type, message)
    Devbench::Fingerprint.compute(kind: kind, source: 'server', service: SERVICE, type: type, message: message)
  end

  def test_the_flush_carries_the_vector_fingerprints_and_kinds
    @ingest.ask_evidence = true
    get('/status?code=500&ms=7&user=1')
    get('/status?code=200&ms=4120')
    10.times { get('/status?code=429') }
    CLOCK.wall_ms = MINUTE + 60_000
    assert Devbench.flush!

    counts = @ingest.flushes.flat_map { |f| f.json['counts'] }.to_h { |c| [c['fp'], c] }
    # testdata/request_signal_vectors.json, build "no route pattern".
    failed = 'de0d7dae7f4931a5ae104306100250f74c6064beb1bb8d793028c805af50b070'
    assert_equal failed, fp('error', 'http_500', 'GET *')
    slow = fp('degradation', 'slow_request', 'GET *')
    spike = fp('error', 'http_429_spike', 'GET *')
    assert_equal({ failed => 'error', slow => 'degradation', spike => 'error' },
                 counts.transform_values { |c| c['kind'] })
    assert_equal [{ 'email' => 'pat@example.com', 'account' => 'dealer-1' }], counts[failed]['users']
    assert_nil counts[spike]['users']

    @ingest.wait_for(3) { |r| r.method == 'PUT' }
    bundles = @ingest.evidence.to_h { |r| [r.json['fp'], r.json] }
    assert_equal 'http_500 at GET *', bundles[failed]['template']
    # The raw exemplar, scrubbed at egress like an exception's message (the
    # durations template to <dur>).
    assert_equal [egress('http_500: GET * in 7 ms')], bundles[failed]['exemplars']
    assert_equal [egress('slow_request: GET * in 4120 ms (status 200)')], bundles[slow]['exemplars']
    assert_equal [egress('http_429_spike: GET * 10 of 10 requests answered 429 in one minute')],
                 bundles[spike]['exemplars']
    assert_equal 'http_500: GET * in <dur>', bundles[failed]['exemplars'][0]
  end

  # A process whose only traffic is 429s: the first one starts the flush
  # thread, whose own flush closes the window. No Devbench.flush!.
  def test_the_background_flush_closes_the_spike_window
    configure(interval: 0.3)
    10.times { get('/status?code=429') }
    CLOCK.wall_ms = MINUTE + 60_000

    spike = fp('error', 'http_429_spike', 'GET *')
    found = @ingest.wait_for(1, timeout: 10) do |r|
      r.path == '/v1/flush' && r.json['counts'].any? { |c| c['fp'] == spike }
    end
    refute_empty found, 'the background flush never reported the spike'
  end
end
