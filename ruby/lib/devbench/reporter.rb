# frozen_string_literal: true

require_relative 'current'
require_relative 'backtrace'
require_relative 'transport'

module Devbench
  # Turns a failure into a report — the control message of
  # docs/SERVER_SDK_SPEC.md, "Control socket — v1" — and hands it to the
  # active transport (Devbench.transport): direct to Dev Bench when a DSN is
  # set, else the local sidecar's socket.
  #
  # The report is built the same way for both, so the direct transport
  # fingerprints exactly what the sidecar would have received.
  #
  # Every failure here is swallowed. This is the one place in the codebase
  # where that is correct — everywhere else it is the exact behavior the
  # product exists to find.
  module Reporter
    DEFAULT_SOCKET = '/tmp/adt-sidecar.sock'

    # docs/SERVER_SDK_SPEC.md, "Control socket — v1".
    MAX_MESSAGE = 2000
    MAX_HANDLED_MESSAGE = 500

    CONTEXTS = %w[request rails_error job explicit].freeze

    # Status codes, not failures. Matched by name against the exception's
    # class and ancestors, so none of these constants needs to be loaded —
    # this gem must work without Rails. Sentry ignores the same by default.
    DEFAULT_IGNORED = %w[
      ActionController::RoutingError
      ActiveRecord::RecordNotFound
      ActionController::InvalidAuthenticityToken
      ActionController::UnknownFormat
      ActionDispatch::Http::MimeNegotiation::InvalidType
    ].freeze

    # Process control, not application failures: a deploy's SIGTERM or an
    # `exit` passing through the middleware is not something to triage.
    #
    # Sidekiq's own control flow likewise (Sidekiq::Shutdown is an Interrupt,
    # so already a SignalException). JobRetry::Handled (and its subclass
    # Skip) means "the job failed and Sidekiq has dealt with it" — the real
    # failure is its cause, reported on its own; Rails' executor around a
    # Sidekiq job would otherwise hand it to Rails.error. Job::Interrupted is
    # an iterable job being re-queued at shutdown.
    NEVER_REPORTED = %w[
      SystemExit SignalException
      Sidekiq::JobRetry::Handled Sidekiq::Job::Interrupted
    ].freeze

    REPORTED_IVAR = :@__adt_reported

    class << self
      attr_writer :socket_path, :app_root

      def socket_path
        @socket_path ||= ENV.fetch('ADT_SIDECAR_SOCKET', DEFAULT_SOCKET)
      end

      # Class names never reported. Extend it from an initializer:
      #
      #   Devbench.ignore_exceptions << 'Pundit::NotAuthorizedError'
      def ignore_exceptions
        @ignore_exceptions ||= DEFAULT_IGNORED.dup
      end

      # Frames under this directory are sent relative to it. Rails.root when
      # Rails is loaded, else the working directory.
      def app_root
        return @app_root if @app_root

        if defined?(::Rails) && ::Rails.respond_to?(:root) && ::Rails.root
          ::Rails.root.to_s
        else
          Dir.pwd
        end
      rescue StandardError, SystemStackError
        nil
      end

      # Records that a failure was handled here. Does not change behavior.
      #
      #   rescue ActiveRecord::RecordInvalid => e
      #     Devbench.report_handled(e, symbol: 'CustomersController#update', reason: 'validation')
      def report_handled(error, symbol:, reason: nil)
        # Counted first, and regardless of whether anything after it works.
        # The count is what the browser is told; a sidecar that is down, or an
        # error object that misbehaves, must not also make the failure
        # invisible to the detector that does not need either.
        begin
          Current.record_handled
        rescue StandardError, SystemStackError
          nil
        end

        emit(handled_payload(error, symbol, reason))
        nil
      rescue StandardError, SystemStackError
        # This runs inside the customer's rescue blocks. Anything raised here
        # means their render never runs — a handled failure turned into an
        # outage by the tool watching for failures.
        nil
      end

      # Sends an `exception` message, once per exception object.
      #
      # Called from the middleware's rescue, from Rails.error, from ActiveJob
      # instrumentation and from Devbench.capture_exception. Anything raised here
      # would replace the application's own exception, so nothing is.
      def capture(error, context:, handled:, symbol: nil)
        return nil unless error.is_a?(Exception)
        return nil if reported?(error) || ignored?(error)

        # Marked before sending, so a second hook reached while this one is
        # still writing does not send it again.
        mark_reported(error)
        emit(exception_payload(error, context, handled, symbol))
        nil
      rescue StandardError, SystemStackError
        nil
      end

      # Whether an exception has already been sent by any hook.
      def reported?(error)
        if error.instance_variable_defined?(REPORTED_IVAR)
          true
        else
          frozen_reported.key?(error)
        end
      rescue StandardError, SystemStackError
        false
      end

      def ignored?(error)
        names = error.class.ancestors.map(&:name)
        return true if (names & NEVER_REPORTED).any?

        (names & ignore_exceptions.map(&:to_s)).any?
      rescue StandardError, SystemStackError
        false
      end

      # The `exception` report for an error, without sending it. Used by
      # the self-test (Devbench.test!), which sends one synthetic report
      # straight to ingest, built exactly as a real one would be.
      def report_for(error, context:, handled:, symbol: nil)
        exception_payload(error, context, handled, symbol)
      end

      private

      def mark_reported(error)
        error.instance_variable_set(REPORTED_IVAR, true)
      rescue FrozenError
        # A frozen exception cannot carry the mark. Held weakly, so the
        # process does not keep every frozen exception it ever saw.
        frozen_reported[error] = true
      end

      def frozen_reported
        @frozen_reported ||= ObjectSpace::WeakMap.new
      end

      # Hands the report to whichever transport is active: direct (DSN set),
      # the sidecar socket, or none. Each one returns at once and swallows its
      # own failures; this guard is for the dispatch itself.
      def emit(payload)
        Devbench.transport.deliver(payload)
      rescue StandardError, SystemStackError
        nil
      end

      # Each field is read on its own: an exception whose #message raises
      # still reports its class and site, which are the fingerprint. The
      # message is detail.
      def handled_payload(error, symbol, reason)
        {
          v: 1,
          kind: 'handled_failure',
          symbol: symbol,
          error: safely { error.class.name },
          message: safely { truncate(error.message, MAX_HANDLED_MESSAGE) },
          reason: reason,
          trace: safely { Current.trace&.to_s },
          user: safely { Current.user }
        }.compact
      end

      def exception_payload(error, context, handled, symbol)
        {
          v: 1,
          kind: 'exception',
          context: CONTEXTS.include?(context) ? context : 'explicit',
          handled: handled ? true : false,
          error: safely { class_name(error) } || 'Exception',
          message: safely { truncate(error.message, MAX_MESSAGE) },
          symbol: safely { symbol.nil? ? nil : utf8(symbol.to_s) } || '',
          frames: safely { Backtrace.frames(error.backtrace, app_root) } || [],
          trace: safely { Current.trace&.to_s },
          user: safely { Current.user }
        }.compact
      end

      # An anonymous class has no name; its nearest named ancestor is stable
      # across processes, where Class#to_s would print an address.
      def class_name(error)
        error.class.ancestors.find { |mod| mod.is_a?(Class) && mod.name }&.name
      end

      def safely
        yield
      rescue StandardError, SystemStackError
        nil
      end

      # At most `limit` characters, including the ellipsis.
      def truncate(text, limit)
        text = utf8(text.to_s)
        text.length > limit ? "#{text[0, limit - 1]}…" : text
      end

      def utf8(text)
        text.encode('UTF-8', invalid: :replace, undef: :replace).scrub
      end
    end
  end

  # Reports an exception the application caught and wants seen, as Sentry's
  # capture_exception does. Returns nil and never raises.
  #
  #   rescue Faraday::Error => e
  #     Devbench.capture_exception(e, symbol: 'Billing::Sync#run')
  def self.capture_exception(error, symbol: nil)
    Reporter.capture(error, context: 'explicit', handled: true, symbol: symbol)
  end

  def self.report_handled(error, symbol:, reason: nil)
    Reporter.report_handled(error, symbol: symbol, reason: reason)
  end

  # Class names never reported as exceptions (see Reporter::DEFAULT_IGNORED).
  def self.ignore_exceptions
    Reporter.ignore_exceptions
  end
end
