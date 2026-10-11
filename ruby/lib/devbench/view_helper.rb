# frozen_string_literal: true

require 'erb'
require_relative 'version'
require_relative 'current'
require_relative 'widget_identity'

module Devbench
  # The browser sensor's script tag, for the layout's <head>:
  #
  #   <%= devbench_script_tag %>
  #
  # renders
  #
  #   <script src="https://unpkg.com/devbench@0.7.0/dist/devbench.min.js"
  #           data-dsn="https://<public>@<host>" data-release="<release>" defer></script>
  #
  # The DSN is the public part of DEVBENCH_DSN only (Devbench.browser_dsn),
  # so the page needs no setting of its own and the secret never reaches
  # HTML. No public part (no DSN, a 0.5 single-key DSN, reporting disabled)
  # renders nothing at all. The src is pinned to this gem's version, so a
  # gem upgrade upgrades the browser sensor with it. Where the app uses a
  # CSP nonce (content_security_policy_nonce_generator), the tag carries it,
  # as Rails' own `javascript_include_tag nonce: true` does.
  #
  # Chat widget (docs/SERVER_SDK_SPEC.md, Capability 6): when
  # DEVBENCH_WIDGET_SECRET is set and Devbench.set_user was called for this
  # request, the tag also carries data-widget-identity="<token>", the user
  # (and their role and chat permission) signed at render time. No secret, no
  # user (a signed-out page) or an identity the signer refuses: no
  # attribute, so no widget. A page carrying one is per-user; don't serve it
  # from a shared cache.
  #
  # Included into ActionView by the Railtie. Never raises.
  module ViewHelper
    CDN = 'https://unpkg.com'

    def self.src
      "#{CDN}/devbench@#{Devbench::VERSION}/dist/devbench.min.js"
    end

    def devbench_script_tag
      dsn = Devbench.browser_dsn
      return devbench_safe('') if dsn.nil?

      attrs = [['src', ViewHelper.src], ['data-dsn', dsn], ['data-release', Devbench.config.resolved_release]]
      identity = devbench_widget_identity
      attrs << ['data-widget-identity', identity] if identity
      nonce = devbench_nonce
      attrs << ['nonce', nonce] if nonce
      html = attrs.map { |name, value| %(#{name}="#{ERB::Util.html_escape(value)}") }.join(' ')
      devbench_safe("<script #{html} defer></script>")
    rescue StandardError, SystemStackError
      devbench_safe('')
    end

    private

    # The signed widget identity for this request's user, or nil.
    def devbench_widget_identity
      secret = Devbench.config.widget_secret
      user = Current.widget_user
      return nil if secret.nil? || secret.to_s.empty? || user.nil?

      WidgetIdentity.sign(secret, email: user[:email], account: user[:account], role: user[:role], chat: user[:chat])
    rescue StandardError, SystemStackError
      nil
    end

    # Rails' content_security_policy_nonce: nil unless the app configured a
    # nonce generator.
    def devbench_nonce
      return nil unless respond_to?(:content_security_policy_nonce, true)

      nonce = content_security_policy_nonce
      nonce.nil? || nonce.to_s.empty? ? nil : nonce.to_s
    rescue StandardError, SystemStackError
      nil
    end

    def devbench_safe(html)
      html.respond_to?(:html_safe) ? html.html_safe : html
    end
  end
end
