# frozen_string_literal: true

# The Sidekiq hooks, against real Sidekiq: jobs run through Sidekiq's own
# Processor#process — its job logger, retry handler, stats, reloader, the
# configured server middleware chain and its error_handlers — and are
# enqueued through Sidekiq::Client#push and the configured client chain.
# Nothing of ours is stubbed; what is asserted is what crossed the unix
# socket.
#
# No Redis: jobs run with retry: false (the path where Sidekiq's retrier
# touches no Redis), and enqueueing uses Sidekiq::Testing's fake mode, which
# replaces only the final Redis write — the client middleware still runs.
#
# Sidekiq is a test-only dependency and never a gem dependency. Where it is
# not installed these tests skip — unless ADT_REQUIRE_SIDEKIQ is set (CI sets
# it), in which case a missing Sidekiq is a failure rather than a silent pass.

require 'minitest/autorun'
require 'logger'
require 'securerandom'
require 'open3'
require 'rbconfig'

SIDEKIQ_LOAD_ERROR = begin
  require 'sidekiq'
  # Loading the CLI is what makes Sidekiq.server? true, as it is in a real
  # Sidekiq process: configure_server blocks run, configure_client blocks do
  # not. It also installs Sidekiq's own InterruptHandler middleware.
  require 'sidekiq/cli'
  require 'sidekiq/processor'
  require 'sidekiq/fetch'
  require 'sidekiq/testing'
  nil
rescue LoadError => e
  e
end

require 'adt'
require_relative 'sidecar_helper'
require_relative 'ingest_helper'

if SIDEKIQ_LOAD_ERROR.nil?
  Sidekiq.default_configuration.logger = nil # Sidekiq: nil means level FATAL
  ADT::SidekiqHooks.install

  # Each job records the exception it raised, so a test can check that the
  # very same object came out of Sidekiq.
  module RaisedBy
    def self.last
      Thread.current[:adt_test_raised]
    end

    def self.raise!(error)
      Thread.current[:adt_test_raised] = error
      raise error
    end
  end

  class FailingWorker
    include Sidekiq::Job
    sidekiq_options retry: false

    def perform(message, email = nil)
      ADT.set_user(email: email, account: 'acct-7') if email
      RaisedBy.raise!(ArgumentError.new(message))
    end
  end

  # Existing installs use the old name too.
  class LegacyWorker
    include Sidekiq::Worker
    sidekiq_options retry: false

    def perform
      RaisedBy.raise!(KeyError.new('legacy'))
    end
  end

  class IdentityLeakingWorker
    include Sidekiq::Job
    sidekiq_options retry: false

    def perform
      ADT.set_user(email: 'leak@example.com', account: 'acct-leak')
      ADT.report_handled(RuntimeError.new('handled in a job'), symbol: 'IdentityLeakingWorker#perform')
    end
  end

  class EnqueuingWorker
    include Sidekiq::Job
    sidekiq_options retry: false

    def perform
      FailingWorker.perform_async('enqueued by a job')
    end
  end

  class ShutdownWorker
    include Sidekiq::Job
    sidekiq_options retry: false

    def perform(wrap)
      raise Sidekiq::Shutdown unless wrap

      begin
        raise Sidekiq::Shutdown
      rescue Exception # rubocop:disable Lint/RescueException
        # What an app's own rescue-and-wrap does to a hard shutdown.
        raise 'wrapped shutdown'
      end
    end
  end

  # An iterable job being stopped for a deploy raises this; Sidekiq's
  # InterruptHandler re-queues it and raises JobRetry::Skip.
  class InterruptedWorker
    include Sidekiq::Job
    sidekiq_options retry: false

    def perform
      raise Sidekiq::Job::Interrupted
    end
  end

  class OkWorker
    include Sidekiq::Job
    sidekiq_options retry: false

    def perform; end
  end
end

