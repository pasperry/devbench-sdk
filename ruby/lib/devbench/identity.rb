# frozen_string_literal: true

require_relative 'current'

module Devbench
  # Who was affected (docs/server-sdk/identity.md, Capability 5).
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
  #
  # role: and chat: are for the chat widget only (docs/server-sdk/chat-widget.md,
  # Capability 6): devbench_script_tag signs them, with the email and
  # account, into the widget's identity token when DEVBENCH_WIDGET_SECRET is
  # set. They are never sent with sensor data. role is the app's own role
  # name for the user (mapped to chat capabilities in the tenant's settings);
  # chat, when given, overrides that mapping: a list of capabilities
  # (:report, :status, :howto, :feature_requests, :handoff), [] for "may not
  # chat", nil (the default) for "not stated".
  def self.set_user(email: nil, account: nil, role: nil, chat: nil)
    user = Identity.normalize(email, account)
    Current.user = user
    # The raw values, not the sensor identity's truncated ones: the widget
    # signer refuses what it cannot sign exactly instead of signing a
    # different person.
    Current.widget_user = user && { email: email, account: account, role: role, chat: chat.is_a?(Array) ? chat.dup.freeze : chat }.freeze
    nil
  rescue StandardError, SystemStackError
    nil
  end
end
