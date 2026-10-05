# frozen_string_literal: true

require_relative 'current'

module Devbench
  # Who was affected (docs/SERVER_SDK_SPEC.md, Capability 5).
  #
  # Identity travels in its own field of every control message and nowhere
  # else: never in a message, a symbol or a log line, so it can never move a
  # fingerprint.
  module Identity
    MAX = 255

    # Returns a frozen {email:, account:} hash with empty fields dropped, or
    # nil when neither is present.
    def self.normalize(email, account)
      user = {}

      email = text(email).strip.downcase
      user[:email] = email[0, MAX] unless email.empty?

      account = text(account).strip
      user[:account] = account[0, MAX] unless account.empty?

      user.empty? ? nil : user.freeze
    end

    # An identity that cannot be read is dropped rather than raised: this is
    # called from a before_action, and a raise there fails the request.
    def self.text(value)
      return '' if value.nil?

      value.to_s.encode('UTF-8', invalid: :replace, undef: :replace).scrub
    rescue StandardError, SystemStackError
      ''
    end
    private_class_method :text
  end

  # Records who the current request is for. Call once per request, typically
  # from a before_action; Devbench::Middleware clears it when the request ends.
  #
  #   Devbench.set_user(email: current_user.email, account: current_account.id)
  #
  # Each field is optional. Calling again replaces the whole identity, and
  # calling with neither clears it. Never raises.
  def self.set_user(email: nil, account: nil)
    Current.user = Identity.normalize(email, account)
    nil
  rescue StandardError, SystemStackError
    nil
  end
end