class SidekiqHooksTest < Minitest::Test
  include SidecarHelper

  def setup
    if SIDEKIQ_LOAD_ERROR
      flunk "Sidekiq is required here but did not load: #{SIDEKIQ_LOAD_ERROR.message}" if ENV['ADT_REQUIRE_SIDEKIQ']
      skip "Sidekiq not installed (#{SIDEKIQ_LOAD_ERROR.message}); the Sidekiq hooks are untested in this run"
    end
    ADT::Current.clear
    Sidekiq::Worker.clear_all
    Thread.current[:adt_test_raised] = nil
    # A real error handler alongside ours, recording what Sidekiq hands its
    # handlers — so a test can show a failure reached both hooks.
    @seen_by_handlers = []
    @spy = ->(ex, _ctx, _cfg) { @seen_by_handlers << ex }
    Sidekiq.default_configuration.error_handlers << @spy
    start_sidecar
  end

  def teardown
    Sidekiq.default_configuration.error_handlers.delete(@spy) if @spy
    stop_sidecar
    ADT::Current.clear
  end

  # Runs one job hash the way a Sidekiq process does after fetching it.
  def process(job)
    config = Sidekiq.default_configuration
    processor = Sidekiq::Processor.new(config.default_capsule) { |*| nil }
    uow = Sidekiq::BasicFetch::UnitOfWork.new('queue:default', Sidekiq.dump_json(job), config)
    processor.send(:process, uow)
  end

  def job_for(klass, *args, **extra)
    {
      'class' => klass.name, 'args' => args, 'jid' => SecureRandom.hex(12),
      'queue' => 'default', 'retry' => false, 'created_at' => Time.now.to_f
    }.merge(extra.transform_keys(&:to_s))
  end

  def test_a_failing_job_is_reported_once_and_the_same_exception_propagates
    raised = assert_raises(ArgumentError) { process(job_for(FailingWorker, 'deal sync failed')) }

    assert_same RaisedBy.last, raised, 'Sidekiq must see the very exception the job raised'
    # Sidekiq gave it to its error_handlers too (ours among them): two hooks,
    # one report.
    assert(@seen_by_handlers.any? { |e| e.equal?(raised) }, 'error_handlers did not receive the failure')

    msg = sidecar_message
    assert_equal 'exception', msg['kind']
    assert_equal 'job', msg['context']
    assert_equal false, msg['handled']
    assert_equal 'ArgumentError', msg['error']
    assert_equal 'deal sync failed', msg['message']
    assert_equal 'FailingWorker#perform', msg['symbol']
    assert(msg['frames'].any? { |f| f['function'].include?('perform') },
           "no frame for the job's #perform: #{msg['frames'].first(3).inspect}")
    refute msg.key?('trace')
    refute msg.key?('user')
  end

  def test_a_sidekiq_worker_is_reported
    assert_raises(KeyError) { process(job_for(LegacyWorker)) }

    assert_equal 'LegacyWorker#perform', sidecar_message['symbol']
  end

  # ActiveJob through Sidekiq's adapter runs as a JobWrapper; the job a
  # triager knows is the wrapped one.
  def test_a_wrapped_job_is_named_by_the_wrapped_class
    job = job_for(FailingWorker, 'wrapped', wrapped: 'Billing::InvoiceJob')
    assert_raises(ArgumentError) { process(job) }

    assert_equal 'Billing::InvoiceJob#perform', sidecar_message['symbol']
  end

  def test_the_jobs_identity_is_reported_and_then_cleared
    assert_raises(ArgumentError) { process(job_for(FailingWorker, 'as pat', ' Pat@Example.com ')) }

    assert_equal({ 'email' => 'pat@example.com', 'account' => 'acct-7' }, sidecar_message['user'])
    assert_nil ADT::Current.user, 'identity outlived the job on the worker thread'
  end

  # The bug this replaces: set_user in a job, outside any request, stayed
  # on the thread and was attributed to the next job's failure.
  def test_identity_and_handled_count_do_not_leak_between_jobs
    process(job_for(IdentityLeakingWorker))
    handled = sidecar_message
    assert_equal 'handled_failure', handled['kind']
    assert_equal({ 'email' => 'leak@example.com', 'account' => 'acct-leak' }, handled['user'])

    assert_nil ADT::Current.user
    assert_equal 0, ADT::Current.handled_count

    assert_raises(ArgumentError) { process(job_for(FailingWorker, 'next job')) }
    msg = sidecar_message
    assert_equal 'next job', msg['message']
    refute msg.key?('user'), "the previous job's identity leaked: #{msg['user'].inspect}"
  end

  # Something left on the thread from before (here: set directly) is not
  # inherited by the job.
  def test_a_job_starts_with_no_identity
    ADT.set_user(email: 'stale@example.com')

    assert_raises(ArgumentError) { process(job_for(FailingWorker, 'fresh')) }

    refute sidecar_message.key?('user')
  end

  def test_the_trace_travels_from_enqueue_to_the_report
    trace = ADT::Trace.parse('v1/sessQ/actJ/2')
    ADT::Current.with(trace) { FailingWorker.perform_async('after the request') }

    job = FailingWorker.jobs.first
    assert_equal 'v1/sessQ/actJ/2', job['adt_trace'], 'the client middleware did not tag the job'

    assert_raises(ArgumentError) { process(job) }
    assert_equal 'v1/sessQ/actJ/2', sidecar_message['trace']
    assert_nil ADT::Current.trace, 'the trace outlived the job'
  end

  def test_no_trace_no_key
    FailingWorker.perform_async('no request')

    refute FailingWorker.jobs.first.key?('adt_trace')
  end

  # The server's client chain: a job that enqueues a job passes the trace on.
  def test_a_job_enqueued_by_a_job_keeps_the_trace
    process(job_for(EnqueuingWorker, adt_trace: 'v1/sessQ/actK/0'))

    assert_equal 'v1/sessQ/actK/0', FailingWorker.jobs.first['adt_trace']
  end

  def test_a_malformed_trace_is_ignored
    assert_raises(ArgumentError) { process(job_for(FailingWorker, 'bad trace', adt_trace: 'v1/no')) }

    refute sidecar_message.key?('trace')
  end

  def test_shutdown_is_not_reported
    process(job_for(ShutdownWorker, false))
    assert_empty sidecar_messages
  end

  # Sidekiq re-queues an exception caused by a hard shutdown as a shutdown.
  def test_an_exception_caused_by_shutdown_is_not_reported
    process(job_for(ShutdownWorker, true))
    assert_empty sidecar_messages
  end

  def test_an_interrupted_iterable_job_is_not_reported
    assert_raises(Sidekiq::JobRetry::Skip) { process(job_for(InterruptedWorker)) }

    assert_equal 1, InterruptedWorker.jobs.size, "Sidekiq's InterruptHandler did not re-queue it"
    assert_empty sidecar_messages
  end

  def test_a_clean_job_sends_nothing
    process(job_for(OkWorker))
    assert_empty sidecar_messages
  end

  # Errors outside job execution reach only the error handlers: a fetch
  # failure (Redis down) is handed over with no context at all.
  def test_an_error_outside_a_job_is_reported_without_identity
    ADT.set_user(email: 'stale@example.com')
    error = RuntimeError.new('Error connecting to Redis')

    Sidekiq.default_configuration.handle_exception(error, {})

    msg = sidecar_message
    assert_equal 'job', msg['context']
    assert_equal 'Sidekiq', msg['symbol']
    assert_equal 'Error connecting to Redis', msg['message']
    refute msg.key?('user')
    assert_equal 'stale@example.com', ADT::Current.user[:email], 'the thread state was not restored'
  end

  def test_an_error_handler_report_for_a_job_uses_the_job
    job = job_for(FailingWorker, adt_trace: 'v1/sessD/actD/0')
    Sidekiq.default_configuration.handle_exception(RuntimeError.new('death handler broke'),
                                                   { context: 'Error calling death handler', job: job })

    msg = sidecar_message
    assert_equal 'FailingWorker#perform', msg['symbol']
    assert_equal 'v1/sessD/actD/0', msg['trace']

    Sidekiq.default_configuration.handle_exception(RuntimeError.new('scheduler'), { context: 'scheduling poller thread died!' })
    assert_equal 'Sidekiq: scheduling poller thread died!', sidecar_message['symbol']
  end

  # A middleware outside ours fails: Sidekiq reports it to error_handlers
  # only. Ours is first in the chain precisely so this case is rare.
  def test_the_server_middleware_is_outermost
    assert_equal ADT::SidekiqHooks::ServerMiddleware, Sidekiq.default_configuration.server_middleware.entries.first.klass
  end

  def test_installing_again_is_a_no_op
    config = Sidekiq.default_configuration
    count = lambda do
      [config.server_middleware.count { |e| e.klass == ADT::SidekiqHooks::ServerMiddleware },
       config.client_middleware.count { |e| e.klass == ADT::SidekiqHooks::ClientMiddleware },
       config.error_handlers.count { |h| h.equal?(ADT::SidekiqHooks::ERROR_HANDLER) }]
    end

    assert ADT::SidekiqHooks.install
    assert ADT::SidekiqHooks.install

    assert_equal [1, 1, 1], count.call
  end

  # Whatever arrives in the job hash, the hooks neither raise nor swallow.
  def test_a_broken_payload_never_raises_into_sidekiq
    server = Sidekiq.default_configuration.server_middleware
    client = Sidekiq.default_configuration.client_middleware
    odd = { 'class' => 42, 'wrapped' => [], 'adt_trace' => { 'not' => 'a string' } }

    # Sidekiq ignores a server chain's return value (its own Metrics
    # middleware replaces it), so what is checked is that the job ran.
    ran = []
    server.invoke(Object.new, odd, 'default') { ran << :odd }
    # Not through the chain: Sidekiq's own Metrics middleware fails on a nil
    # job hash. Ours must not.
    ADT::SidekiqHooks::ServerMiddleware.new.call(Object.new, nil, nil) { ran << :nil }
    assert_equal %i[odd nil], ran

    # The client chain's value is the payload Sidekiq pushes.
    assert_equal :pushed, client.invoke('X', nil, 'default', nil) { :pushed }
    ADT::Current.with(ADT::Trace.parse('v1/s/a/0')) do
      frozen = { 'class' => 'X' }.freeze
      assert_equal :pushed, client.invoke('X', frozen, 'default', nil) { :pushed }
    end

    # The job's own exception still comes out, the same object, even when
    # its #message raises and the hash is nonsense.
    hostile = Class.new(StandardError) { def message = raise('no message for you') }.new
    out = assert_raises(StandardError) { server.invoke(Object.new, odd, 'default') { raise hostile } }
    assert_same hostile, out

    Sidekiq.default_configuration.handle_exception(RuntimeError.new('odd context'), 'not a hash')

    # Sidekiq rescues a raising error handler and logs it, so through
    # handle_exception a fault of ours would be invisible. Called directly,
    # it must not raise at all.
    hostile_context = Class.new(Hash) { def [](_key) = raise('no context for you') }.new
    assert_nil ADT::SidekiqHooks::ERROR_HANDLER.call(RuntimeError.new('x'), hostile_context, nil)

    messages = sidecar_messages
    assert_equal %w[StandardError RuntimeError], messages.map { |m| m['error'] }
    assert_equal ['', 'Sidekiq'], messages.map { |m| m['symbol'] }
  end

  # Enqueueing must never fail because of us, and a failing push is not
  # ours to swallow.
  def test_the_client_middleware_never_swallows_a_push_failure
    client = Sidekiq.default_configuration.client_middleware
    error = assert_raises(IOError) { client.invoke('X', {}, 'default', nil) { raise IOError, 'redis down' } }
    assert_equal 'redis down', error.message
  end
