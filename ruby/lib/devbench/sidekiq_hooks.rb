# frozen_string_literal: true

require_relative 'trace'
require_relative 'current'
require_relative 'reporter'

module Devbench
  # Native Sidekiq jobs (docs/SERVER_SDK_SPEC.md, Capability 2, `job`).
  #
  # A class that includes Sidekiq::Job never touches ActiveJob, so the
  # perform.active_job hook sees none of its failures. This is the
  # sentry-sidekiq equivalent:
  #
  #   * ServerMiddleware — outermost in the server chain. Scopes the trace and
  #     identity to the job, reports what escapes #perform (or any middleware
  #     inside it), then re-raises the same object, so Sidekiq's retry logic
  #     sees exactly what it would have without it.
  #   * ClientMiddleware — copies the active trace into the job hash at
  #     enqueue, so a job's failure joins the request that enqueued it.
  #   * ERROR_HANDLER — Sidekiq's error_handlers also receive failures that
  #     never ran inside the middleware: fetch/Redis errors, a job class that
  #     will not constantize, a failing death handler. Job failures the
  #     middleware already sent reach it too, and are deduped by the
  #     Reporter's mark on the exception object.
  #
  # Plain Ruby: no Sidekiq constant is named at load time, so this file loads
  # without Sidekiq. Devbench::Railtie installs it at boot when Sidekiq is loaded;
  # a non-Rails Sidekiq process calls Devbench::SidekiqHooks.install itself.
  #
  # Every entry point runs inside Sidekiq's processor. Anything raised from
  # here would fail, or worse un-acknowledge, a customer's job, so each one is
  # fully guarded — and the job's own exception is always the one that
  # propagates.
  module SidekiqHooks
    # The job-hash key the trace travels in. Sidekiq passes unknown top-level
    # keys through untouched, including across retries.
    TRACE_KEY = 'adt_trace'

    # Exceptions whose cause chain is followed looking for a shutdown.
    MAX_CAUSES = 10

    # Wraps every job the server runs.
    class ServerMiddleware
      def call(_job_instance, job, _queue)
        # Scoped even without a trace: identity set inside a job must end
        # with it, and the next job on this thread must start with none.
        Current.with(SidekiqHooks.trace_from(job)) do
          yield
        rescue Exception => e # rubocop:disable Lint/RescueException
          # Inside the scope, so the report carries this job's trace and
          # whatever Devbench.set_user the job did. report never raises.
          SidekiqHooks.report(e, job)
          raise e
        end
      end
    end

    # Runs on every push, in the web process and, for jobs enqueued by jobs,
    # in the server.
    class ClientMiddleware
      def call(_job_class, job, _queue, _redis_pool)
        SidekiqHooks.tag(job)
        yield
      end
    end

    # Three parameters, the Sidekiq 7 signature; the third is optional so a
    # caller passing two still works.
    ERROR_HANDLER = proc do |error, context, _config = nil|
      SidekiqHooks.handle_error(error, context)
    end

    class << self
      # Installs the hooks into Sidekiq's global configuration. Safe to call
      # more than once. Returns true when Sidekiq is loaded and the hooks
      # were registered with it.
      #
      # configure_server blocks run only in the Sidekiq process (and for
      # embedded Sidekiq), configure_client blocks only outside it.
      def install
        return false unless defined?(::Sidekiq) && ::Sidekiq.respond_to?(:configure_server)

        ::Sidekiq.configure_server do |config|
          install_server(config)
          install_client(config)
        end
        ::Sidekiq.configure_client { |config| install_client(config) }
        true
      rescue StandardError, SystemStackError
        false
      end

      # The server middleware, outermost so a failure in another middleware
      # is seen too, and the error handler. Returns true when installed.
      def install_server(config)
        config.server_middleware do |chain|
          # prepend replaces an existing entry for the class, so this is
          # idempotent.
          chain.respond_to?(:prepend) ? chain.prepend(ServerMiddleware) : chain.add(ServerMiddleware)
        end

        handlers = config.error_handlers
        handlers << ERROR_HANDLER unless handlers.any? { |h| h.equal?(ERROR_HANDLER) }
        true
      rescue StandardError, SystemStackError
        false
      end

      def install_client(config)
        config.client_middleware { |chain| chain.add(ClientMiddleware) }
        true
      rescue StandardError, SystemStackError
        false
      end

      # Called from the server middleware's rescue.
      def report(error, job)
        return nil if control_flow?(error)

        Reporter.capture(error, context: 'job', handled: false, symbol: job_symbol(job))
      rescue Exception # rubocop:disable Lint/RescueException
        nil
      end

      # Called by Sidekiq's handle_exception. A job failure the middleware
      # sent is already marked and is skipped by Reporter.capture.
      def handle_error(error, context)
        return nil if control_flow?(error)

        job = context.is_a?(Hash) ? context[:job] : nil
        symbol = job.is_a?(Hash) ? job_symbol(job) : sidekiq_symbol(context)

        # Outside any job scope: the job's own trace if there is one, and
        # never the identity of whatever this thread last held.
        Current.with(trace_from(job)) do
          Reporter.capture(error, context: 'job', handled: false, symbol: symbol)
        end
        nil
      rescue Exception # rubocop:disable Lint/RescueException
        nil
      end

      # Copies the active trace into a job hash being enqueued. Never
      # replaces one already there: a re-push (a retry, an interrupted
      # iterable job) keeps the request it came from.
      def tag(job)
        return nil unless job.is_a?(Hash) && !job.key?(TRACE_KEY)

        trace = Current.trace
        job[TRACE_KEY] = trace.to_s unless trace.nil?
        nil
      rescue StandardError, SystemStackError
        nil
      end

      def trace_from(job)
        return nil unless job.is_a?(Hash)

        Trace.parse(job[TRACE_KEY])
      rescue StandardError, SystemStackError
        nil
      end

      # Sidekiq's own control flow, never an application failure:
      # Sidekiq::Shutdown is already skipped as a SignalException; the
      # JobRetry::Handled/Skip and Job::Interrupted it raises through the
      # middleware are on Reporter::NEVER_REPORTED. What remains is an
      # exception *caused* by a hard shutdown — Sidekiq itself treats that
      # as a shutdown and re-queues the job (JobRetry#exception_caused_by_shutdown?).
      def control_flow?(error)
        cause = error
        MAX_CAUSES.times do
          return false if cause.nil?
          return true if cause.class.ancestors.any? { |mod| mod.name == 'Sidekiq::Shutdown' }

          cause = cause.cause
        end
        false
      rescue StandardError, SystemStackError
        false
      end

      # "InvoiceSyncJob#perform". An ActiveJob run through Sidekiq's adapter
      # arrives as a JobWrapper with the real class in "wrapped".
      def job_symbol(job)
        return nil unless job.is_a?(Hash)

        name = job['wrapped'] || job['class']
        name = name.name if name.is_a?(Module)
        return nil unless name.is_a?(String) && !name.empty?

        "#{name}#perform"
      rescue StandardError, SystemStackError
        nil
      end

      private

      # Sidekiq's description of where it caught an error outside a job
      # ("Error calling death handler"), or just "Sidekiq" (a fetch error
      # carries none).
      def sidekiq_symbol(context)
        label = context.is_a?(Hash) ? context[:context] : nil
        label.is_a?(String) && !label.empty? ? "Sidekiq: #{label}" : 'Sidekiq'
      end
    end
  end
end
