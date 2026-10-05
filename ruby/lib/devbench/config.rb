# frozen_string_literal: true

require 'uri'

module Devbench
  # Where reports go, parsed from DEVBENCH_DSN (docs/SERVER_SDK_SPEC.md, "Dev
  # Bench naming and configuration"; DECISIONS #161):
  #
  #   https://<public>:<secret>@<host>[:port]    from `adt dsn create`
  #   https://<key>@<host>[:port]                0.5 and earlier: one key
  #
  # scheme://host[:port] is the ingest base. The secret (a server key) is
  # what this process authenticates with; the public part (a browser key) is
  # only ever handed to the page, via #browser_dsn. A single-key DSN keeps
  # working with its one key and has no browser part — that key is a server
  # secret and must never reach a page. A path or query, if present, is
  # ignored: the keys alone identify tenant, environment and source.
  class DSN
    class Invalid < StandardError; end

    LOOPBACK = /\A(?:localhost|127(?:\.\d{1,3}){3}|::1|\[::1\])\z|\.localhost\z/i
    SHAPE = 'https://<public>:<secret>@<host>'

    attr_reader :base, :key, :public_key

    # key: what the server sends as X-ADT-Key (the secret, or a 0.5 DSN's
    # single key). public_key: the browser key, or nil.
    def initialize(base, key, public_key = nil)
      @base = base.freeze
      @key = key.freeze
      @public_key = public_key&.freeze
      freeze
    end

    def flush_url
      "#{@base}/v1/flush"
    end

    # The self-test endpoint: validates the key, stores nothing.
    def check_url
      "#{@base}/v1/check"
    end

    def logs_url
      "#{@base}/v1/logs"
    end

    # The DSN a browser may hold: https://<public>@host. Nil when there is
    # no public part.
    def browser_dsn
      return nil if @public_key.nil?

      scheme, rest = @base.split('://', 2)
      "#{scheme}://#{URI.encode_www_form_component(@public_key)}@#{rest}"
    end

    # The DSN without its keys, for messages and logs. The secret must never
    # be printed.
    def to_s
      @base
    end

    alias inspect to_s

    # Raises DSN::Invalid with a message that says what to fix. Never echoes
    # either key back.
    def self.parse(raw)
      text = raw.to_s.strip
      raise Invalid, 'is empty' if text.empty?

      uri = begin
        URI.parse(text)
      rescue URI::Error
        raise Invalid, "is not a URL (expected #{SHAPE})"
      end

      scheme = uri.scheme.to_s.downcase
      raise Invalid, "must start with https:// (expected #{SHAPE})" unless %w[http https].include?(scheme)

      host = uri.host.to_s
      raise Invalid, "has no host (expected #{SHAPE})" if host.empty?

      user = URI.decode_www_form_component(uri.user.to_s).strip
      secret = URI.decode_www_form_component(uri.password.to_s).strip
      raise Invalid, "has no key (expected #{SHAPE})" if user.empty? && secret.empty?

      # The secret is a real secret on a server. Over plain http it would
      # cross the network readable by anything on the path, so http is
      # accepted only for a loopback ingest (local development and tests).
      if scheme == 'http' && !LOOPBACK.match?(host)
        raise Invalid, "uses http:// for #{host}: the key would cross the network in plaintext; use https://"
      end

      port = uri.port && uri.port != uri.default_port ? ":#{uri.port}" : ''
      host = "[#{host}]" if host.include?(':') && !host.start_with?('[')
      base = "#{scheme}://#{host}#{port}"
      return new(base, user) if secret.empty?

      new(base, secret, user.empty? ? nil : user)
    end
  end

  # Everything Dev Bench reads at start. From the environment by default;
  # Devbench.configure sets the same fields from code.
  #
  #   DEVBENCH_DSN       where to send (fallback ADT_DSN). Unset: the local
  #                      sidecar's socket, as before 0.5.
  #   DEVBENCH_SERVICE   this app's name. Default: the Rails application's
  #                      module, underscored (AcmeShop -> acme_shop);
  #                      else "app"; with "-sidekiq" appended in a
  #                      Sidekiq process.
  #   DEVBENCH_RELEASE   the deployed version. Default: GIT_SHA,
  #                      SOURCE_VERSION, RENDER_GIT_COMMIT, else "".
  #   DEVBENCH_ENABLED   "false" turns everything off.
  class Configuration
    RELEASE_FALLBACKS = %w[GIT_SHA SOURCE_VERSION RENDER_GIT_COMMIT].freeze
    OFF = %w[false 0 no off].freeze

    # Strings, or nil for "not set".
    attr_accessor :dsn, :service, :release
    # Seconds between background flushes in direct mode (spec: 60).
    attr_accessor :flush_interval
    attr_writer :enabled

    def initialize(env = ENV)
      @dsn = presence(env['DEVBENCH_DSN']) || presence(env['ADT_DSN'])
      @service = presence(env['DEVBENCH_SERVICE'])
      @release = presence(env['DEVBENCH_RELEASE']) ||
                 RELEASE_FALLBACKS.lazy.map { |name| presence(env[name]) }.find(&:itself)
      @enabled = !OFF.include?(env['DEVBENCH_ENABLED'].to_s.strip.downcase)
      @flush_interval = 60
    end

    def enabled?
      @enabled ? true : false
    end

    # The service name sent with every flush and folded into every
    # fingerprint. Resolved when reporting starts, after Rails has booted.
    #
    # A Sidekiq process (Sidekiq.server?) defaults to "<app>-sidekiq", so a
    # typical app's web and job processes are told apart with no
    # DEVBENCH_SERVICE at all. An explicit service always wins.
    def resolved_service
      explicit = presence(@service)
      return explicit if explicit

      base = rails_service || 'app'
      sidekiq_server? ? "#{base}-sidekiq" : base
    end

    def resolved_release
      @release.to_s
    end

    private

    def sidekiq_server?
      defined?(::Sidekiq) && ::Sidekiq.respond_to?(:server?) && ::Sidekiq.server? ? true : false
    rescue StandardError, SystemStackError
      false
    end

    def presence(value)
      text = value.to_s.strip
      text.empty? ? nil : text
    end

    # Rails.application.class.module_parent_name.underscore, without
    # assuming ActiveSupport's inflections are loaded.
    def rails_service
      return nil unless defined?(::Rails) && ::Rails.respond_to?(:application) && ::Rails.application

      klass = ::Rails.application.class
      name = klass.respond_to?(:module_parent_name) ? klass.module_parent_name : klass.name.to_s.split('::').first
      return nil if name.nil? || name.empty? || name == 'Object'

      name.respond_to?(:underscore) ? name.underscore : underscore(name)
    rescue StandardError, SystemStackError
      nil
    end

    def underscore(name)
      name.gsub('::', '/')
          .gsub(/([A-Z\d]+)([A-Z][a-z])/, '\1_\2')
          .gsub(/([a-z\d])([A-Z])/, '\1_\2')
          .tr('-', '_')
          .downcase
    end
  end
end
