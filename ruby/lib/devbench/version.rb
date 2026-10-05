# frozen_string_literal: true

module Devbench
  # Tracks the protocol, not the gem's own churn.
  #
  # The wire format this speaks — the trace header, the handled header, the
  # control-socket message shapes — is what a customer's deployment depends
  # on. See docs/SERVER_SDK_SPEC.md.
  VERSION = '0.5.0'
end
