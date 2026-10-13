# frozen_string_literal: true

require_relative 'fingerprint'

module Devbench
  # Request signals (docs/server-sdk/failed-and-slow-requests.md, "Capability
  # 2b — Failed and slow requests"; DECISIONS #171): what a request does
  # without raising.
  #
  #   * failed: the application answered 500–599 and no exception was
  #     reported during the request;
  #   * slow: it answered 100–499, not streaming, after more than 3,000 ms;
  #   * rate_limit_spike: one route's 429s in one unix minute met the spike
  #     rule (at least ten, and at least 10% of that route's requests).
  #
  # A port of the backend's internal/reqsignal (and the Go SDK's copy,
  # sdk/server/go/reqsignal.go). A port can drift, and a drifted port reports
  # a different set of problems by language, so it is held to the shared
  # vectors (testdata/request_signal_vectors.json) every implementation must
  # reproduce. Change nothing here without changing those in lockstep.
  #
  # Devbench::Middleware observes; this module classifies, builds and keeps
  # the per-process spike detector. Nothing here raises into the
  # application.
  module RequestSignals
    # The thresholds: constants, not configuration. Tenant-configurable
    # thresholds are a later issue (DECISIONS #171), and every SDK must apply
    # the same numbers or the same app reports differently by language.
    SLOW_THRESHOLD_MS = 3000
    SPIKE_WINDOW_SECONDS = 60
    SPIKE_MIN_COUNT = 10
    SPIKE_MIN_PERCENT = 10
    MAX_ROUTES_PER_WINDOW = 512
    MAX_ROUTE_CHARS = 255

    # A request matched to no route pattern: one literal, never the raw path
    # (a path carries ids and slugs).
    UNKNOWN_ROUTE = '*'
    # Any method outside the standard set, so a client sending invented
    # methods cannot mint fingerprints.
    OTHER_METHOD = 'OTHER'
    METHODS = %w[GET HEAD POST PUT PATCH DELETE OPTIONS TRACE CONNECT].freeze

    # The `signal` field of the `request` control message.
    FAILED = 'failed'
    SLOW = 'slow'
    RATE_LIMIT_SPIKE = 'rate_limit_spike'

    # Everything the direct transport needs to count a request signal: the
    # count's kind, the fingerprint's type and message, the template text
    # kept for evidence, and the raw exemplar (scrubbed at egress).
    Built = Struct.new(:kind, :type, :message, :text, :exemplar)

    # One (method, route) whose window met the spike rule. `window` is the
    # window's start, unix seconds.
    Spike = Struct.new(:method, :route, :window, :count, :total)

    # The clock the middleware reads: monotonic milliseconds for a request's
    # duration, wall milliseconds for the spike window. Replaceable only so
    # tests can pin a duration to the millisecond.
    module SystemClock
      def self.monotonic_ms
        Process.clock_gettime(Process::CLOCK_MONOTONIC, :float_millisecond)
      end

      def self.wall_ms
        Process.clock_gettime(Process::CLOCK_REALTIME, :millisecond)
      end
    end

    # The 429 spike rule for one process's requests. Thread-safe: every
    # request's thread observes, the flush thread ticks.
    #
    # One window is open at a time. An observation in a later window closes
    # the open one first; one in an earlier window (a clock step back) counts
    # in the open window. Memory is bounded by MAX_ROUTES_PER_WINDOW, and
    # nothing is kept past the open window. A forked child starts empty: the
    # parent reports what it saw.
    class SpikeDetector
      Tally = Struct.new(:count, :total)

      def initialize
        @lock = Mutex.new
        reset
      end

      # Records one finished request at t_ms (unix milliseconds) and returns
      # the spikes of any window it closed.
      def observe(t_ms, method, route, status)
        w = RequestSignals.window_of(t_ms)
        key = [RequestSignals.normalize_method(method), RequestSignals.normalize_route(route)].freeze

        @lock.synchronize do
          check_fork
          closed = []
          if @window.nil?
            start(w)
          elsif w > @window
            closed = evaluate
            start(w)
          end

          tally = @routes[key]
          if tally.nil?
            return closed if @routes.size >= MAX_ROUTES_PER_WINDOW

            tally = @routes[key] = Tally.new(0, 0)
          end
          tally.total += 1
          tally.count += 1 if status == 429
          closed
        end
      end

      # Closes the open window if now_ms is past it (at each flush) and
      # returns its spikes.
      def tick(now_ms)
        @lock.synchronize do
          check_fork
          return [] if @window.nil? || RequestSignals.window_of(now_ms) <= @window

          out = evaluate
          reset
          out
        end
      end

      # Evaluates the open window as it stands (process exit, a new
      # configuration) and closes it.
      def finish
        @lock.synchronize do
          check_fork
          return [] if @window.nil?

          out = evaluate
          reset
          out
        end
      end

      private

      def reset
        @pid = Process.pid
        @window = nil
        @routes = {}
      end

      def check_fork
        reset unless @pid == Process.pid
      end

      def start(window)
        @window = window
        @routes = {}
      end

      # The open window's spikes, sorted by method then route (byte order,
      # as Go's string comparison). Caller holds @lock.
      def evaluate
        @routes.filter_map do |(method, route), tally|
          next unless RequestSignals.spike?(tally.count, tally.total)

          Spike.new(method, route, @window * SPIKE_WINDOW_SECONDS, tally.count, tally.total)
        end.sort_by { |s| [s.method.b, s.route.b] }
      end
    end

    class << self
      attr_writer :clock

      def clock
        @clock || SystemClock
      end

      # The process's spike detector, in both transports.
      def detector
        @detector ||= SpikeDetector.new
      end

      # The signal one finished request produces: FAILED, SLOW or nil. At
      # most one per request. 429s are not classified here; they feed the
      # spike detector.
      def classify(status:, duration_ms:, exception_reported:, streaming:)
        return nil if status < 100 || status > 599
        # A 5xx after a reported exception: the exception is the report.
        return (exception_reported ? nil : FAILED) if status >= 500
        return nil if streaming || status == 101

        duration_ms > SLOW_THRESHOLD_MS ? SLOW : nil
      end

      # Uppercased and trimmed; anything outside the standard set is OTHER.
      def normalize_method(method)
        m = Fingerprint.trim(utf8(method)).upcase
        METHODS.include?(m) ? m : OTHER_METHOD
      end

      # Trimmed and bounded to MAX_ROUTE_CHARS code points; empty is
      # UNKNOWN_ROUTE.
      def normalize_route(route)
        r = Fingerprint.trim(utf8(route))
        return UNKNOWN_ROUTE if r.empty?

        r.length > MAX_ROUTE_CHARS ? r[0, MAX_ROUTE_CHARS] : r
      end

      # Fingerprint inputs, template text and exemplar for a request signal,
      # as the sidecar builds them from the `request` control message. Nil
      # for a signal/status pair a sensor must refuse.
      def build(signal:, method:, route:, status:, duration_ms: 0, count: 0, total: 0)
        status = status.to_i
        message = "#{normalize_method(method)} #{normalize_route(route)}"
        case signal
        when FAILED
          return nil unless status.between?(500, 599)

          type = "http_#{status}"
          new_built('error', type, message, "#{type}: #{message} in #{non_negative(duration_ms)} ms")
        when SLOW
          return nil unless status.between?(100, 499)

          type = 'slow_request'
          new_built('degradation', type, message,
                    "#{type}: #{message} in #{non_negative(duration_ms)} ms (status #{status})")
        when RATE_LIMIT_SPIKE
          type = 'http_429_spike'
          new_built('error', type, message,
                    "#{type}: #{message} #{non_negative(count)} of #{non_negative(total)} " \
                    'requests answered 429 in one minute')
        end
      end

      # The rule, in integer arithmetic: at least SPIKE_MIN_COUNT 429s, and
      # at least SPIKE_MIN_PERCENT of the route's requests in the window.
      def spike?(count, total)
        count >= SPIKE_MIN_COUNT && count * 100 >= total * SPIKE_MIN_PERCENT
      end

      # Floors toward negative infinity (Ruby's integer division does), so a
      # pre-1970 clock still maps to one window per minute.
      def window_of(t_ms)
        t_ms.to_i / (SPIKE_WINDOW_SECONDS * 1000)
      end

      # ---- Reporting (called by Devbench::Middleware and the flushes)

      # Classifies a finished request, reports its signal, and feeds the spike
      # detector. Called inside the request's scope, so a signal carries its
      # trace and user. Never raises.
      def observe(status:, method:, route:, duration_ms:, streaming:, exception_reported:, finished_ms:)
        return nil unless status.is_a?(Integer)

        method = normalize_method(method)
        route = normalize_route(route)
        signal = classify(status: status, duration_ms: duration_ms,
                          exception_reported: exception_reported, streaming: streaming)
        emit(request_payload(signal, method, route, status, duration_ms)) unless signal.nil?
        return nil unless status.between?(100, 599)

        install_exit_hook
        report_spikes(detector.observe(finished_ms, method, route, status))
        start_flusher if status == 429
        nil
      rescue StandardError, SystemStackError
        nil
      end

      # Closes the spike window if the clock has left it (at a flush) and
      # reports its spikes to `transport`.
      def tick!(transport = nil)
        report_spikes(detector.tick(clock.wall_ms), transport)
      rescue StandardError, SystemStackError
        nil
      end

      # Evaluates the open window as it stands and reports its spikes (process
      # exit, a replaced configuration). Returns how many were reported.
      def finish!(transport = nil)
        report_spikes(detector.finish, transport)
      rescue StandardError, SystemStackError
        0
      end

      # The `request` control message for a spike: an aggregate of many
      # requests, so no trace and no user.
      def spike_payload(spike)
        { v: PROTOCOL_VERSION, kind: 'request', signal: RATE_LIMIT_SPIKE, method: spike.method,
          route: spike.route, status: 429, count: spike.count, total: spike.total }
      end

      private

      def new_built(kind, type, message, exemplar)
        Built.new(kind, type, message, "#{type} at #{message}", exemplar)
      end

      def non_negative(n)
        n = n.to_i
        n.negative? ? 0 : n
      end

      def utf8(text)
        text.to_s.encode('UTF-8', invalid: :replace, undef: :replace).scrub
      end

      def request_payload(signal, method, route, status, duration_ms)
        {
          v: PROTOCOL_VERSION, kind: 'request', signal: signal, method: method, route: route,
          status: status, duration_ms: non_negative(duration_ms),
          trace: safely { Current.trace&.to_s }, user: safely { Current.user }
        }.compact
      end

      def report_spikes(spikes, transport = nil)
        spikes.each { |s| emit(spike_payload(s), transport) }
        spikes.length
      end

      def emit(payload, transport = nil)
        (transport || Devbench.transport).deliver(payload)
      rescue StandardError, SystemStackError
        nil
      end

      # Direct mode: a process whose only traffic is a burst of 429s has
      # counted nothing, so its flush thread may not be running; without it
      # the spike would wait for the next request or the process's exit.
      def start_flusher
        transport = Devbench.transport
        transport.start if transport.respond_to?(:start)
      rescue StandardError, SystemStackError
        nil
      end

      # The open window is evaluated at process exit, in either transport:
      # the spikes are delivered, then direct mode flushes them (bounded, as
      # the transport's own exit flush is). Inherited by forked children,
      # which report their own window.
      def install_exit_hook
        return if @exit_hook_installed

        @exit_hook_installed = true
        at_exit do
          reported = finish!
          Devbench.transport.flush!(timeout: DirectTransport::EXIT_TIMEOUT) if reported.positive?
        rescue StandardError, SystemStackError
          nil
        end
      end

      def safely
        yield
      rescue StandardError, SystemStackError
        nil
      end
    end
  end
end
