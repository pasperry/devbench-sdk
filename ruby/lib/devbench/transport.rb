# frozen_string_literal: true

require_relative 'config'
require_relative 'sidecar_transport'
require_relative 'direct'
require_relative 'logs'

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
      # Hooks already in place follow the new configuration: on in direct
      # mode, inert otherwise.
      Logs.hooked? && direct? ? Logs.activate! : Logs.deactivate!
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

    # The DSN the browser sensor may hold — https://<public>@host — derived
    # from DEVBENCH_DSN, so a page needs no setting of its own. Nil when the
    # DSN has no public part (a 0.5 single-key DSN: that key is a server
    # secret), does not parse, or reporting is disabled. Never raises, and
    # never returns anything carrying the secret.
    def browser_dsn
      cfg = config
      return nil unless cfg.enabled?

      raw = cfg.dsn.to_s
      cached = @browser_dsn
      return cached[1] if cached && cached[0] == raw

      value = raw.strip.empty? ? nil : DSN.parse(raw).browser_dsn
      @browser_dsn = [raw, value]
      value
    rescue StandardError, SystemStackError
      nil
    end

    # Where the browser sensor connects, for the app's CSP connect_src:
    # the ingest host from DEVBENCH_DSN and evidence storage. Read when the
    # CSP initializer runs, so the policy follows the DSN instead of a host
    # frozen at generate time; empty when there is no usable DSN, so an app
    # without Dev Bench configured (e.g. production) keeps its policy as is.
    # `bin/rails generate devbench` adds `*Devbench.csp_connect_sources`.
    STORAGE_SOURCE = 'https://*.storage.supabase.co'

    def csp_connect_sources
      return [] if browser_dsn.nil?

      [DSN.parse(config.dsn.to_s).base, STORAGE_SOURCE]
    rescue StandardError, SystemStackError
      []
    end

    # True when this process reports straight to Dev Bench (a DSN is set
    # and parses). Builds the transport if it was not built yet.
    def direct?
      transport.is_a?(DirectTransport)
    rescue StandardError, SystemStackError
      false
    end

    # Keeps recent lines from these loggers, per trace, for triage to ask
    # for (direct mode only; elsewhere a no-op). The Railtie does this for
    # Rails.logger and Sidekiq.logger; a Rack app without Rails calls it
    # itself. Returns true when capture is on. Never raises.
    def capture_logs(*loggers)
      return false unless enabled? && direct?

      hooked = loggers.flatten.map { |l| Logs.install(l) }.compact
      Logs.activate! unless hooked.empty?
      Logs.active?
    rescue StandardError, SystemStackError
      false
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
                'environment (https://<public>:<secret>@<host>, from `adt dsn create`), or call Devbench.configure { |c| c.dsn = ... }.'
        return false
      end

      dsn = begin
        DSN.parse(cfg.dsn)
      rescue DSN::Invalid => e
        io.puts "DEVBENCH_DSN #{e.message}."
        return false
      end

      client = DirectTransport.new(dsn: dsn, service: cfg.resolved_service, release: cfg.resolved_release)
      client.self_test(io)
    rescue StandardError, SystemStackError => e
      io.puts "Dev Bench test failed: #{e.class}: #{e.message}"
      false
    end

    # Drops the active transport (stopping a direct client's thread) and the
    # configuration, so both are rebuilt from scratch. For tests and for
    # Devbench.configure.
    def reset!
      reset_transport!
      Logs.deactivate!
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
