# frozen_string_literal: true

# An ActiveJob job's log lines in direct mode (#356): a real in-process Rails
# application (test/support/active_job_app.rb) booted with only DEVBENCH_DSN
# set, real ActiveJob adapters, a real HTTP server standing in for ingest.
# The job's lines — its own and ActiveJob's "Performing"/"Error performing"
# — are kept under the trace of the request that enqueued it, on the
# adapter's worker thread, and delivered when ingest asks for that trace.
#
# Rails is test-only; without it these skip, unless ADT_REQUIRE_RAILS is set
# (CI sets it).

require 'minitest/autorun'
require 'logger'
require_relative 'ingest_helper'

AJ_LOGS_LOAD_ERROR = begin
  require 'rails'
  require 'action_controller/railtie'
  require 'active_job/railtie'
  nil
rescue LoadError => e
  e
end

AJ_LOGS_INGEST = FakeIngest.new
%w[DEVBENCH_DSN ADT_DSN DEVBENCH_SERVICE DEVBENCH_ENABLED].each { |k| ENV.delete(k) }
ENV['DEVBENCH_DSN'] = AJ_LOGS_INGEST.pair_dsn('adt_client_aj', 'adt_server_aj')

require 'devbench'
require_relative 'support/active_job_app' if AJ_LOGS_LOAD_ERROR.nil?

class ActiveJobLogsTest < Minitest::Test
  def setup
    if AJ_LOGS_LOAD_ERROR
      flunk "Rails is required here but did not load: #{AJ_LOGS_LOAD_ERROR.message}" if ENV['ADT_REQUIRE_RAILS']
      skip "Rails not installed (#{AJ_LOGS_LOAD_ERROR.message})"
    end
    Devbench::Current.clear
    Devbench::Logs.buffer.clear
    JobProbe.clear
  end

  def teardown
    Devbench::Current.clear
  end

  def request(method, path, headers = {})
    env = Rack::MockRequest.env_for(path, { method: method }.merge(headers))
    status, headers, body = DevbenchActiveJobTestApp.call(env)
    body.close if body.respond_to?(:close)
    [status, headers, body]
  end

  # The job's lines are written on the adapter's thread after the request
  # returned; ActiveJob's "Error performing" line is its last.
  def await_lines(key, marker, timeout: 5)
    deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + timeout
    loop do
      lines = Devbench::Logs.lookup(key, 1000)
      return lines if lines.any? { |l| l.include?(marker) }
      return lines if Process.clock_gettime(Process::CLOCK_MONOTONIC) > deadline

      sleep 0.02
    end
  end

  def test_an_async_jobs_lines_are_kept_under_the_enqueuing_requests_trace
    status, = request('POST', '/orders/1182/async', 'HTTP_X_ADT_TRACE' => 'v1/sessL/checkout1/0')
    assert_equal 200, status
    seen = JobProbe.next_seen
    refute_equal Thread.current.object_id, seen[:thread]

    lines = await_lines('sessL/checkout1', 'Error performing OrderConfirmationJob')
    joined = lines.join("\n")
    # The request's own lines, then the job's, all under the one trace.
    assert_match %r{\A\[v1/sessL/checkout1/0\] INFO Started POST "/orders/1182/async"}, lines.first, joined
    assert_includes joined, 'INFO Sending confirmation for order 1182 to pat.secret@example.com'
    assert_includes joined, 'INFO Performing OrderConfirmationJob'
    assert_includes joined, 'ERROR Error performing OrderConfirmationJob'
    assert(lines.all? { |l| l.start_with?('[v1/sessL/checkout1/0] ') }, joined)
  end

  def test_a_job_enqueued_outside_a_request_keeps_no_lines
    OrderConfirmationJob.perform_later(77)
    JobProbe.next_seen
    sleep 0.2 # the job's last lines come after its probe

    assert_equal 0, Devbench::Logs.buffer.size,
                 "untraced job lines were kept: #{Devbench::Logs.buffer.inspect[0, 300]}"
  end

  def test_an_async_jobs_lines_are_delivered_when_asked
    request('POST', '/orders/91/async', 'HTTP_X_ADT_TRACE' => 'v1/sessD/order91/0')
    await_lines('sessD/order91', 'Error performing OrderConfirmationJob')
    before = AJ_LOGS_INGEST.log_deliveries.length
    AJ_LOGS_INGEST.need_logs = ['sessD/order91']

    assert Devbench.flush!
    delivery = AJ_LOGS_INGEST.log_deliveries[before]
    refute_nil delivery, 'no POST /v1/logs arrived'
    assert_equal 'adt_server_aj', delivery.headers['x-adt-key']
    slice = delivery.json['slices'].find { |s| s['trace'] == 'sessD/order91' }
    refute_nil slice, "no slice for the job's trace: #{delivery.json.inspect}"
    # Redacted on the way out, as every delivered line is.
    assert_includes slice['lines'], '[v1/sessD/order91/0] INFO Sending confirmation for order <num> to <email>'
    refute_includes slice['lines'].join("\n"), 'pat.secret@example.com'
  ensure
    AJ_LOGS_INGEST.need_logs = []
  end
end
