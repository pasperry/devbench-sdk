# frozen_string_literal: true

# Capability 2b (docs/server-sdk/failed-and-slow-requests.md) under a real
# Rails application: real HTTP over a real TCP socket (RackServer) into the
# real Rails stack with the README's middleware wiring, reports over a real
# unix socket (SidecarHelper) or to the real-HTTP ingest stand-in
# (FakeIngest). The only seam is RequestSignals.clock (TestClock), so a
# duration is exact. The route is Rails' Controller#action.
#
# Rails is a test-only dependency: where it is not installed these tests
# skip, unless ADT_REQUIRE_RAILS is set (CI), which makes that a failure.

require 'minitest/autorun'
require 'json'

RAILS_LOAD_ERROR = begin
  require 'rails'
  require 'action_controller/railtie'
  nil
rescue LoadError => e
  e
end

require 'adt'
require_relative 'sidecar_helper'
require_relative 'ingest_helper'
require_relative 'rack_server_helper'

REQUESTS_CLOCK = TestClock.new
require_relative 'support/requests_rails_app' if RAILS_LOAD_ERROR.nil?

module RailsRequestsSetup
  include SidecarHelper
  RS = Devbench::RequestSignals
  MINUTE = 1_758_799_980_000

  def setup
    if RAILS_LOAD_ERROR
      flunk "Rails is required here but did not load: #{RAILS_LOAD_ERROR.message}" if ENV['ADT_REQUIRE_RAILS']
      skip "Rails not installed (#{RAILS_LOAD_ERROR.message}); request signals under Rails are untested in this run"
    end
    Devbench.reset!
    Devbench::Current.clear
    start_sidecar
    REQUESTS_CLOCK.wall_ms = MINUTE + 1000
    RS.clock = REQUESTS_CLOCK
    @server = RackServer.new(AdtRequestsTestApp)
  end

  def teardown
    @server&.close
    RS.clock = nil
    Devbench.reset!
    stop_sidecar
    Devbench::Current.clear
  end

  def call(method, path, headers: {})
    body = method == :put ? '' : nil
    headers = headers.merge('content-type' => 'text/plain') if body
    @server.request(method, path, headers: headers, body: body)
  end

  def requests(messages)
    messages.select { |m| m['kind'] == 'request' }
  end
end

class RailsRequestSignalsTest < Minitest::Test
  include RailsRequestsSetup

  def test_a_503_without_raising_is_failed_under_controller_action
    res = call(:put, '/deals/9?code=503&user=1', headers: { 'x-adt-trace' => 'v1/sessR/actR/0' })

    assert_equal '503', res.code
    msg = sidecar_message
    assert_equal ['request', 'failed', 'PUT', 'DealsController#update', 503, 'v1/sessR/actR/0'],
                 msg.values_at('kind', 'signal', 'method', 'route', 'status', 'trace')
    assert_equal({ 'email' => 'pat@example.com', 'account' => 'dealer-1' }, msg['user'])
    refute_includes JSON.generate(msg), '/deals/9', 'never the raw path'
  end

  # Rails renders the 500 page itself (action_dispatch.exception): the
  # exception is the report, once, and nothing more.
  def test_a_raising_action_is_reported_only_as_the_exception
    assert_equal '500', call(:get, '/raises').code
    assert_equal '500', call(:get, '/reported').code

    messages = sidecar_messages
    assert_equal %w[exception exception], messages.map { |m| m['kind'] }, messages.inspect
  end

  # An exception the app told Dev Bench to ignore must not come back as an
  # http_500.
  def test_an_ignored_exception_rendered_as_500_reports_nothing
    Devbench.ignore_exceptions << 'IgnoredByTheAppError'
    assert_equal '500', call(:get, '/ignored').code

    assert_empty sidecar_messages
  ensure
    Devbench.ignore_exceptions.delete('IgnoredByTheAppError')
  end

  def test_report_handled_then_a_5xx_is_still_failed
    res = call(:get, '/handled?code=502', headers: { 'x-adt-trace' => 'v1/sessH/actH/0' })
    assert_equal '1', res['x-adt-handled'], 'the header is unchanged'

    messages = sidecar_messages
    assert_equal %w[handled_failure request], messages.map { |m| m['kind'] }
    assert_equal ['failed', 502, 'DealsController#handled'], messages[1].values_at('signal', 'status', 'route')
  end

  def test_slow_is_strictly_more_than_three_seconds
    call(:get, '/ok?ms=3000')
    call(:get, '/ok?ms=3001')

    msg = sidecar_message
    assert_equal ['slow', 'GET', 'DealsController#ok', 200, 3001],
                 msg.values_at('signal', 'method', 'route', 'status', 'duration_ms')
  end

  # ActionController::Live: the response's duration is the stream's.
  def test_a_live_event_stream_is_never_slow_but_its_5xx_is_failed
    res = call(:get, '/events?ms=60000')
    assert_equal '200', res.code
    assert_includes res.body, "data: one\n\n"
    call(:get, '/events?ms=60000&code=503')

    msgs = sidecar_messages
    assert_equal [['failed', 503, 'StreamsController#events']], msgs.map { |m| m.values_at('signal', 'status', 'route') }
  end

  def test_an_unmatched_route_counts_under_star
    assert_equal '404', call(:get, '/nowhere/123?ms=4000').code

    msg = sidecar_message
    assert_equal %w[slow *], msg.values_at('signal', 'route')
  end

  def test_429s_on_one_action_spike_once_per_window
    10.times { call(:get, '/limited', headers: { 'x-adt-trace' => 'v1/sessS/actS/0' }) }
    call(:get, '/ok')
    REQUESTS_CLOCK.wall_ms = MINUTE + 60_000
    call(:get, '/ok')
    REQUESTS_CLOCK.wall_ms = MINUTE + 120_000
    call(:get, '/ok')

    msg = sidecar_message
    assert_equal({ 'v' => Devbench::PROTOCOL_VERSION, 'kind' => 'request', 'signal' => 'rate_limit_spike',
                   'method' => 'GET', 'route' => 'DealsController#limited', 'status' => 429,
                   'count' => 10, 'total' => 10 }, msg)
  end
end

class RailsRequestSignalsDirectTest < Minitest::Test
  include RailsRequestsSetup

  def setup
    super
    @ingest = FakeIngest.new
    Devbench.configure do |c|
      c.dsn = @ingest.dsn
      c.service = 'rails-api'
      c.flush_interval = 3600
    end
  end

  def teardown
    super
    @ingest&.close
  end

  # testdata/request_signal_vectors.json, build "rails failed 500".
  def test_a_rails_500_without_raising_counts_the_vector_fingerprint
    call(:put, '/deals/7?code=500&user=1')
    call(:get, '/ok?ms=4120')
    assert Devbench.flush!

    counts = @ingest.flushes.flat_map { |f| f.json['counts'] }.to_h { |c| [c['fp'], c] }
    failed = 'edcff75da6707047170e9b7c34e5a3863512aa8132f44c45d2ecfd98c89924a4'
    slow = Devbench::Fingerprint.compute(kind: 'degradation', source: 'server', service: 'rails-api',
                                         type: 'slow_request', message: 'GET DealsController#ok')
    assert_equal({ failed => 'error', slow => 'degradation' }, counts.transform_values { |c| c['kind'] })
    assert_equal [{ 'email' => 'pat@example.com', 'account' => 'dealer-1' }], counts[failed]['users']
  end
end
