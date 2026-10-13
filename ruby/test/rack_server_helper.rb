# frozen_string_literal: true

require 'net/http'
require 'socket'
require 'stringio'

# A real HTTP/1.1 server on 127.0.0.1 that hands each request to a Rack app,
# so the request-signal tests send real HTTP through a real socket into the
# real middleware and the real framework. (No Rack server ships with Ruby
# or with railties, and CI installs only railties, activejob and sidekiq;
# like FakeIngest, this parses just enough HTTP for Net::HTTP: request line,
# headers, a Content-Length body, one request per connection.)
#
# It does what a server does with a Rack response: writes the status and
# headers, iterates the body (streaming each chunk as it comes), closes it,
# and honours a full hijack (env['rack.hijack']) and a partial one (a
# `rack.hijack` response header).
class RackServer
  attr_reader :port

  def initialize(app)
    @app = app
    @server = TCPServer.new('127.0.0.1', 0)
    @port = @server.addr[1]
    @acceptor = Thread.new { accept_loop }
    @acceptor.report_on_exception = false
  end

  def close
    @server.close
    @acceptor.kill
    @acceptor.join(1)
  rescue IOError, SystemCallError
    nil
  end

  # One request over a real connection. Returns the Net::HTTPResponse.
  def request(method, path, headers: {}, body: nil, read_timeout: 30)
    Net::HTTP.start('127.0.0.1', @port, read_timeout: read_timeout) do |http|
      klass = Net::HTTP.const_get(method.to_s.capitalize)
      req = klass.new(path, headers)
      req.body = body unless body.nil?
      http.request(req)
    end
  end

  # A raw request line and headers, for responses Net::HTTP cannot read (a
  # 101 upgrade). Returns everything the server wrote before closing.
  def raw(method, path, headers: {})
    TCPSocket.open('127.0.0.1', @port) do |s|
      s.write("#{method} #{path} HTTP/1.1\r\nhost: 127.0.0.1\r\n")
      headers.each { |k, v| s.write("#{k}: #{v}\r\n") }
      s.write("\r\n")
      s.read
    end
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

    method, target, = line.split(' ')
    headers = {}
    while (h = conn.gets("\r\n")) && h != "\r\n"
      name, value = h.split(':', 2)
      headers[name.strip.downcase] = value.to_s.strip
    end
    body = conn.read(headers['content-length'].to_i).to_s
    env = env_for(method, target, headers, body, conn)

    begin
      status, response_headers, response_body = @app.call(env)
    rescue Exception # rubocop:disable Lint/RescueException
      # What every server does with an exception out of the app.
      write_head(conn, 500, { 'content-type' => 'text/plain' })
      conn.write('Internal Server Error')
      return
    end
    return if env['rack.hijack_io'] # the app owns the socket now

    write_response(conn, status, response_headers, response_body)
  rescue IOError, SystemCallError
    nil
  ensure
    conn.close unless conn.closed? || env&.dig('rack.hijack_io')
  end

  def env_for(method, target, headers, body, conn)
    path, query = target.split('?', 2)
    env = {
      'REQUEST_METHOD' => method, 'SCRIPT_NAME' => '', 'PATH_INFO' => path, 'QUERY_STRING' => query.to_s,
      'SERVER_NAME' => '127.0.0.1', 'SERVER_PORT' => @port.to_s, 'SERVER_PROTOCOL' => 'HTTP/1.1',
      'REMOTE_ADDR' => '127.0.0.1', 'rack.url_scheme' => 'http',
      'rack.input' => StringIO.new(body.b), 'rack.errors' => $stderr,
      'rack.multithread' => true, 'rack.multiprocess' => false, 'rack.run_once' => false,
      'rack.hijack?' => true
    }
    env['rack.hijack'] = lambda do
      env['rack.hijack_io'] = conn
      conn
    end
    headers.each do |name, value|
      key = name.upcase.tr('-', '_')
      case key
      when 'CONTENT_TYPE', 'CONTENT_LENGTH' then env[key] = value
      else env["HTTP_#{key}"] = value
      end
    end
    env
  end

  def write_response(conn, status, headers, body)
    hijack = nil
    plain = {}
    headers.each do |name, value|
      if name.to_s.downcase == 'rack.hijack'
        hijack = value
      else
        plain[name] = value
      end
    end
    write_head(conn, status, plain)
    if hijack
      hijack.call(conn)
      return
    end

    if body.respond_to?(:each)
      body.each do |chunk|
        conn.write(chunk)
        conn.flush
      end
    elsif body.respond_to?(:call)
      body.call(conn)
    end
  ensure
    body.close if body.respond_to?(:close)
  end

  def write_head(conn, status, headers)
    conn.write("HTTP/1.1 #{status} X\r\n")
    headers.each do |name, value|
      Array(value).flat_map { |v| v.to_s.split("\n") }.each { |v| conn.write("#{name}: #{v}\r\n") }
    end
    conn.write("connection: close\r\n\r\n")
  end
end

# The request clock, pinned: a handler advances it by exactly the duration
# a test wants, so 3,000 ms and 3,001 ms are both reachable without
# sleeping. The wall clock moves with it, so the spike window is the
# test's to choose too.
class TestClock
  def initialize(wall_ms: 1_758_799_981_000)
    @lock = Mutex.new
    @monotonic = 1_000_000.0
    @wall = wall_ms
  end

  def monotonic_ms
    @lock.synchronize { @monotonic }
  end

  def wall_ms
    @lock.synchronize { @wall }
  end

  def advance(ms)
    @lock.synchronize do
      @monotonic += ms
      @wall += ms.to_i
    end
  end

  def wall_ms=(value)
    @lock.synchronize { @wall = value }
  end
end
