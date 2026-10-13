# frozen_string_literal: true

require_relative 'trace'
require_relative 'current'

module Devbench
  # Carries the request's trace into ActiveJob jobs on every adapter (Solid
  # Queue, GoodJob, async, inline, Sidekiq, ...), so a job's failure and its
  # log lines join the user action that enqueued it (docs/server-sdk/trace-propagation.md,
  # Capability 1; README "Server log lines").
  #
  # The trace travels inside the job's own serialized payload, which every
  # adapter stores and hands back verbatim to ActiveJob::Base.execute:
  #
  #   * serialize   — adds "adt_trace": the trace the job already carries (a
  #                   job re-enqueued by retry_on keeps the request it came
  #                   from), else the active one. Unchanged, as the Sidekiq
  #                   client middleware carries it: a job is the same
  #                   service, not a downstream hop.
  #   * deserialize — reads it back. A payload from an older gem has no key:
  #                   the job runs untraced.
  #   * perform_now — a job that came through deserialize runs inside
  #                   Current.with(its trace), outermost (before ActiveJob's
  #                   logging and instrumentation), so the perform.active_job
  #                   report and every line the job logs carry it; the
  #                   thread's previous trace and identity come back after.
  #                   Identity is scoped to the job, never inherited from the
  #                   thread (a pool thread's last job is not this job's user).
  #
  # Not twice: a job that runs inside a scope already open on its thread —
  # Sidekiq's server middleware (which scoped the trace from the outer job
  # hash), or the request whose inline adapter runs it — belongs to that
  # scope and opens no second one, unless it carries a different trace. A
  # job built and run with perform_now (never serialized) is left alone: it
  # runs in its caller's context.
  #
  # Plain Ruby: no ActiveJob constant is named at load time. The Railtie
  # installs it with ActiveSupport.on_load(:active_job); ActiveJob without
  # Rails calls Devbench::ActiveJobHooks.install(ActiveJob::Base). Every
  # override is guarded: the job's own behaviour and its own exception are
  # what come out, whatever happens in here.
  module ActiveJobHooks
    # The payload key, the same one Sidekiq jobs carry. ActiveJob ignores
    # keys it does not know, so payloads stay readable by apps without the gem.
    TRACE_KEY = 'adt_trace'

    # Prepended to ActiveJob::Base.
    module Job
      def serialize
        payload = super
        ActiveJobHooks.tag(self, payload)
        payload
      end

      def deserialize(job_data)
        super
        ActiveJobHooks.read(self, job_data)
      end

      def perform_now
        trace = ActiveJobHooks.scope_for(self)
        return super if trace == :none

        Current.with(trace) { super }
      end
    end

    class << self
      # Prepends the hooks to ActiveJob::Base (or any class with its
      # serialize/deserialize/perform_now). Safe to call more than once.
      # Returns true when installed.
      def install(base)
        return false unless base.is_a?(Class) && base.method_defined?(:serialize)

        base.prepend(Job) unless base <= Job
        true
      rescue StandardError, SystemStackError
        false
      end

      # Adds the trace to a payload being built by serialize, and remembers
      # it on the job so a later serialize (a retry) carries the same one.
      def tag(job, payload)
        return nil unless payload.is_a?(Hash)

        trace = job.instance_variable_get(:@devbench_trace)
        trace ||= Current.trace&.to_s
        return nil if trace.nil?

        job.instance_variable_set(:@devbench_trace, trace)
        payload[TRACE_KEY] = trace
        nil
      rescue StandardError, SystemStackError
        nil
      end

      def read(job, job_data)
        trace = job_data.is_a?(Hash) ? Trace.parse(job_data[TRACE_KEY]) : nil
        job.instance_variable_set(:@devbench_trace, trace&.to_s)
        job.instance_variable_set(:@devbench_deserialized, true)
        nil
      rescue StandardError, SystemStackError
        nil
      end

      # The trace perform_now runs the job under, or :none to leave the
      # thread's context as it is.
      def scope_for(job)
        return :none unless job.instance_variable_get(:@devbench_deserialized)

        trace = Trace.parse(job.instance_variable_get(:@devbench_trace))
        return trace unless Current.scoped?
        return :none if trace.nil? || Current.trace&.to_s == trace.to_s

        trace
      rescue StandardError, SystemStackError
        :none
      end
    end
  end
end
