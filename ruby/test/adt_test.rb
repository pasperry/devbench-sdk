# frozen_string_literal: true

require 'minitest/autorun'
require 'json'
require 'socket'
require 'tmpdir'
require 'adt'
require_relative 'sidecar_helper'

class TraceTest < Minitest::Test
  # These must match the Go SDK and the sidecar's extraction regex exactly. A
  # trace the browser writes and Rails mangles is worse than no trace: the
  # request still succeeds, so nothing looks wrong, and the evidence simply
  # cannot be joined.
  def test_parses_a_valid_trace
    t = ADT::Trace.parse('v1/sess_A-1/act_B-2/3')
    refute_nil t
    assert_equal 'sess_A-1', t.session
    assert_equal 'act_B-2', t.intent
    assert_equal 3, t.hop
    assert_equal 'sess_A-1/act_B-2', t.key
  end

  def test_round_trips
    original = 'v1/abc/def/7'
    assert_equal original, ADT::Trace.parse(original).to_s
  end

  def test_rejects_the_same_malformed_values_as_the_go_sdk
    [
      '', 'garbage', 'v2/a/b/0', 'v1/a/b', 'v1/a/b/c', 'v1//b/0',
      'v1/a/b/-1', 'v1/a/b/1000', "v1/#{'x' * 65}/b/0",
      'v1/a b/c/0', 'v1/a/b/0/extra', nil, 42
    ].each do |bad|
      assert_nil ADT::Trace.parse(bad), "accepted malformed trace: #{bad.inspect}"
    end
  end

  def test_next_hop_increments
    assert_equal 'v1/a/b/4', ADT::Trace.parse('v1/a/b/3').next_hop.to_s
  end

  # A request that has crossed a hundred services is a routing loop, and
  # continuing to propagate would help it along.
  def test_next_hop_stops_at_the_limit
    assert_nil ADT::Trace.parse('v1/a/b/99').next_hop
  end
end

class MiddlewareTest < Minitest::Test
  def test_binds_the_trace_for_the_request
    seen = nil
    app = ->(_env) { seen = ADT::Current.trace; [200, {}, ['ok']] }

    status, = ADT::Middleware.new(app).call('HTTP_X_ADT_TRACE' => 'v1/sessA/actB/0')

    assert_equal 200, status
    refute_nil seen
    assert_equal 'sessA/actB', seen.key
  end

  # Servers reuse threads. Leaving a trace behind would attribute one user's
  # next request to a previous user's action.
  def test_does_not_leak_between_requests
    app = ->(_env) { [200, {}, ['ok']] }
    mw = ADT::Middleware.new(app)

    mw.call('HTTP_X_ADT_TRACE' => 'v1/sessA/actB/0')
    assert_nil ADT::Current.trace, 'the trace outlived its request'

    mw.call({})
    assert_nil ADT::Current.trace
  end

  # ADT must never fail a customer's request.
  def test_a_malformed_header_is_ignored_not_rejected
    served = false
    app = lambda do |_env|
      served = true
      assert_nil ADT::Current.trace
      [200, {}, ['ok']]
    end

    status, = ADT::Middleware.new(app).call('HTTP_X_ADT_TRACE' => 'total garbage')

    assert served
    assert_equal 200, status
  end

  def test_no_header_is_fine
    status, = ADT::Middleware.new(->(_e) { [200, {}, ['ok']] }).call({})
    assert_equal 200, status
  end
end

class ForwardingTest < Minitest::Test
  def teardown
    ADT::Current.clear
  end

  # The capability people forget. Accepting is easy; forwarding is what breaks,
  # and it breaks silently.
  def test_forwards_with_the_hop_incremented
    ADT::Current.with(ADT::Trace.parse('v1/sessA/actB/0')) do
      assert_equal({ 'x-adt-trace' => 'v1/sessA/actB/1' }, ADT::HTTP.headers)
    end
  end

  def test_no_trace_means_no_header
    assert_empty ADT::HTTP.headers
  end

  def test_merge_does_not_mutate_the_callers_hash
    original = { 'content-type' => 'application/json' }.freeze

    ADT::Current.with(ADT::Trace.parse('v1/a/b/0')) do
      merged = ADT::HTTP.merge(original)
      assert_equal 'application/json', merged['content-type']
      assert_equal 'v1/a/b/1', merged['x-adt-trace']
    end

    assert_equal({ 'content-type' => 'application/json' }, original)
  end
end

