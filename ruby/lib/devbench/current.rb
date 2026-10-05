# frozen_string_literal: true

module Devbench
  # Per-request storage for the active trace.
  #
  # Plain thread/fiber-local rather than ActiveSupport::CurrentAttributes, so
  # the core of this gem loads and is testable without Rails. The Railtie wires
  # CurrentAttributes semantics where Rails is present.
  module Current
    KEY = :__adt_trace
    HANDLED_KEY = :__adt_handled
    USER_KEY = :__adt_user

    # Counts failures handled during this request.
    #
    # The middleware turns a non-zero count into a response header, which is
    # what lets the browser decide — at the moment it has both facts — whether
    # the user was told. See docs/DETECTORS.md.
    def self.handled_count
      Thread.current[HANDLED_KEY] || 0
    end

    def self.record_handled
      Thread.current[HANDLED_KEY] = handled_count + 1
    end

    # Who this request is for, as set by Devbench.set_user: a frozen hash with
    # :email and/or :account, or nil. Normalised before it gets here.
    def self.user
      Thread.current[USER_KEY]
    end

    def self.user=(value)
      Thread.current[USER_KEY] = value
    end

    def self.trace
      Thread.current[KEY]
    end

    def self.trace=(value)
      Thread.current[KEY] = value
    end

    # Runs the block with trace active, restoring whatever was there before.
    #
    # Restores rather than clearing, because servers reuse threads: clearing
    # would leak one request's absence into the next request's presence.
    def self.with(trace)
      previous = Thread.current[KEY]
      previous_handled = Thread.current[HANDLED_KEY]
      previous_user = Thread.current[USER_KEY]
      Thread.current[KEY] = trace
      # Reset rather than inherit: servers reuse threads, and carrying a count
      # across requests would mark an innocent response as having swallowed
      # the previous request's failure.
      Thread.current[HANDLED_KEY] = 0
      # Same for identity, and more so: inheriting it would attribute one
      # user's failure to whoever this thread served before.
      Thread.current[USER_KEY] = nil
      yield
    ensure
      Thread.current[KEY] = previous
      Thread.current[HANDLED_KEY] = previous_handled
      Thread.current[USER_KEY] = previous_user
    end

    def self.clear
      Thread.current[KEY] = nil
      Thread.current[HANDLED_KEY] = nil
      Thread.current[USER_KEY] = nil
    end
  end
end
