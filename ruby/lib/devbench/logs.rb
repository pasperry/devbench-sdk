# frozen_string_literal: true

require 'logger'
require_relative 'current'

module Devbench
  # In-process log capture (docs/SERVER_SDK_SPEC.md, "In-process log capture
  # (direct mode)"; DECISIONS #161): recent log lines kept in memory, keyed
  # by the trace that was current when each was written, so triage can be
  # handed the server lines of one user action without a sidecar.
  #
  # Two ways in, both installed by the Railtie in direct mode only:
  #
  #   * Rails 7.1+ (Rails.logger is an ActiveSupport::BroadcastLogger):
  #     broadcast_to a CaptureLogger — Rails' own public API for "also send
  #     these lines there". It writes nothing anywhere; it only remembers.
  #   * Any other ::Logger (Rails < 7.1, Sidekiq.logger, a plain Logger):
  #     a Tee prepended to that one logger object. It calls the logger's own
  #     #add first, unchanged, and remembers the line afterwards; a block
  #     message is evaluated once, by the logger, and its value reused.
  #
  # Never harms the host: what the app's logger writes is untouched; a line
  # with no current trace costs one thread-local read; the buffer lock is
  # held for an Array push, never across I/O or redaction; nothing raises
  # out of a logging call. Fork-safe: a child starts with an empty buffer.
  module Logs
    MAX_LINES = 10_000
    MAX_BYTES = 4 * 1024 * 1024
    MAX_AGE = 15 * 60
    MAX_LINE_BYTES = 4096

    SEVERITY = %w[DEBUG INFO WARN ERROR FATAL].freeze
    # The last line captured on this thread, and by which hook: see #capture.
    LAST = :__devbench_last_log

    # Bounded, thread-safe, per-trace store of recent lines. Oldest evicted
    # first, by count, bytes and age.
    class Buffer
      Entry = Struct.new(:key, :at, :text)

      def initialize(max_lines: MAX_LINES, max_bytes: MAX_BYTES, max_age: MAX_AGE,
                     clock: -> { Process.clock_gettime(Process::CLOCK_MONOTONIC) })
        @max_lines = max_lines
        @max_bytes = max_bytes
        @max_age = max_age
        @clock = clock
        @lock = Mutex.new
        reset
      end

      # Keeps one line under a trace key.
      def push(key, text)
        now = @clock.call
        entry = Entry.new(key, now, text)
        @lock.synchronize do
          reset if @pid != Process.pid
          @entries << entry
          (@by_key[key] ||= []) << entry
          @bytes += text.bytesize
          evict(now)
        end
        nil
      end

      # The lines held for a key, oldest first, at most `limit` (the most
      # recent ones when there are more). A copy: the caller redacts it
      # outside the lock.
      def lookup(key, limit)
        @lock.synchronize do
          reset if @pid != Process.pid
          evict(@clock.call)
          list = @by_key[key]
          return [] if list.nil?

          (list.length > limit ? list.last(limit) : list).map(&:text)
        end
      end

      def size
        @lock.synchronize { @entries.length }
      end

      # Whether any line younger than the age bound is held (by this
      # process).
      def any?
        @lock.synchronize do
          reset if @pid != Process.pid
          evict(@clock.call)
          !@entries.empty?
        end
      end

      def bytes
        @lock.synchronize { @bytes }
      end

      def clear
        @lock.synchronize { reset }
      end

      private

      def reset
        @pid = Process.pid
        @entries = []
        @by_key = {}
        @bytes = 0
      end

      # Every entry is the oldest of its own key when it is the oldest
      # overall, so removing it from its key's list is a shift.
      def evict(now)
        cutoff = now - @max_age
        while (oldest = @entries.first) &&
              (@entries.length > @max_lines || @bytes > @max_bytes || oldest.at < cutoff)
          @entries.shift
          list = @by_key[oldest.key]
          list.shift
          @by_key.delete(oldest.key) if list.empty?
          @bytes -= oldest.text.bytesize
        end
      end
    end

    # Rails 7.1+: the logger handed to BroadcastLogger#broadcast_to. Its
    # level mirrors the app's other loggers, so adding it never turns on a
    # level the app has off (BroadcastLogger#level is the minimum of its
    # loggers, and #debug? asks whether any is at debug). It has no #tagged:
    # BroadcastLogger would run a tagged block once per logger that has one.
    module CaptureLogger
      attr_accessor :devbench_broadcast

      def level
        own = super
        others = devbench_broadcast&.broadcasts&.reject { |l| l.equal?(self) }
        return own if others.nil? || others.empty?

        mirrored = others.map(&:level).min
        local = respond_to?(:local_level) ? local_level : nil
        local.nil? ? mirrored : [local, mirrored].max
      rescue StandardError, SystemStackError
        ::Logger::FATAL
      end

      def add(severity, message = nil, progname = nil)
        severity ||= ::Logger::UNKNOWN
        return true if severity < level || Current.trace.nil?

        if message.nil?
          message = block_given? ? yield : progname
        end
        Logs.capture(severity, message, :broadcast)
        true
      rescue StandardError, SystemStackError
        true
      end
      alias log add

      def <<(message)
        Logs.capture(nil, message, :broadcast) unless Current.trace.nil?
        self
      rescue StandardError, SystemStackError
        self
      end
    end

    # Every other ::Logger: prepended to the one logger object's singleton
    # class. The logger's own #add runs first, exactly as before; only then
    # is the line remembered.
    module Tee
      def add(severity, message = nil, progname = nil, &block)
        return super if Current.trace.nil? || !Logs.active?

        yielded = false
        value = nil
        if block
          original = block
          block = proc do
            value = original.call
            yielded = true
            value
          end
        end
        result = super(severity, message, progname, &block)
        Logs.tee(self, severity, message, progname, !block.nil?, yielded, value)
        result
      end
      alias log add
    end

    class << self
      def buffer
        @buffer ||= Buffer.new
      end

      # For tests: a buffer with other bounds or a controllable clock.
      attr_writer :buffer

      # Whether capture is on: direct mode, after install. Hooks stay in
      # place when it is turned off, and do nothing.
      def active?
        @active ? true : false
      end

      # A new transport (Devbench.configure) must be started again by the
      # next line held, so both reset the per-process start.
      def activate!
        @polling_pid = nil
        @active = true
      end

      # Whether any logger was hooked in this process.
      def hooked?
        @hooked ? true : false
      end

      def deactivate!
        @polling_pid = nil
        @active = false
      end

      # Hooks one logger. Returns :broadcast, :tee, or nil (not a logger we
      # can hook — left alone). Idempotent.
      def install(logger)
        return nil if logger.nil?

        if logger.respond_to?(:broadcast_to) && logger.respond_to?(:broadcasts)
          return :broadcast if logger.broadcasts.any? { |l| l.is_a?(CaptureLogger) }

          capture = capture_logger_class.new(nil)
          capture.level = logger.level
          capture.devbench_broadcast = logger
          logger.broadcast_to(capture)
          @hooked = true
          :broadcast
        elsif logger.is_a?(::Logger)
          logger.singleton_class.prepend(Tee) unless logger.singleton_class.include?(Tee)
          @hooked = true
          :tee
        end
      rescue StandardError, SystemStackError
        nil
      end

      # Whether this process holds any traced line: the transport polls
      # ingest for log requests only then.
      def holding?
        buffer.any?
      rescue StandardError, SystemStackError
        false
      end

      # The lines held for one trace key (session/intent), oldest first.
      def lookup(key, limit)
        buffer.lookup(key, limit)
      rescue StandardError, SystemStackError
        []
      end

      # Called by Tee after the logger's own #add returned.
      def tee(logger, severity, message, progname, had_block, yielded, value)
        severity ||= ::Logger::UNKNOWN
        return if severity < logger.level

        if message.nil?
          return if had_block && !yielded

          message = had_block ? value : progname
        end
        capture(severity, message, :tee)
      rescue StandardError, SystemStackError
        nil
      end

      # Keeps one line under the current trace. Never raises.
      #
      # One line can reach both hooks: in a Sidekiq process Rails.logger may
      # broadcast to Sidekiq.logger. Both run on this thread, one right
      # after the other, so a line equal to the one just captured *by the
      # other hook* is that same write and is skipped. The same hook
      # repeating a line is a real repeat and is kept.
      def capture(severity, message, source)
        return nil unless active?

        trace = Current.trace
        return nil if trace.nil?

        line = line_for(trace, severity, message)
        last = Thread.current[LAST]
        if last && last[0] != source && last[1] == line
          Thread.current[LAST] = nil
          return nil
        end
        Thread.current[LAST] = [source, line]
        buffer.push(trace.key, line)
        start_polling if @polling_pid != Process.pid
        nil
      rescue StandardError, SystemStackError, ThreadError
        nil
      end

      private

      # The first line this process holds starts the transport's flush
      # thread, so a process with nothing to report still polls for log
      # requests (a forked worker included: the PID is per process).
      def start_polling
        @polling_pid = Process.pid
        transport = Devbench.transport
        transport.start if transport.respond_to?(:start)
      end

      # "[v1/s/i/h] INFO message", at most MAX_LINE_BYTES, valid UTF-8.
      def line_for(trace, severity, message)
        label = severity.is_a?(Integer) ? SEVERITY[severity] : nil
        text = message_text(message)
        line = label ? "[#{trace}] #{label} #{text}" : "[#{trace}] #{text}"
        line = line.byteslice(0, MAX_LINE_BYTES) if line.bytesize > MAX_LINE_BYTES
        line = line.dup.force_encoding(Encoding::UTF_8) unless line.encoding == Encoding::UTF_8
        line.valid_encoding? ? line : line.scrub('')
      end

      # As ::Logger::Formatter#msg2str.
      def message_text(message)
        text = case message
               when ::String then message
               when ::Exception
                 "#{message.message} (#{message.class})\n#{(message.backtrace || []).join("\n")}"
               else message.inspect
               end
        text.end_with?("\n") ? text.chomp : text
      end

      def capture_logger_class
        @capture_logger_class ||= begin
          base = defined?(::ActiveSupport::Logger) ? ::ActiveSupport::Logger : ::Logger
          Class.new(base) { include CaptureLogger }
        end
      end
    end
  end
end
