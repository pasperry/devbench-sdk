# frozen_string_literal: true

require_relative 'reporter'

module Devbench
  # The Rails-side exception hooks (docs/SERVER_SDK_SPEC.md, Capability 2).
  #
  # Plain Ruby: nothing here names a Rails constant, so this file loads
  # without Rails. Devbench::Railtie installs the hooks against the real
  # Rails.error and ActiveSupport::Notifications at boot.
  #
  # Every entry point here runs inside Rails' own error and instrumentation
  # paths. Since Rails 7.1 an exception raised by a notification subscriber
  # is re-raised into the code being instrumented, so a fault in these hooks
  # would fail the customer's job — every one of them is fully guarded.
  module RailsHooks
    JOB_EVENT = 'perform.active_job'

    # Where Rails' executor says it caught an error, mapped to the context a
    # triager would use. From Rails 7.1, ActionDispatch::Executor reports an
    # unhandled request exception (raised, or rendered as a 500 by
    # ShowExceptions) to Rails.error before Devbench::Middleware, outermost, sees
    # it, so this subscriber is the first hook it reaches. Labelling it
    # `rails_error` would leave `request` empty on every modern Rails app.
    #
    # Jobs need no entry: the perform.active_job hook runs inside the job,
    # before the executor around it reports.
    #
    # Sidekiq's Rails reloader wraps each job in the executor with
    # source 'job.sidekiq', so a failure outside Sidekiq's middleware (a job
    # class that will not load) reaches Rails.error first. It is a job
    # failure.
    SOURCE_CONTEXTS = {
      'application.action_dispatch' => 'request',
      'job.sidekiq' => 'job'
    }.freeze

    # Subscribed to Rails.error (Rails >= 7.0). Receives both Rails.error
    # .report and .handle; `handled` is passed through as Rails gives it.
    class ErrorSubscriber
      def report(error, handled: false, severity: nil, context: nil, source: nil, **_rest)
        Reporter.capture(
          error,
          context: SOURCE_CONTEXTS.fetch(source.to_s, 'rails_error'),
          handled: handled,
          symbol: RailsHooks.symbol_from_context(context)
        )
        nil
      rescue StandardError, SystemStackError
        nil
      end
    end

    class << self
      # Subscribes once per reporter. Returns true when subscribed.
      def install_error_reporter(reporter)
        return false if reporter.nil? || !reporter.respond_to?(:subscribe)
        return true if error_reporters.any? { |r| r.equal?(reporter) }

        reporter.subscribe(ErrorSubscriber.new)
        error_reporters << reporter
        true
      rescue StandardError, SystemStackError
        false
      end

      # Subscribes once per notifier. Returns true when subscribed.
      def install_active_job(notifications)
        return false if notifications.nil? || !notifications.respond_to?(:subscribe)
        return true if job_notifiers.any? { |n| n.equal?(notifications) }

        notifications.subscribe(JOB_EVENT) do |_name, _started, _finished, _id, payload|
          job_performed(payload)
        end
        job_notifiers << notifications
        true
      rescue StandardError, SystemStackError
        false
      end

      # A perform.active_job payload. ActiveJob puts the exception that
      # escaped #perform in :exception_object.
      def job_performed(payload)
        return nil unless payload.is_a?(Hash)

        error = payload[:exception_object]
        return nil if error.nil?

        Reporter.capture(error, context: 'job', handled: false, symbol: job_symbol(payload[:job]))
      rescue StandardError, SystemStackError
        nil
      end

      # Rails >= 7.1 puts the controller or job being run into the error
      # context (ActiveSupport::ExecutionContext).
      def symbol_from_context(context)
        return nil unless context.is_a?(Hash)

        controller = context[:controller]
        if controller.respond_to?(:action_name) && controller.action_name
          return "#{controller.class.name}##{controller.action_name}"
        end

        job_symbol(context[:job])
      rescue StandardError, SystemStackError
        nil
      end

      private

      def job_symbol(job)
        return nil if job.nil?

        "#{job.class.name}#perform"
      end

      def error_reporters
        @error_reporters ||= []
      end

      def job_notifiers
        @job_notifiers ||= []
      end
    end
  end
end
