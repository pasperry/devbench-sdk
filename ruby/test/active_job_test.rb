# frozen_string_literal: true

# The request's trace carried into ActiveJob jobs (#356), against a real
# in-process Rails application (test/support/active_job_app.rb) with real
# ActiveJob adapters: async (ActiveJob's thread pool — serialize at
# enqueue, ActiveJob::Base.execute on a worker thread, the path Solid Queue
# and GoodJob take), inline, and Sidekiq's own adapter run through Sidekiq's
# Processor and server middleware. Nothing of ours is stubbed; what is
# asserted is what crossed the unix socket. Solid Queue on real Postgres is
# proved end to end by the install test (tools/installtest/run.sh).
#
# Rails and Sidekiq are test-only dependencies. Where they are not installed
# these tests skip — unless ADT_REQUIRE_RAILS / ADT_REQUIRE_SIDEKIQ is set
# (CI sets both), in which case that is a failure.

require 'minitest/autorun'
require 'logger'
require 'securerandom'

%w[DEVBENCH_DSN ADT_DSN DEVBENCH_ENABLED].each { |k| ENV.delete(k) }

AJ_RAILS_LOAD_ERROR = begin
  require 'rails'
  require 'action_controller/railtie'
  require 'active_job/railtie'
  nil
rescue LoadError => e
  e
end

AJ_SIDEKIQ_LOAD_ERROR = begin
  require 'sidekiq'
  require 'sidekiq/cli' # Sidekiq.server? is true, as in a real Sidekiq process
  require 'sidekiq/processor'
  require 'sidekiq/fetch'
  require 'sidekiq/testing'
  nil
rescue LoadError => e
  e
end

require 'devbench'
require_relative 'sidecar_helper'

if AJ_RAILS_LOAD_ERROR.nil?
  Sidekiq.default_configuration.logger = Logger.new(IO::NULL) if AJ_SIDEKIQ_LOAD_ERROR.nil?
  require_relative 'support/active_job_app'
end

if AJ_RAILS_LOAD_ERROR.nil? && AJ_SIDEKIQ_LOAD_ERROR.nil?
  # An app's own Sidekiq server middleware, inside ours, that sets the user
  # from the job — what an app with per-tenant jobs does. A second trace
  # scope inside it (ActiveJob's, on top of Sidekiq's) would wipe it.
  class SetsUserFromJobMiddleware
    def call(_worker, job, _queue)
      Devbench.set_user(email: 'from-middleware@example.com') if job['wrapped'] == 'SidekiqAdapterJob'
      yield
    end
  end
  Sidekiq.default_configuration.server_middleware { |chain| chain.add(SetsUserFromJobMiddleware) }
  Sidekiq::Testing.fake!
end

module ActiveJobTestSupport
  include SidecarHelper

  def setup
    if AJ_RAILS_LOAD_ERROR
      flunk "Rails is required here but did not load: #{AJ_RAILS_LOAD_ERROR.message}" if ENV['ADT_REQUIRE_RAILS']
      skip "Rails not installed (#{AJ_RAILS_LOAD_ERROR.message}); the ActiveJob hooks are untested in this run"
    end
    Devbench::Current.clear
    JobProbe.clear
    start_sidecar
  end

  def teardown
    stop_sidecar
    Devbench::Current.clear
  end

  def request(method, path, headers = {})
    env = Rack::MockRequest.env_for(path, { method: method }.merge(headers))
    status, headers, body = DevbenchActiveJobTestApp.call(env)
    body.close if body.respond_to?(:close)
    [status, headers, body]
  end

  # Waits for the next message the SDK sends (an async job reports from
  # the adapter's worker thread, after the request has returned).
  def next_message(timeout = 5)
    raw = @sidecar_received.pop(timeout: timeout)
    flunk 'timed out waiting for a report from the job' if raw.nil?
    JSON.parse(raw)
  end
end

