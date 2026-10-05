# frozen_string_literal: true

require_relative 'config'
require_relative 'sidecar_transport'
require_relative 'direct'

# Configuration and transport selection (docs/SERVER_SDK_SPEC.md,
# "Transports: direct (default) or sidecar (optional)").
#
# Exactly one transport per process, decided at the first report:
#
#   DEVBENCH_ENABLED=false     -> none
#   DEVBENCH_DSN (or ADT_DSN)  -> direct to Dev Bench; a DSN that does not
#                                 parse logs one warning and reports nothing
#   neither                    -> the sidecar socket, as before 0.5
#
# Never both: a sidecar on the same host still reads logs, but writing to
# its socket as well would count every report twice.
module Devbench
  # Raised (and rescued) by Devbench.test! to make its synthetic report.
  class TestException < StandardError; end

  TRANSPORT_LOCK = Mutex.new
  CONFIG_LOCK = Mutex.new
  private_constant :TRANSPORT_LOCK, :CONFIG_LOCK

  class << self
    # The configuration, read from the environment on first use.
    def config
      @config || CONFIG_LOCK.synchronize { @config ||= Configuration.new }
    end

    # Configures from code instead of (or on top of) the environment, e.g.
    # in config/initializers/devbench.rb:
    #
    #   Devbench.configure do |c|
    #     c.dsn = Rails.application.credentials.devbench_dsn
    #     c.service = 'billing'
    #   end
    #
    # Takes effect from the next report. Never raises into the caller.
    def configure
      yield config if block_given?
      reset_transport!
      nil
    rescue StandardError, SystemStackError => e
      warn_once(:configure, "Devbench.configure raised #{e.class}; reporting is unchanged")
      nil
    end

    def enabled?
      config.enabled?
    rescue StandardError, SystemStackError
      false
    end

    # The active transport. Built once, on first use, so a DSN set from an
    # initializer is seen.
    def transport
      @transport || TRANSPORT_LOCK.synchronize { @transport ||= build_transport }
    end

    # Sends whatever direct mode has counted, now, waiting at most `timeout`
    # seconds. A no-op in sidecar mode. Returns true when nothing is left
    # unsent. Never raises.
    def flush!(timeout: 5)
      transport.flush!(timeout: timeout) ? true : false
    rescue StandardError, SystemStackError
      false
    end

    # Sends one synthetic exception straight to Dev Bench and prints the
    # result, so an operator can check the DSN without waiting for a flush
    # (`rake devbench:test` under Rails). Returns true when ingest accepted
    # it. Never raises.
    def test!(io: $stdout)
      cfg = config
      unless cfg.enabled?
        io.puts 'Dev Bench is disabled (DEVBENCH_ENABLED=false); nothing was sent.'
        return false
      end
      if cfg.dsn.nil? || cfg.dsn.strip.empty?
        io.puts 'DEVBENCH_DSN is not set. Set it to the DSN Dev Bench gave you for this ' \
                'environment (https://<key>@<host>), or call Devbench.configure { |c| c.dsn = ... }.'
        return false
      end

      dsn = begin
        DSN.parse(cfg.dsn)
      rescue DSN::Invalid => e
        io.puts "DEVBENCH_DSN #{e.message}."
        return false
      end

      error = begin
        raise TestException, 'Dev Bench test exception: if you can read this, the DSN works'
      rescue TestException => e
        e
      end
      payload = Reporter.report_for(error, context: 'explicit', handled: true, symbol: 'devbench:test')
      client = DirectTransport.new(dsn: dsn, service: cfg.resolved_service, release: cfg.resolved_release)
      client.self_test(payload, io)
    rescue StandardError, SystemStackError => e
      io.puts "Dev Bench test failed: #{e.class}: #{e.message}"
      false
    end

    # Drops the active transport (stopping a direct client's thread) and the
    # configuration, so both are rebuilt from scratch. For tests and for
    # Devbench.configure.
    def reset!
      reset_transport!
      CONFIG_LOCK.synchronize { @config = nil }
      nil
    end

    # Logs a problem with Dev Bench itself, once per kind per process. Never
    # raises: the logger is the application's.
    def warn_once(kind, message)
      @warned ||= {}
      return if @warned[kind]

      @warned[kind] = true
      line = "[devbench] #{message}"
      if defined?(::Rails) && ::Rails.respond_to?(:logger) && ::Rails.logger
        ::Rails.logger.warn(line)
      else
        Kernel.warn(line)
      end
      nil
    rescue StandardError, SystemStackError
      nil
    end

    private

    def reset_transport!
      old = TRANSPORT_LOCK.synchronize do
        previous = @transport
        @transport = nil
        previous
      end
      old.stop if old.respond_to?(:stop)
    end

    def build_transport
      cfg = config
      return NullTransport unless cfg.enabled?
      return SidecarTransport if cfg.dsn.nil? || cfg.dsn.strip.empty?

      dsn = begin
        DSN.parse(cfg.dsn)
      rescue DSN::Invalid => e
        warn_once(:dsn, "DEVBENCH_DSN #{e.message}; nothing will be reported")
        return NullTransport
      end

      install_exit_flush
      DirectTransport.new(dsn: dsn, service: cfg.resolved_service, release: cfg.resolved_release,
                          interval: cfg.flush_interval)
    rescue StandardError, SystemStackError
      NullTransport
    end

    # One last flush when the process exits, bounded to 2 s (spec). Inherited
    # by forked children, where it flushes the child's own counts. Not
    # destructive: a later report (e.g. from a test runner's at_exit) still
    # works.
    def install_exit_flush
      return if @exit_flush_installed

      @exit_flush_installed = true
      at_exit do
        current = @transport
        current.flush!(timeout: DirectTransport::EXIT_TIMEOUT) if current.is_a?(DirectTransport)
      rescue StandardError, SystemStackError
        nil
      end
    end
  end
end
