# frozen_string_literal: true

require 'json'
require 'socket'
require 'tmpdir'
require 'fileutils'

# A real unix socket server standing in for the sidecar. Not a stub: what the
# tests assert on is the bytes that actually crossed the socket, which is the
# wire contract (docs/SERVER_SDK_SPEC.md, "Control socket — v1").
module SidecarHelper
  SENTINEL = "__flush__\n"

  def start_sidecar
    @sidecar_dir = Dir.mktmpdir
    @sidecar_path = File.join(@sidecar_dir, 'sidecar.sock')
    @sidecar_server = UNIXServer.new(@sidecar_path)
    @sidecar_received = Queue.new
    ADT::Reporter.socket_path = @sidecar_path

    # One connection per message: the SDK connects, writes, closes. A single
    # acceptor keeps arrival order equal to send order.
    @sidecar_acceptor = Thread.new do
      loop do
        conn = @sidecar_server.accept
        @sidecar_received << conn.read
        conn.close
      end
    rescue IOError, SystemCallError
      nil
    end
  end

  def stop_sidecar
    @sidecar_server&.close
    @sidecar_acceptor&.kill&.join
    FileUtils.rm_rf(@sidecar_dir) if @sidecar_dir
    ADT::Reporter.socket_path = nil
  end

  # Every message the SDK has sent so far, parsed.
  #
  # Negative assertions ("sent exactly once", "sent nothing") need to know
  # that everything already sent has arrived. A sentinel written from here
  # queues behind the SDK's connections, so seeing it means they are in.
  def sidecar_messages
    UNIXSocket.open(@sidecar_path) { |s| s.write(SENTINEL) }
    messages = []
    loop do
      raw = @sidecar_received.pop(timeout: 3)
      flunk 'timed out waiting for the sidecar socket' if raw.nil?
      break if raw == SENTINEL

      assert raw.end_with?("\n"), "message is not newline-terminated: #{raw[0, 200].inspect}"
      assert_equal 1, raw.count("\n"), 'one message per line, one line per connection'
      messages << JSON.parse(raw)
    end
    messages
  end

  def sidecar_message
    messages = sidecar_messages
    assert_equal 1, messages.length, "expected exactly one message, got #{messages.inspect}"
    messages.first
  end
end