class ActiveJobTraceTest < Minitest::Test
  include ActiveJobTestSupport

  def test_the_railtie_installed_the_hooks
    assert_operator ActiveJob::Base, :<=, Devbench::ActiveJobHooks::Job
    assert_equal 1, ActiveJob::Base.ancestors.count(Devbench::ActiveJobHooks::Job)
  end

  def test_installing_again_is_a_no_op
    assert Devbench::ActiveJobHooks.install(ActiveJob::Base)
    assert Devbench::ActiveJobHooks.install(ActiveJob::Base)
    assert_equal 1, ActiveJob::Base.ancestors.count(Devbench::ActiveJobHooks::Job)
    refute Devbench::ActiveJobHooks.install(Object.new)
  end

  # The demo's R5: a traced request enqueues a job; the job runs later on
  # another thread and fails. Its report carries the request's trace.
  def test_an_async_jobs_failure_carries_the_enqueuing_requests_trace
    status, = request('POST', '/orders/1182/async', 'HTTP_X_ADT_TRACE' => 'v1/sessA/checkout1/0')
    assert_equal 200, status

    msg = next_message
    assert_equal 'exception', msg['kind']
    assert_equal 'job', msg['context']
    assert_equal 'ArgumentError', msg['error']
    assert_equal 'confirmation failed for order 1182', msg['message']
    assert_equal 'OrderConfirmationJob#perform', msg['symbol']
    # Unchanged, as Sidekiq carries it: the job is the same service.
    assert_equal 'v1/sessA/checkout1/0', msg['trace']
    # Identity is the job's own, not the request's (as on Sidekiq).
    refute msg.key?('user'), "the request's identity was carried: #{msg['user'].inspect}"

    seen = JobProbe.next_seen
    assert_equal 'v1/sessA/checkout1/0', seen[:trace]
    refute_equal Thread.current.object_id, seen[:thread], 'the async adapter ran it on this thread'
    assert_empty sidecar_messages, 'reported more than once'
  end

  def test_the_trace_rides_in_the_jobs_payload
    payload = Devbench::Current.with(Devbench::Trace.parse('v1/sessP/actP/3')) { RecordingJob.new.serialize }
    assert_equal 'v1/sessP/actP/3', payload['adt_trace']

    refute RecordingJob.new.serialize.key?('adt_trace'), 'an untraced enqueue must add no key'
  end

  def test_a_job_enqueued_outside_a_request_runs_untraced
    OrderConfirmationJob.perform_later(7)

    msg = next_message
    assert_equal 'confirmation failed for order 7', msg['message']
    refute msg.key?('trace'), "an untraced job reported trace #{msg['trace'].inspect}"
  end

  # A payload serialized by a gem without this hook (no key), or carrying
  # something that is not a trace, still deserializes and runs, untraced.
  def test_a_payload_from_an_older_gem_still_runs
    old = OrderConfirmationJob.new(8).serialize
    old.delete('adt_trace')
    assert_raises(ArgumentError) { ActiveJob::Base.execute(old) }
    refute sidecar_message.key?('trace')

    junk = OrderConfirmationJob.new(9).serialize.merge('adt_trace' => { 'not' => 'a trace' })
    assert_raises(ArgumentError) { ActiveJob::Base.execute(junk) }
    refute sidecar_message.key?('trace')
  end

  # The worker thread's own context comes back after the job, whatever it
  # was: the job's trace never outlives it.
  def test_the_jobs_trace_is_scoped_to_the_job
    payload = Devbench::Current.with(Devbench::Trace.parse('v1/sessS/actS/0')) { OrderConfirmationJob.new(5).serialize }
    outer = Devbench::Trace.parse('v1/sessO/actO/0')

    Devbench::Current.with(outer) do
      Devbench.set_user(email: 'worker@example.com')
      assert_raises(ArgumentError) { ActiveJob::Base.execute(payload) }

      assert_equal 'v1/sessO/actO/0', Devbench::Current.trace.to_s
      assert_equal({ email: 'worker@example.com' }, Devbench::Current.user)
    end
    msg = sidecar_message
    assert_equal 'v1/sessS/actS/0', msg['trace']
    refute msg.key?('user'), "the worker thread's identity was inherited: #{msg['user'].inspect}"
    assert_nil Devbench::Current.trace
  end

  # Pool threads are reused: identity and handled counts set in one job
  # must not reach the next job on the same thread.
  def test_identity_does_not_leak_between_jobs_on_a_pool_thread
    IdentityJob.perform_later
    handled = next_message
    assert_equal 'handled_failure', handled['kind']
    assert_equal({ 'email' => 'leak@example.com', 'account' => 'acct-leak' }, handled['user'])
    first = JobProbe.next_seen

    OrderConfirmationJob.perform_later(3)
    msg = next_message
    assert_equal 'confirmation failed for order 3', msg['message']
    second = JobProbe.next_seen
    assert_equal first[:thread], second[:thread], 'the pool was meant to reuse its one thread'
    refute msg.key?('user'), "the previous job's identity leaked: #{msg['user'].inspect}"
  end

  # retry_on re-enqueues from the worker thread; the retry keeps the trace
  # of the request that enqueued the first attempt.
  def test_a_retried_job_keeps_the_enqueuing_requests_trace
    request('POST', '/retried', 'HTTP_X_ADT_TRACE' => 'v1/sessR/retry1/0')

    first = JobProbe.next_seen
    second = JobProbe.next_seen
    refute_nil second, 'the job was not retried'
    assert_equal ['v1/sessR/retry1/0'] * 2, [first[:trace], second[:trace]]
    msg = next_message
    assert_equal 'attempt 2', msg['message']
    assert_equal 'v1/sessR/retry1/0', msg['trace']
  end

  # A job object that came with a trace keeps it when serialized again from
  # another context (a re-enqueue outside the job's scope).
  def test_a_deserialized_jobs_trace_wins_over_the_active_one
    payload = Devbench::Current.with(Devbench::Trace.parse('v1/sessK/actK/0')) { RecordingJob.new.serialize }
    job = ActiveJob::Base.deserialize(payload)

    again = Devbench::Current.with(Devbench::Trace.parse('v1/sessX/actX/0')) { job.serialize }
    assert_equal 'v1/sessK/actK/0', again['adt_trace']
  end

  # The inline adapter runs the job inside the request: the request's trace
  # and identity, one report.
  def test_an_inline_job_reports_once_with_the_requests_trace_and_user
    status, = request('POST', '/orders/44/inline', 'HTTP_X_ADT_TRACE' => 'v1/sessI/actI/0')
    assert_equal 500, status

    msg = sidecar_message
    assert_equal 'job', msg['context']
    assert_equal 'InlineConfirmationJob#perform', msg['symbol']
    assert_equal 'v1/sessI/actI/0', msg['trace']
    assert_equal({ 'email' => 'buyer@example.com', 'account' => 'acct-1' }, msg['user'])
  end

  # perform_now on a job never serialized is a method call: the caller's
  # context, untouched — inside a request's scope, and outside any scope
  # (a rake task or console that set its own trace and user).
  def test_perform_now_runs_in_the_callers_context
    Devbench::Current.with(Devbench::Trace.parse('v1/sessN/actN/0')) do
      Devbench.set_user(email: 'caller@example.com')
      assert_raises(ArgumentError) { OrderConfirmationJob.perform_now(2) }
    end
    msg = sidecar_message
    assert_equal 'v1/sessN/actN/0', msg['trace']
    assert_equal({ 'email' => 'caller@example.com' }, msg['user'])

    refute Devbench::Current.scoped?
    Devbench::Current.trace = Devbench::Trace.parse('v1/sessM/actM/0')
    Devbench.set_user(email: 'rake@example.com')
    assert_raises(ArgumentError) { OrderConfirmationJob.perform_now(4) }
    msg = sidecar_message
    assert_equal 'v1/sessM/actM/0', msg['trace']
    assert_equal({ 'email' => 'rake@example.com' }, msg['user'])
  end