class ReporterTest < Minitest::Test
  def teardown
    ADT::Current.clear
    ADT::Reporter.socket_path = nil
  end

  # A real unix socket, not a stub: this is the wire contract with the sidecar.
  def test_sends_a_handled_failure_over_the_socket
    Dir.mktmpdir do |dir|
      path = File.join(dir, 'sidecar.sock')
      server = UNIXServer.new(path)
      ADT::Reporter.socket_path = path

      received = nil
      reader = Thread.new do
        conn = server.accept
        received = conn.gets
        conn.close
      end

      ADT::Current.with(ADT::Trace.parse('v1/sessQ/actR/0')) do
        ADT.report_handled(ArgumentError.new('email taken'),
                           symbol: 'CustomersController#update', reason: 'validation')
      end

      reader.join(3)
      server.close

      refute_nil received, 'nothing reached the sidecar socket'
      payload = JSON.parse(received)
      assert_equal 'handled_failure', payload['kind']
      assert_equal 'CustomersController#update', payload['symbol']
      assert_equal 'ArgumentError', payload['error']
      assert_equal 'email taken', payload['message']
      assert_equal 'v1/sessQ/actR/0', payload['trace']
    end
  end

  # Called from inside rescue blocks in the request path. A diagnostics call
  # that raises there turns a handled failure into an outage.
  def test_never_raises_when_the_sidecar_is_absent
    ADT::Reporter.socket_path = '/nonexistent/definitely/not/here.sock'

    ADT.report_handled(RuntimeError.new('boom'), symbol: 'X#y')
    ADT.report_handled(RuntimeError.new('boom'), symbol: 'X#y', reason: 'r')
    # Reaching here without raising is the assertion.
    assert true
  end

  def test_truncates_an_enormous_message
    Dir.mktmpdir do |dir|
      path = File.join(dir, 'sidecar.sock')
      server = UNIXServer.new(path)
      ADT::Reporter.socket_path = path

      received = nil
      reader = Thread.new do
        conn = server.accept
        received = conn.gets
        conn.close
      end

      ADT.report_handled(RuntimeError.new('x' * 10_000), symbol: 'X#y')

      reader.join(3)
      server.close

      payload = JSON.parse(received)
      assert_operator payload['message'].length, :<=, 501
    end
  end

  # A real exception class, not a stub: application errors with a broken
  # #message exist in the wild, and this call sits inside their rescue blocks
  # (e.g. Twilio webhooks). A raise here means the rescue's
  # render never runs.
  class MessageRaises < StandardError
    def message
      raise NoMethodError, 'undefined method for nil'
    end
  end

  def test_never_raises_when_the_error_itself_misbehaves
    ADT::Reporter.socket_path = '/nonexistent/definitely/not/here.sock'

    assert_nil ADT.report_handled(MessageRaises.new, symbol: 'X#y')
    assert_equal 1, ADT::Current.handled_count,
                 'the browser must still be told a failure was handled'
  end

  # Losing the message must not lose the report: class and site are the
  # fingerprint, the message is detail.
  def test_reports_without_the_message_when_the_message_raises
    Dir.mktmpdir do |dir|
      path = File.join(dir, 'sidecar.sock')
      server = UNIXServer.new(path)
      ADT::Reporter.socket_path = path

      received = nil
      reader = Thread.new do
        conn = server.accept
        received = conn.gets
        conn.close
      end

      ADT.report_handled(MessageRaises.new, symbol: 'TwilioController#hold')

      reader.join(3)
      server.close

      refute_nil received, 'nothing reached the sidecar socket'
      payload = JSON.parse(received)
      assert_equal 'ReporterTest::MessageRaises', payload['error']
      assert_equal 'TwilioController#hold', payload['symbol']
      refute payload.key?('message')
    end
  end
end

