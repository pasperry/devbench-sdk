# frozen_string_literal: true

require_relative 'session'

module Devbench
  # Rack middleware that injects a session token into HTML responses.
  #
  # Automatic on purpose. The customer already installs Devbench::Middleware for
  # trace propagation; making them also thread a token through every layout
  # would be real work in their codebase for something that can be done here.
  #
  #   config.middleware.insert_before 0, Devbench::SessionMiddleware,
  #     secret: ENV['ADT_SESSION_SECRET'], tenant: 'acme'
  #
  # Only HTML responses are touched. JSON, images, and downloads pass through.
  class SessionMiddleware
    def initialize(app, secret:, tenant:, ttl: Session::DEFAULT_TTL, subject: nil)
      @app = app
      @secret = secret
      @tenant = tenant
      @ttl = ttl
      @subject = subject
    end

    def call(env)
      status, headers, body = @app.call(env)
      return [status, headers, body] if @secret.to_s.empty? || @tenant.to_s.empty?
      return [status, headers, body] unless html?(headers)

      token = begin
        Session.mint(@secret, @tenant, subject: @subject&.call(env), ttl: @ttl)
      rescue StandardError
        # Never fail a page over instrumentation.
        return [status, headers, body]
      end

      rendered = +''
      body.each { |chunk| rendered << chunk }
      body.close if body.respond_to?(:close)

      injected = inject(rendered, token)

      # A stale Content-Length truncates the page in the browser.
      headers = headers.dup
      headers.delete('Content-Length')
      headers.delete('content-length')
      headers['Content-Length'] = injected.bytesize.to_s

      [status, headers, [injected]]
    end

    private

    def html?(headers)
      value = headers.find { |k, _| k.to_s.downcase == 'content-type' }&.last
      value.to_s.downcase.include?('text/html')
    end

    def inject(document, token)
      tag = %(<meta name="#{Session::META_NAME}" content="#{escape(token)}">)

      idx = document =~ /<head[^>]*>/i
      return document if idx.nil?

      insert_at = idx + Regexp.last_match(0).length
      document[0...insert_at] + tag + document[insert_at..]
    end

    def escape(text)
      text.gsub('&', '&amp;').gsub('"', '&quot;').gsub('<', '&lt;').gsub('>', '&gt;')
    end
  end
end
