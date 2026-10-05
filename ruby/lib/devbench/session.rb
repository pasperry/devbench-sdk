# frozen_string_literal: true

require 'base64'
require 'openssl'

module Devbench
  # Session tokens prove a browser was served by the customer's real backend.
  #
  # The ingest key is public — it ships in a JavaScript bundle, so anyone can
  # read it and post with it. A session token closes that gap without the
  # customer writing anything: the Rack middleware they already install for
  # trace propagation mints one and injects it into HTML responses.
  #
  # This must agree byte for byte with the Go SDK, because a token minted by a
  # Rails app is verified by ingest. Divergence produces a request that succeeds
  # locally and is rejected in production, with nothing to say why —
  # testdata/session_vectors.json is what holds the two together.
  module Session
    HEADER = 'x-adt-session'
    META_NAME = 'adt-session'
    PREFIX = 'adts1'

    # Deliberately long: the token asserts only "served by the real app", so a
    # long life costs little, and a single-page application does full page loads
    # rarely — a short TTL would expire mid-session and force a refresh endpoint,
    # which is more surface in the customer's app for no security gain.
    DEFAULT_TTL = 3600
    MAX_SUBJECT = 128

    class Expired < StandardError; end
    class Invalid < StandardError; end

    Token = Struct.new(:tenant, :subject, :expires)

    # Mints a token. subject is optional — pass a stable user identifier for
    # per-user attribution, or leave it nil.
    def self.mint(secret, tenant, subject: nil, ttl: DEFAULT_TTL)
      raise ArgumentError, 'secret is required' if secret.nil? || secret.strip.empty?
      raise ArgumentError, 'tenant is required' if tenant.nil? || tenant.strip.empty?

      subject = subject.to_s[0, MAX_SUBJECT]
      ttl = DEFAULT_TTL if ttl.nil? || ttl <= 0

      # The payload is pipe-delimited, so a pipe in a field would let a crafted
      # subject forge a different tenant.
      if tenant.include?('|') || subject.include?('|')
        raise ArgumentError, "tenant and subject must not contain '|'"
      end

      payload = "#{tenant}|#{Time.now.to_i + ttl}|#{subject}"
      encoded = b64(payload)

      "#{PREFIX}.#{encoded}.#{sign(secret, encoded)}"
    end

    # Verifies a token, returning a Token or raising Invalid/Expired.
    def self.verify(secret, token)
      raise Invalid if secret.nil? || secret.strip.empty?
      raise Invalid unless token.is_a?(String)

      parts = token.strip.split('.')
      raise Invalid unless parts.length == 3 && parts[0] == PREFIX

      # Constant time: a signature check that leaks timing can be brute-forced
      # a byte at a time.
      raise Invalid unless OpenSSL.secure_compare(parts[2], sign(secret, parts[1]))

      raw = unb64(parts[1])
      raise Invalid if raw.nil?

      fields = raw.split('|', 3)
      raise Invalid unless fields.length == 3

      expires = Integer(fields[1], exception: false)
      raise Invalid if expires.nil?

      # Expiry is checked after the signature, so an unsigned token can never
      # reveal whether a guessed expiry was plausible.
      raise Expired if Time.now.to_i > expires

      Token.new(fields[0], fields[2], Time.at(expires))
    end

    def self.sign(secret, encoded_payload)
      b64(OpenSSL::HMAC.digest('SHA256', secret, encoded_payload))
    end

    # Base64url without padding, matching Go's RawURLEncoding.
    def self.b64(bytes)
      Base64.urlsafe_encode64(bytes, padding: false)
    end

    def self.unb64(text)
      Base64.urlsafe_decode64(text)
    rescue ArgumentError
      nil
    end
  end
end