class LoadTest < Minitest::Test
  # VERSION was defined in both adt.rb and adt/version.rb: a warning on every
  # boot of the customer's app, and a silent winner by load order if the two
  # ever diverged.
  def test_loads_without_warnings
    require 'open3'
    lib = File.expand_path('../lib', __dir__)
    # Bundler evaluates the gemspec (which loads adt/version) before the app
    # requires the gem. That order is what produced the warning.
    script = 'require "devbench/version"; require "devbench"; print ADT::VERSION'
    _out, err, status = Open3.capture3(RbConfig.ruby, '-w', '-I', lib, '-e', script)
    assert status.success?, err
    assert_empty err.lines.grep(/#{Regexp.escape(lib)}/), "warnings from the gem:\n#{err}"
  end

  # Existing installs call ADT.set_user, ADT.report_handled,
  # ADT.capture_exception and name ADT::Middleware / ADT::SidekiqHooks. The
  # rename must not move a single one of them.
  def test_the_old_names_still_work
    require 'open3'
    lib = File.expand_path('../lib', __dir__)
    script = <<~'RUBY'
      require "adt"
      print [ADT.equal?(Devbench), ADT::Middleware.equal?(Devbench::Middleware),
             ADT::SidekiqHooks.equal?(Devbench::SidekiqHooks),
             %i[set_user report_handled capture_exception ignore_exceptions].all? { |m| ADT.respond_to?(m) },
             ADT::VERSION].join(" ")
    RUBY
    out, err, status = Open3.capture3(RbConfig.ruby, '-w', '-I', lib, '-e', script)
    assert status.success?, err
    assert_equal "true true true true #{Devbench::VERSION}", out
    assert_empty err.lines.grep(/#{Regexp.escape(lib)}/), "warnings from the gem:\n#{err}"
  end

  def test_require_devbench_defines_both_names
    require 'open3'
    lib = File.expand_path('../lib', __dir__)
    out, err, status = Open3.capture3(RbConfig.ruby, '-w', '-I', lib, '-e',
                                      'require "devbench"; print defined?(ADT::Middleware).inspect')
    assert status.success?, err
    assert_equal '"constant"', out
  end
end

# Detector 2's server half.
#
# The browser already knows whether the application showed the user an error.
# It does not know whether anything went wrong. One response header closes
# that gap, and the join then happens in the browser, where both facts are
# present at the same moment.
class HandledHeaderTest < Minitest::Test
  def setup
    ADT::Current.clear
  end

  def teardown
    ADT::Current.clear
    ADT::Reporter.socket_path = nil
  end

  # No sidecar is listening in tests. That is deliberate: the count must be
  # recorded even when reporting fails, or a sidecar that is down would also
  # make the failure invisible to the detector that does not need it.
  def app_that_handles(times)
    lambda do |_env|
      times.times do
        ADT.report_handled(StandardError.new('boom'), symbol: 'Thing#do', reason: 'validation')
      end
      [200, { 'content-type' => 'application/json' }, ['{"ok":true}']]
    end
  end

  def env_with_trace
    { ADT::Trace::RACK_HEADER => 'v1/sessA/act1/0' }
  end

  def test_successful_response_that_swallowed_a_failure_is_marked
    mw = ADT::Middleware.new(app_that_handles(1))
    status, headers, = mw.call(env_with_trace)

    assert_equal 200, status
    assert_equal '1', headers['x-adt-handled'],
                 'a 200 that rescued an exception must say so, or the browser cannot tell'
  end

  def test_count_is_the_only_detail_disclosed
    mw = ADT::Middleware.new(app_that_handles(3))
    _, headers, = mw.call(env_with_trace)

    assert_equal '3', headers['x-adt-handled']

    # Readable by any script on the page, so it carries no class name, no
    # message and no symbol. Those travel to the sidecar over a unix socket on
    # the customer's own host.
    joined = headers.to_a.flatten.join(' ')
    refute_includes joined, 'StandardError'
    refute_includes joined, 'boom'
    refute_includes joined, 'Thing#do'
  end

  # A browser cannot read a custom response header cross-origin unless the
  # server lists it. A common setup is an Angular app on a different origin from
  # its Rails API, so without this the detector sees nothing — and would fail
  # silently, which is the one failure mode this product cannot have.
  def test_header_is_exposed_for_cross_origin_readers
    mw = ADT::Middleware.new(app_that_handles(1))
    _, headers, = mw.call(env_with_trace)

    assert_includes headers['Access-Control-Expose-Headers'].to_s.downcase, 'x-adt-handled'
  end

  def test_existing_exposed_headers_are_preserved
    app = lambda do |_env|
      ADT.report_handled(StandardError.new('boom'), symbol: 'Thing#do')
      [200, { 'Access-Control-Expose-Headers' => 'x-request-id' }, ['']]
    end

    _, headers, = ADT::Middleware.new(app).call(env_with_trace)

    exposed = headers['Access-Control-Expose-Headers'].to_s
    assert_includes exposed, 'x-request-id', 'clobbered the application\'s own exposed headers'
    assert_includes exposed.downcase, 'x-adt-handled'
  end

  def test_clean_response_carries_no_header
    app = ->(_env) { [200, {}, ['']] }
    _, headers, = ADT::Middleware.new(app).call(env_with_trace)

    refute headers.key?('x-adt-handled'),
           'a request that handled nothing must not look like one that did'
  end

  # Servers reuse threads. A count carried across requests would mark an
  # innocent response as having swallowed the previous request's failure.
  def test_count_does_not_leak_into_the_next_request
    mw = ADT::Middleware.new(app_that_handles(2))
    _, dirty, = mw.call(env_with_trace)
    assert_equal '2', dirty['x-adt-handled']

    clean = ADT::Middleware.new(->(_env) { [200, {}, ['']] })
    _, headers, = clean.call(env_with_trace)

    refute headers.key?('x-adt-handled'),
           'the previous request\'s handled count leaked into this one'
  end

  # A diagnostics middleware that can fail a request is worse than no
  # diagnostics.
  def test_a_frozen_header_hash_does_not_break_the_response
    app = lambda do |_env|
      ADT.report_handled(StandardError.new('boom'), symbol: 'Thing#do')
      [200, { 'content-type' => 'text/plain' }.freeze, ['ok']]
    end

    status, _, body = ADT::Middleware.new(app).call(env_with_trace)
    assert_equal 200, status
    assert_equal ['ok'], body
  end
end

# Capability 5. Who was affected travels in its own field, on every message.
class IdentityTest < Minitest::Test
  include SidecarHelper

  def setup
    ADT::Current.clear
    start_sidecar
  end

  def teardown
    stop_sidecar
    ADT::Current.clear
  end

  def test_handled_failure_carries_the_normalised_user
    ADT.set_user(email: "  Pat@Example.COM\n", account: 1182)
    ADT.report_handled(ArgumentError.new('x'), symbol: 'X#y')

    assert_equal({ 'email' => 'pat@example.com', 'account' => '1182' }, sidecar_message['user'])
  end

  def test_user_is_omitted_when_not_set
    ADT.report_handled(ArgumentError.new('x'), symbol: 'X#y')

    refute sidecar_message.key?('user'), 'an absent identity must be omitted, not sent empty'
  end

  def test_empty_fields_are_omitted
    ADT.set_user(email: '   ', account: nil)
    ADT.report_handled(ArgumentError.new('x'), symbol: 'X#y')
    ADT.set_user(account: 'acct-1')
    ADT.report_handled(ArgumentError.new('x'), symbol: 'X#y')

    first, second = sidecar_messages
    refute first.key?('user')
    assert_equal({ 'account' => 'acct-1' }, second['user'])
  end

  # Longer is truncated, not rejected: losing who was affected over a long
  # address would be worse than keeping 255 characters of it.
  def test_fields_are_truncated_to_255
    ADT.set_user(email: "#{'a' * 300}@x.com", account: 'b' * 300)
    ADT.report_handled(ArgumentError.new('x'), symbol: 'X#y')

    user = sidecar_message['user']
    assert_equal 'a' * 255, user['email']
    assert_equal 'b' * 255, user['account']
  end

  class Unprintable
    def to_s
      raise 'no'
    end
  end

  # Called from a before_action: a raise there fails the request.
  def test_set_user_never_raises
    assert_nil ADT.set_user(email: 'pat@example.com', account: Unprintable.new)
    assert_equal({ email: 'pat@example.com' }, ADT::Current.user)
    assert_nil ADT.set_user(email: "\xFF\xFEpat".b)
  end

  # Servers reuse threads. One user's identity on the next user's failure
  # would be a wrong answer to the first question triage asks.
  def test_middleware_clears_the_user_at_the_end_of_the_request
    setter = lambda do |_env|
      ADT.set_user(email: 'first@example.com')
      [200, {}, ['ok']]
    end
    ADT::Middleware.new(setter).call(ADT::Trace::RACK_HEADER => 'v1/s/a/0')
    assert_nil ADT::Current.user, 'the user outlived the request (traced)'

    ADT::Middleware.new(setter).call({})
    assert_nil ADT::Current.user, 'the user outlived the request (untraced)'

    reporter = lambda do |_env|
      ADT.report_handled(StandardError.new('boom'), symbol: 'X#y')
      [200, {}, ['ok']]
    end
    ADT::Middleware.new(reporter).call({})

    refute sidecar_message.key?('user'), 'the next request inherited the previous user'
  end

  # Something outside a request (a console, a job runner) may have left an
  # identity on the thread. A request must not start with it.
  def test_a_request_does_not_inherit_a_stale_user
    ADT.set_user(email: 'stale@example.com')
    seen = :unset
    ADT::Middleware.new(->(_env) { seen = ADT::Current.user; [200, {}, ['']] }).call({})

    assert_nil seen
  end
end