end

# The gem in processes other than a plain Sidekiq server, each in a fresh
# Ruby: what is installed depends on what is loaded at boot.
class SidekiqInstallTest < Minitest::Test
  include SidecarHelper

  LIB = File.expand_path('../lib', __dir__)

  def setup
    if SIDEKIQ_LOAD_ERROR
      flunk "Sidekiq is required here but did not load: #{SIDEKIQ_LOAD_ERROR.message}" if ENV['ADT_REQUIRE_SIDEKIQ']
      skip "Sidekiq not installed (#{SIDEKIQ_LOAD_ERROR.message})"
    end
  end

  def ruby(script, env = {})
    out, err, status = Open3.capture3(env, RbConfig.ruby, '-w', '-I', LIB, '-e', script)
    assert status.success?, "subprocess failed:\n#{err}"
    assert_empty err.lines.grep(/#{Regexp.escape(LIB)}/), "warnings from the gem:\n#{err}"
    out
  end

  # A web process enqueues; it never runs jobs.
  def test_a_client_process_gets_only_the_client_middleware
    out = ruby(<<~RUBY)
      require 'sidekiq'
      require 'adt'
      print ADT::SidekiqHooks.install, ' ',
            Sidekiq.default_configuration.client_middleware.exists?(ADT::SidekiqHooks::ClientMiddleware), ' ',
            Sidekiq.default_configuration.server_middleware.exists?(ADT::SidekiqHooks::ServerMiddleware)
    RUBY
    assert_equal 'true true false', out
  end

  def test_without_sidekiq_install_is_false
    assert_equal 'false', ruby('require "adt"; print ADT::SidekiqHooks.install')
  end

  # A typical shape: Rails, a native Sidekiq job, a Sidekiq server process.
  # The Railtie installs the hooks with no code from the app; a job runs
  # through Sidekiq's Rails reloader (Rails' executor, which reports to
  # Rails.error itself); and an ActiveJob through Sidekiq's adapter passes
  # four hooks. Each failure is reported once.
  def test_under_rails_the_railtie_installs_and_each_failure_is_reported_once
    begin
      require 'rails'
    rescue LoadError => e
      flunk "Rails is required here but did not load: #{e.message}" if ENV['ADT_REQUIRE_RAILS']
      skip "Rails not installed (#{e.message})"
    end

    start_sidecar
    begin
      out = ruby(RAILS_SCRIPT, 'ADT_SIDECAR_SOCKET' => @sidecar_path)
      assert_equal 'installed', out.lines.first.strip

      messages = sidecar_messages
      assert_equal 3, messages.length, messages.inspect

      native, missing, wrapped = messages
      assert_equal ['job', 'NativeJob#perform', 'NoMethodError', 'v1/sessR/actR/0'],
                   native.values_at('context', 'symbol', 'error', 'trace')
      assert_equal({ 'email' => 'pat@example.com' }, native['user'])

      # A job class that does not exist fails inside Sidekiq's reloader,
      # before the middleware: reported once, as a job failure.
      assert_equal %w[job NameError], missing.values_at('context', 'error')

      assert_equal ['job', 'AjFailing#perform', 'ArgumentError'], wrapped.values_at('context', 'symbol', 'error')
    ensure
      stop_sidecar
    end
  end

  # Direct mode in a Sidekiq server process: a failing job is counted with
  # the job's user and reaches ingest over HTTP, with nothing else to set up.
  def test_a_failing_job_reaches_ingest_directly
    ingest = FakeIngest.new
    out = ruby(<<~'RUBY', 'DEVBENCH_DSN' => ingest.dsn, 'DEVBENCH_SERVICE' => 'worker', 'DEVBENCH_ENABLED' => nil)
      require 'sidekiq'
      require 'sidekiq/cli'
      require 'sidekiq/processor'
      require 'sidekiq/fetch'
      require 'devbench'
      Sidekiq.default_configuration.logger = nil
      ADT::SidekiqHooks.install

      class DirectJob
        include Sidekiq::Job
        sidekiq_options retry: false
        def perform
          ADT.set_user(email: 'Pat@Example.com')
          raise ArgumentError, 'job 42 failed'
        end
      end

      config = Sidekiq.default_configuration
      processor = Sidekiq::Processor.new(config.default_capsule) { |*| nil }
      job = { 'class' => 'DirectJob', 'args' => [], 'jid' => 'j1', 'queue' => 'default', 'retry' => false }
      begin
        processor.send(:process, Sidekiq::BasicFetch::UnitOfWork.new('queue:default', Sidekiq.dump_json(job), config))
      rescue ArgumentError
        nil
      end
      print Devbench.flush!
    RUBY
    assert_equal 'true', out
    counts = ingest.flushes.flat_map { |f| f.json['counts'] }
    assert_equal 1, counts.length, counts.inspect
    assert_equal ['error', 1, [{ 'email' => 'pat@example.com' }]], counts.first.values_at('kind', 'n', 'users')
  ensure
    ingest&.close
  end

  RAILS_SCRIPT = <<~'RUBY'
    require 'logger'
    require 'sidekiq'
    require 'sidekiq/cli' # as the sidekiq executable does, before booting the app
    require 'rails'
    require 'active_job/railtie'
    require 'sidekiq/rails'
    require 'adt'
    require 'sidekiq/processor'
    require 'sidekiq/fetch'
    require 'sidekiq/testing'

    class SidekiqRailsApp < Rails::Application
      config.root = __dir__
      config.load_defaults "#{Rails::VERSION::MAJOR}.#{Rails::VERSION::MINOR}"
      config.eager_load = false
      config.logger = Logger.new(nil)
      config.secret_key_base = 'x' * 64
      config.active_job.queue_adapter = :sidekiq
    end
    SidekiqRailsApp.initialize!
    Sidekiq.default_configuration.logger = nil

    class NativeJob
      include Sidekiq::Job
      sidekiq_options retry: false
      def perform
        ADT.set_user(email: 'pat@example.com')
        nil.name
      end
    end

    class AjFailing < ActiveJob::Base
      sidekiq_options retry: false
      def perform = raise(ArgumentError, 'aj')
    end

    config = Sidekiq.default_configuration
    ok = config.server_middleware.entries.first.klass == ADT::SidekiqHooks::ServerMiddleware &&
         config.client_middleware.exists?(ADT::SidekiqHooks::ClientMiddleware) &&
         config.error_handlers.include?(ADT::SidekiqHooks::ERROR_HANDLER)
    puts(ok ? 'installed' : 'not installed')

    def run(job)
      config = Sidekiq.default_configuration
      processor = Sidekiq::Processor.new(config.default_capsule) { |*| nil }
      processor.send(:process, Sidekiq::BasicFetch::UnitOfWork.new('queue:default', Sidekiq.dump_json(job), config))
    rescue Exception # rubocop:disable Lint/RescueException
      nil
    end

    ADT::Current.with(ADT::Trace.parse('v1/sessR/actR/0')) { NativeJob.perform_async }
    run(NativeJob.jobs.first)
    run('class' => 'NoSuchJob', 'args' => [], 'jid' => 'j1', 'queue' => 'default', 'retry' => false)
    AjFailing.perform_later
    run(Sidekiq::Queues['default'].last)
  RUBY
end