end

class ActiveJobOnSidekiqTest < Minitest::Test
  include ActiveJobTestSupport

  def setup
    super
    if AJ_SIDEKIQ_LOAD_ERROR
      flunk "Sidekiq is required here but did not load: #{AJ_SIDEKIQ_LOAD_ERROR.message}" if ENV['ADT_REQUIRE_SIDEKIQ']
      skip "Sidekiq not installed (#{AJ_SIDEKIQ_LOAD_ERROR.message})"
    end
    Sidekiq::Worker.clear_all
  end

  # Runs a pushed job the way a Sidekiq process does after fetching it.
  # retry: false (ActiveJob's adapter defaults it on) is the path where
  # Sidekiq's retrier touches no Redis; there is none here.
  def process(job)
    config = Sidekiq.default_configuration
    processor = Sidekiq::Processor.new(config.default_capsule) { |*| nil }
    uow = Sidekiq::BasicFetch::UnitOfWork.new('queue:default', Sidekiq.dump_json(job), config)
    processor.send(:process, uow)
  end

  def enqueued
    jobs = Sidekiq::Queues.jobs_by_queue.values.flatten
    assert_equal 1, jobs.length, "expected one pushed job, got #{jobs.inspect}"
    jobs.first
  end

  def test_activejob_on_sidekiq_carries_the_trace_once
    status, = request('POST', '/orders/61/sidekiq', 'HTTP_X_ADT_TRACE' => 'v1/sessQ/actQ/0')
    assert_equal 200, status

    job = enqueued
    assert_equal 'SidekiqAdapterJob', job['wrapped']
    # Both carry it: Sidekiq's client middleware on the job hash, ours in
    # the ActiveJob payload. The same value, so no second scope.
    assert_equal 'v1/sessQ/actQ/0', job['adt_trace']
    assert_equal 'v1/sessQ/actQ/0', job['args'].first['adt_trace']

    assert_raises(ArgumentError) { process(job.merge('retry' => false)) }
    msg = sidecar_message
    assert_equal 'v1/sessQ/actQ/0', msg['trace']
    assert_equal 'SidekiqAdapterJob#perform', msg['symbol']
    # The user an app's Sidekiq middleware set inside Sidekiq's scope
    # survives: ActiveJob did not open a second scope that would reset it.
    assert_equal({ 'email' => 'from-middleware@example.com' }, msg['user'])
    seen = JobProbe.next_seen
    assert_equal({ email: 'from-middleware@example.com' }, seen[:user])
    assert_nil Devbench::Current.trace, 'the trace outlived the job'
  end

  def test_an_untraced_activejob_on_sidekiq_runs_untraced
    SidekiqAdapterJob.perform_later(62)

    job = enqueued
    refute job.key?('adt_trace')
    refute job['args'].first.key?('adt_trace')
    assert_raises(ArgumentError) { process(job.merge('retry' => false)) }
    msg = sidecar_message
    refute msg.key?('trace')
    # Untraced, it is still Sidekiq's scope the job runs in, not a second one.
    assert_equal({ 'email' => 'from-middleware@example.com' }, msg['user'])
  end
end
