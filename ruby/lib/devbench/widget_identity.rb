# frozen_string_literal: true

require 'base64'
require 'openssl'

module Devbench
  # The chat widget's signed identity (docs/SERVER_SDK_SPEC.md, "Capability 6
  # — Chat widget").
  #
  #   dbw1.<base64url(issued_at \n email \n account \n role \n chat)>.<base64url(HMAC-SHA256)>
  #
  # The app's server signs the signed-in user with the tenant's widget secret
  # (DEVBENCH_WIDGET_SECRET); the browser holds only the token, and the chat
  # service shows the widget only to a user whose token verifies. Every signer
  # must reproduce testdata/widget_identity_vectors.json byte for byte, or the
  # widget works in a unit test and never renders in production.
  #
  # Rails apps never call this: devbench_script_tag signs whatever
  # Devbench.set_user recorded for the request.
  module WidgetIdentity
    PREFIX = 'dbw1'
    # The closed list, in the token's canonical (byte) order. A new one is a
    # contract change, made in lockstep with the vectors.
    CAPABILITIES = %w[feature_requests handoff howto report status].freeze
    MAX = 255
    # ASCII whitespace only, as the contract names it. String#strip would
    # also remove a trailing NUL, which must survive the trim to be refused
    # (Go's signer refuses it): the two would disagree.
    TRIM = /\A[ \t\n\r\f\v]+|[ \t\n\r\f\v]+\z/
    # C0 controls and DEL, refused when left in a field after trimming (the
    # trimmed whitespace is C0 too, and fine at the ends). Identities are
    # compared and displayed; invisible bytes could make two look alike.
    CONTROL = /[\x00-\x1f\x7f]/
    NO_CHAT = 'none'

    # Raised internally for an identity that cannot be signed; never escapes.
    class Refused < StandardError; end

    # Returns the token, or nil when this identity must not be signed: no
    # secret, neither email nor account, a control character (U+0000-U+001F,
    # U+007F) left in a field after trimming, a field over 255 characters,
    # invalid text, or a chat capability not in CAPABILITIES (exact,
    # lowercase). Refuses rather than repairs: truncating would sign a
    # different person. The first refusal in the process is logged, once.
    #
    # chat: nil states nothing (the tenant's settings decide); [] means the
    # user may not chat; otherwise their capabilities, symbols or strings,
    # in any order, duplicates allowed. Never raises.
    def self.sign(secret, email: nil, account: nil, role: nil, chat: nil, issued_at: Time.now.to_i)
      token(secret.to_s, email, account, role, chat, issued_at)
    rescue Refused => e
      Devbench.warn_once(:widget_identity, "chat widget identity not signed: #{e.message}; " \
                                           'no chat widget for that user (further refusals are not logged)')
      nil
    rescue StandardError, SystemStackError
      Devbench.warn_once(:widget_identity, 'chat widget identity not signed: unexpected error; ' \
                                           'no chat widget for that user (further refusals are not logged)')
      nil
    end

    def self.token(secret, email, account, role, chat, issued_at)
      raise Refused, 'no widget secret (DEVBENCH_WIDGET_SECRET)' if secret.empty?

      email = field(email, 'email')
      account = field(account, 'account')
      role = field(role, 'role')
      # Simple lowercase mapping, as Go's strings.ToLower: Ruby's full
      # mapping turns U+0130 into two characters where Go makes one. It is
      # the only code point the two disagree on.
      email = email.tr("\u0130", 'i').downcase
      raise Refused, 'neither email nor account' if email.empty? && account.empty?

      ts = Integer(issued_at)
      raise Refused, 'issued before 1970' if ts.negative?

      raw = [ts.to_s, email, account, role, chat_field(chat)].join("\n")
      payload = b64(raw.b)
      "#{PREFIX}.#{payload}.#{b64(OpenSSL::HMAC.digest('SHA256', secret.b, "#{PREFIX}.#{payload}"))}"
    end
    private_class_method :token

    # Trimmed, valid UTF-8, no control character, at most MAX characters; else
    # Refused. nil is "".
    def self.field(value, name)
      text = utf8(value, name).gsub(TRIM, '')
      raise Refused, "control character in #{name}" if text.match?(CONTROL)
      raise Refused, "#{name} over #{MAX} characters" if text.length > MAX

      text
    end
    private_class_method :field

    def self.utf8(value, name)
      text = value.nil? ? +'' : value.to_s.dup
      text = text.encoding == Encoding::BINARY ? text.force_encoding(Encoding::UTF_8) : text.encode(Encoding::UTF_8)
      raise Refused, "#{name} is not valid UTF-8" unless text.valid_encoding?

      text
    rescue EncodingError
      raise Refused, "#{name} is not valid UTF-8"
    end
    private_class_method :utf8

    # "" not stated, "none" may not chat, else known names de-duplicated,
    # sorted by bytes, joined by ",".
    def self.chat_field(chat)
      return '' if chat.nil?
      raise Refused, 'chat must be a list of capabilities' unless chat.is_a?(Array)

      names = chat.map do |c|
        name = c.is_a?(Symbol) || c.is_a?(String) ? c.to_s : nil
        raise Refused, "unknown chat capability #{c.inspect}" unless CAPABILITIES.include?(name)

        name
      end
      names.empty? ? NO_CHAT : names.uniq.sort.join(',')
    end
    private_class_method :chat_field

    def self.b64(bytes)
      Base64.urlsafe_encode64(bytes, padding: false)
    end
    private_class_method :b64
  end
end
