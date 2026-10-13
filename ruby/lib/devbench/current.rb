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
    WIDGET_USER_KEY = :__adt_widget_user
    SCOPE_KEY = :__adt_scope
    EXCEPTION_KEY = :__adt_exception_reported

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

    # Whether an `exception` was reported during this scope (any context:
    # request, rails_error, job, explicit). A 5xx after one is not also a
    # `failed` request: the exception is the report (Capability 2b). A
    # handled failure (report_handled) does not set it.
    def self.exception_reported?
      Thread.current[EXCEPTION_KEY] == true
    end

    def self.record_exception
      Thread.current[EXCEPTION_KEY] = true
    end

    # Who this request is for, as set by Devbench.set_user: a frozen hash with
    # :email and/or :account, or nil. Normalised before it gets here.
    def self.user
      Thread.current[USER_KEY]
    end

    def self.user=(value)
      Thread.current[USER_KEY] = value
    end

    # The raw identity, role and chat permission Devbench.set_user was given,
    # for the chat widget's signed token (Devbench::WidgetIdentity); nil when
    # no user is set. Never sent with sensor data.
    def self.widget_user
      Thread.current[WIDGET_USER_KEY]
    end

    def self.widget_user=(value)
      Thread.current[WIDGET_USER_KEY] = value
    end

    def self.trace
      Thread.current[KEY]
    end

    def self.trace=(value)
      Thread.current[KEY] = value
    end

    # True while a scope (Current.with: a request in the middleware, a
    # Sidekiq job, an ActiveJob job) is open on this thread, traced or not.
    # An ActiveJob job run inside one (the inline adapter in a request,
    # ActiveJob on Sidekiq) belongs to it and opens no second scope.
    def self.scoped?
      Thread.current[SCOPE_KEY] == true
    end

    # Runs the block with trace active, restoring whatever was there before.
    #
    # Restores rather than clearing, because servers reuse threads: clearing
    # would leak one request's absence into the next request's presence.
    def self.with(trace)
      previous = Thread.current[KEY]
      previous_handled = Thread.current[HANDLED_KEY]
      previous_user = Thread.current[USER_KEY]
      previous_widget_user = Thread.current[WIDGET_USER_KEY]
      previous_scope = Thread.current[SCOPE_KEY]
      previous_exception = Thread.current[EXCEPTION_KEY]
      Thread.current[KEY] = trace
      Thread.current[SCOPE_KEY] = true
      # Reset rather than inherit: servers reuse threads, and carrying a count
      # across requests would mark an innocent response as having swallowed
      # the previous request's failure.
      Thread.current[HANDLED_KEY] = 0
      # Same for identity, and more so: inheriting it would attribute one
      # user's failure to whoever this thread served before.
      Thread.current[USER_KEY] = nil
      Thread.current[WIDGET_USER_KEY] = nil
      Thread.current[EXCEPTION_KEY] = nil
      yield
    ensure
      # An exception reported in a nested scope (a second middleware, a job
      # run inside the request) was reported during the enclosing one too.
      nested_exception = previous_scope == true && Thread.current[EXCEPTION_KEY] == true
      Thread.current[EXCEPTION_KEY] = nested_exception ? true : previous_exception
      Thread.current[KEY] = previous
      Thread.current[HANDLED_KEY] = previous_handled
      Thread.current[USER_KEY] = previous_user
      Thread.current[WIDGET_USER_KEY] = previous_widget_user
      Thread.current[SCOPE_KEY] = previous_scope
    end

    def self.clear
      Thread.current[KEY] = nil
      Thread.current[HANDLED_KEY] = nil
      Thread.current[USER_KEY] = nil
      Thread.current[WIDGET_USER_KEY] = nil
      Thread.current[SCOPE_KEY] = nil
      Thread.current[EXCEPTION_KEY] = nil
    end
  end
end
