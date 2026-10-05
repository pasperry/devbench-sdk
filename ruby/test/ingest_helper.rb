# frozen_string_literal: true

require 'json'
require 'socket'

# A real HTTP server on 127.0.0.1 standing in for Dev Bench ingest and its
# presigned evidence URLs. Not a stub of our client: the SDK speaks real
# HTTP over a real socket to it, and the tests assert on the bytes that
# arrived. (WEBrick left the standard library in Ruby 3.0, so this parses
# HTTP/1.1 itself — enough for Net::HTTP: request line, headers,
# Content-Length body, one request per connection.)
class FakeIngest
  Request = Struct.new(:method, :path, :headers, :body) do
    def json
      JSON.parse(body)
    end
  end

  attr_reader :port
  # Status for POST /v1/flush (default 200); a proc gets the request.
  attr_accessor :flush_status
  # When true, every fp in a flush is answered with a need_evidence ask.
  attr_accessor :ask_evidence
  # Trace keys (session/intent) every flush response asks lines for, as
  # ingest's need_logs does for a pending log slice request.
  attr_accessor :need_logs
  # The learned rule set every flush response carries (`redaction`), or nil
  # to omit the field, as ingest does when it could not look rules up.
  attr_accessor :redaction
  # When true, need_logs is offered only on an empty flush (a poll), so a
  # test proves the poll is what answered.
  attr_accessor :need_logs_only_on_poll
  # Status for POST /v1/logs (default 200).
  attr_accessor :logs_status

  def initialize
    @server = TCPServer.new('127.0.0.1', 0)
    @port = @server.addr[1]
    @requests = []
    @lock = Mutex.new
    @arrived = ConditionVariable.new
    @flush_status = 200
    @ask_evidence = false
    @need_logs = []
    @redaction = nil
    @logs_status = 200
    @asked = {}
    @acceptor = Thread.new { accept_loop }
    @acceptor.report_on_exception = false
  end

  def base
    "http://127.0.0.1:#{@port}"
  end

  def dsn(key = 'adt_server_testkey')
    "http://#{key}@127.0.0.1:#{@port}"
  end

  # The DSN `adt dsn create` prints: public (browser) key : secret (server) key.
  def pair_dsn(public_key = 'adt_client_testpub', secret = 'adt_server_testsecret')
    "http://#{public_key}:#{secret}@127.0.0.1:#{@port}"
  end

  def requests
    @lock.synchronize { @requests.dup }
  end

  def flushes
    requests.select { |r| r.method == 'POST' && r.path == '/v1/flush' }
  end

  def evidence
    requests.select { |r| r.method == 'PUT' }
  end

  def log_deliveries
    requests.select { |r| r.method == 'POST' && r.path == '/v1/logs' }
  end

  # Every slice delivered so far, {trace => [lines]}, in arrival order.
  def slices
    log_deliveries.flat_map { |r| r.json['slices'] }.each_with_object({}) do |sl, out|
      (out[sl['trace']] ||= []).concat(sl['lines'])
    end
  end

  # Waits until `n` requests matching the block have arrived.
  def wait_for(n = 1, timeout: 5, &match)
    match ||= ->(_r) { true }
    deadline = Process.clock_gettime(Process::CLOCK_MONOTONIC) + timeout
    @lock.synchronize do
      loop do
        found = @requests.select(&match)
        return found if found.length >= n

        remaining = deadline - Process.clock_gettime(Process::CLOCK_MONOTONIC)
        return found if remaining <= 0

        @arrived.wait(@lock, remaining)
      end
    end
  end

  def close
    @server.close
    @acceptor.kill
    @acceptor.join(1)
  rescue IOError, SystemCallError
    nil
  end

  private

  def accept_loop
    loop do
      conn = @server.accept
      Thread.new(conn) { |c| serve(c) }.report_on_exception = false
    end
  rescue IOError, SystemCallError
    nil
  end

  def serve(conn)
    line = conn.gets("\r\n")
    return if line.nil?

    method, path, = line.split(' ')
    headers = {}
    while (h = conn.gets("\r\n")) && h != "\r\n"
      name, value = h.split(':', 2)
      headers[name.strip.downcase] = value.to_s.strip
    end
    body = conn.read(headers['content-length'].to_i).to_s
    request = Request.new(method, path, headers, body)

    status, response = respond(request)
    @lock.synchronize do
      @requests << request
      @arrived.broadcast
    end
    conn.write("HTTP/1.1 #{status} X\r\ncontent-type: application/json\r\n" \
               "content-length: #{response.bytesize}\r\nconnection: close\r\n\r\n#{response}")
  rescue IOError, SystemCallError
    nil
  ensure
    conn.close
  end

  def respond(request)
    if request.method == 'PUT'
      [200, '{}']
    elsif request.path == '/v1/flush'
      status = @flush_status.respond_to?(:call) ? @flush_status.call(request) : @flush_status
      return [status, '{"error":"unauthorized"}'] if status == 401
      return [status, '{"error":"nope"}'] unless (200..299).cover?(status)

      offer = !@need_logs_only_on_poll || request.json['counts'].empty?
      [status, JSON.generate(asks_for(request).merge(offer ? log_asks : {}))]
    elsif request.method == 'POST' && request.path == '/v1/check'
      status = @flush_status.respond_to?(:call) ? @flush_status.call(request) : @flush_status
      return [status, '{"error":"unauthorized"}'] unless (200..299).cover?(status)

      [status, '{"ok":true,"tenant":"acme","environment":"test","source":"server"}']
    elsif request.method == 'POST' && request.path == '/v1/logs'
      [@logs_status, (200..299).cover?(@logs_status) ? '{"ok":true}' : '{"error":"nope"}']
    else
      [404, '{}']
    end
  end

  def log_asks
    out = {}
    out[:need_logs] = @need_logs.map { |k| { trace: k } } unless @need_logs.empty?
    out[:redaction] = @redaction unless @redaction.nil?
    out
  end

  # At most one ask per fingerprint, as ingest's evidence claim does.
  def asks_for(request)
    return {} unless @ask_evidence

    asks = request.json['counts'].filter_map do |c|
      next if @lock.synchronize { @asked[c['fp']] }

      @lock.synchronize { @asked[c['fp']] = true }
      { fp: c['fp'], claim: "c_#{c['fp'][0, 6]}", url: "#{base}/evidence/#{c['fp']}",
        expires: Time.now.to_i + 300 }
    end
    asks.empty? ? {} : { need_evidence: asks }
  end
end
