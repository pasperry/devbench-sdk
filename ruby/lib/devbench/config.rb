# frozen_string_literal: true

require 'uri'

module Devbench
  # Where reports go, parsed from DEVBENCH_DSN (docs/SERVER_SDK_SPEC.md, "Dev
  # Bench naming and configuration"):
  #
  #   https://<ingest key>@<host>[:port]
  #
  # scheme://host[:port] is the ingest base and the userinfo is the key. A
  # path or query, if present, is ignored: the key alone identifies tenant,
  # environment and source.
  class DSN
    class Invalid < StandardError; end

    LOOPBACK = /\A(?:localhost|127(?:\.\d{1,3}){3}|::1|\[::1\])\z|\.localhost\z/i

    attr_reader :base, :key

    def initialize(base, key)
      @base = base.freeze
      @key = key.freeze
      freeze
    end

    def flush_url
      "#{@base}/v1/flush"
    end

    # The DSN without its key, for messages and logs. The key is a secret
    # on a server: it must never be printed.
    def to_s
      @base
    end

    alias inspect to_s

    # Raises DSN::Invalid with a message that says what to fix. Never echoes
    # the key back.
    def self.parse(raw)
      text = raw.to_s.strip
      raise Invalid, 'is empty' if text.empty?

      uri = begin
        URI.parse(text)
      rescue URI::Error
        raise Invalid, 'is not a URL (expected https://<key>@<host>)'
      end

      scheme = uri.scheme.to_s.downcase
      raise Invalid, 'must start with https:// (expected https://<key>@<host>)' unless %w[http https].include?(scheme)

      host = uri.host.to_s
      raise Invalid, 'has no host (expected https://<key>@<host>)' if host.empty?

      key = URI.decode_www_form_component(uri.user.to_s)
      raise Invalid, 'has no key (expected https://<key>@<host>)' if key.strip.empty?

      # The key is a real secret on a server. Over plain http it would cross
      # the network readable by anything on the path, so http is accepted
      # only for a loopback ingest (local development and tests).
      if scheme == 'http' && !LOOPBACK.match?(host)
        raise Invalid, "uses http:// for #{host}: the key would cross the network in plaintext; use https://"
      end

      port = uri.port && uri.port != uri.default_port ? ":#{uri.port}" : ''
      host = "[#{host}]" if host.include?(':') && !host.start_with?('[')
      new("#{scheme}://#{host}#{port}", key.strip)
    end
  end

  # Everything Dev Bench reads at start. From the environment by default;
  # Devbench.configure sets the same fields from code.
  #
  #   DEVBENCH_DSN       where to send (fallback ADT_DSN). Unset: the local
  #                      sidecar's socket, as before 0.5.
  #   DEVBENCH_SERVICE   this app's name. Default: the Rails application's
  #                      module, underscored (AcmeShop -> acme_shop);
  #                      else "app".
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
    def resolved_service
      presence(@service) || rails_service || 'app'
    end

    def resolved_release
      @release.to_s
    end

    private

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
