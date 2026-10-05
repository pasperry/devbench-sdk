# frozen_string_literal: true

require 'json'
require 'socket'

module Devbench
  # Sidecar mode: each report is one line of JSON on the local sidecar's
  # unix socket (docs/SERVER_SDK_SPEC.md, "Control socket — v1"). Used when no
  # DSN is set; unchanged from 0.4.
  #
  # Never blocks for long: this runs inside rescue blocks and exception
  # paths in the customer's request, and a diagnostics call that can hang
  # there would turn a failure into an outage.
  module SidecarTransport
    MAX_LINE = 256 * 1024

    # The longest a report may hold up the request that produced it. A
    # sidecar that has stopped reading costs this much, once per report, and
    # never more.
    WRITE_TIMEOUT = 0.2

    class << self
      # Connects, writes one line, closes, with a deadline on both. A plain
      # blocking write would hang the request whenever the payload outgrows
      # the socket buffer (8 KiB on macOS) and the sidecar is not reading.
      def deliver(payload)
        line = "#{JSON.generate(payload)}\n"
        return if line.bytesize > MAX_LINE

        socket = Socket.new(Socket::AF_UNIX, Socket::SOCK_STREAM)
        begin
          write(socket, line)
        ensure
          socket.close
        end
        nil
      rescue StandardError, SystemStackError
        # No sidecar, a full socket buffer, a malformed payload — none of it is
        # the application's problem. Losing a report is always preferable to
        # affecting the request that produced it.
        nil
      end

      # Nothing is buffered here; the sidecar flushes on its own schedule.
      def flush!(**)
        true
      end

      private

      def write(socket, line)
        deadline = now + WRITE_TIMEOUT
        address = Socket.sockaddr_un(Reporter.socket_path)
        if socket.connect_nonblock(address, exception: false) == :wait_writable
          return unless writable?(socket, deadline)

          begin
            socket.connect_nonblock(address)
          rescue Errno::EISCONN
            nil
          end
        end

        until line.empty?
          written = socket.write_nonblock(line, exception: false)
          if written == :wait_writable
            return unless writable?(socket, deadline)
          else
            line = line.byteslice(written..)
          end
        end
      end

      def writable?(socket, deadline)
        remaining = deadline - now
        remaining.positive? && !IO.select(nil, [socket], nil, remaining).nil?
      end

      def now
        Process.clock_gettime(Process::CLOCK_MONOTONIC)
      end
    end
  end

  # Reporting is off: DEVBENCH_ENABLED=false, or a DSN that does not parse.
  module NullTransport
    def self.deliver(_payload)
      nil
    end

    def self.flush!(**)
      true
    end
  end
end
