# frozen_string_literal: true

require 'json'
require 'net/http'
require 'securerandom'
require 'uri'
require_relative 'fingerprint'
require_relative 'scrub'
require_relative 'egress_policy'
require_relative 'logs'

module Devbench
  # Direct mode (docs/SERVER_SDK_SPEC.md, "Direct mode"): fingerprint and
  # count in-process, flush counts to ingest once a minute, upload one
  # redacted evidence bundle per fingerprint only when ingest asks, and
  # answer ingest's requests for the log lines of a trace (need_logs) from
  # the in-process buffer (Devbench::Logs), redacted. The same
  # phase-1/phase-2 protocol as the browser sensor, and the same signals and
  # bundle the sidecar builds from the equivalent control message, so a
  # customer can move between modes without forking an issue.
  #
  # Never harms the host:
  #   * The calling thread fingerprints the report (tens of microseconds, no
  #     I/O) and folds it into a count under a lock that is never held across
  #     I/O. Nothing is queued per occurrence: the table of counts *is* the
  #     buffer, bounded at MAX_FINGERPRINTS per window; further distinct
  #     fingerprints are counted in `overflowed`, as the browser does.
  #   * All network I/O is on a background thread (or an explicit flush!),
  #     with timeouts of at most 5 s, one retry with jitter, then discard.
  #   * Nothing raises into the application.
  #   * Fork-safe: Puma, Unicorn and Sidekiq fork after boot. A child notices
  #     the PID change on its first report, drops the parent's counts (the
  #     parent sends those), takes a new sensor id, and starts its own thread.
  class DirectTransport
    MAX_FINGERPRINTS = 512
    MAX_USERS = 20
    MAX_DETAILS = 512
    MAX_COUNTS_PER_POST = 512
    MAX_BODY_BYTES = 192 * 1024
    MAX_MESSAGE = 2000
    MAX_FRAMES = 50
    MAX_IDENTITY = 255
    MAX_RESPONSE_BYTES = 64 * 1024
    # Log slices (spec: <= 500 lines per slice). 200 and 64 KiB are what the
    # store keeps of a slice; sending more would spend the customer's
    # bandwidth on lines truncated on arrival. 25 slices is what one
    # delivery may carry.
    MAX_SLICE_LINES = 200
    MAX_SLICE_BYTES = 64 * 1024
    MAX_SLICES_PER_POST = 25
    HTTP_TIMEOUT = 5.0
    EXIT_TIMEOUT = 2.0

    Count = Struct.new(:kind, :n, :first, :last, :users, :users_overflow)
    Detail = Struct.new(:kind, :text, :exemplar, :frames)
    Signal = Struct.new(:fp, :kind, :text, :user, :frames, :exemplar)

    attr_reader :dsn, :service, :release

    def initialize(dsn:, service:, release:, interval: 60)
      @dsn = dsn
      @service = service.to_s
      @release = release.to_s
      @interval = interval.to_f.positive? ? interval.to_f : 60.0
      @flush_uri = URI(dsn.flush_url)
      @logs_uri = URI(dsn.logs_url)
      @check_uri = URI(dsn.check_url)
      # Learned redaction rules from the last response that carried a set.
      # Not per process: a forked child keeps them until ingest re-sends.
      @policy = nil
      @fork_lock = Mutex.new
      @stopped = false
      reset_state
    end

    # Called by Reporter on the application's thread with a control message
    # (the same hash the sidecar transport would write to its socket).
    def deliver(payload)
      signal = signal_for(payload)
      return nil if signal.nil?

      ensure_running
      @lock.synchronize { record(signal) }
      nil
    rescue StandardError, SystemStackError
      nil
    end

    # Sends everything counted so far, now, waiting at most `timeout`
    # seconds. Returns true when nothing is left unsent.
    def flush!(timeout: HTTP_TIMEOUT)
      check_fork
      deadline = monotonic + timeout.to_f
      return false unless acquire(@cycle, deadline)

      begin
        flush_cycle(deadline)
      ensure
        @cycle.unlock
      end
    rescue StandardError, SystemStackError
      false
    end

    # A last flush (bounded to EXIT_TIMEOUT), then the thread stops. Called
    # when the transport is replaced (Devbench.configure, Devbench.reset!).
    def stop
      flush!(timeout: EXIT_TIMEOUT)
      @stopped = true
      lock = @lock
      lock.synchronize { @wake.broadcast }
      thread = @thread
      thread.join(0.5) if thread && thread != Thread.current
      nil
    rescue StandardError, SystemStackError
      nil
    end

    # The learned rules in force (an EgressPolicy), or nil before any
    # arrived. For tests and diagnostics.
    attr_reader :policy

    # Counts not yet flushed, by fingerprint. For tests and diagnostics.
    def pending
      @lock.synchronize { @window.transform_values { |c| c.n } }
    end

    # rake devbench:test. Sends one synthetic exception straight to ingest
    # (not via the background thread), uploads its evidence if asked, and
    # prints what happened. Returns true when ingest accepted it.
    # The self-test (Devbench.test!): asks ingest whether this DSN's key is
    # valid, and for which tenant and environment. Creates nothing — it used
    # to send a synthetic exception, which became a real issue on a new
    # customer's punch list.
    def self_test(io)
      io.puts "Dev Bench: checking the DSN against #{@dsn} (service #{@service.inspect})"
      response, error = attempt(:post, @check_uri, '{}', flush_headers, HTTP_TIMEOUT)
      if error
        io.puts "  could not reach #{@dsn}: #{error.class}: #{error.message}"
        return false
      end

      code = response.code.to_i
      unless (200..299).cover?(code)
        io.puts "  HTTP #{code}: #{explain(code, response)}"
        return false
      end

      info = begin
        JSON.parse(response.body.to_s)
      rescue JSON::ParserError
        {}
      end
      io.puts "  HTTP #{code}: accepted — tenant #{info['tenant'].inspect}, environment " \
              "#{info['environment'].inspect}. Nothing was recorded; real errors will appear as issues."
      true
    end

    # Starts the background flush thread if it is not running (in this
    # process). Called when this process first holds a log line, so a
    # worker that reports nothing still polls for log requests.
    def start
      ensure_running
      nil
    rescue StandardError, SystemStackError
      nil
    end

    private

    # ---- Signals: exactly what the sidecar builds (internal/sidecar/control.go)

    def signal_for(payload)
      return nil unless payload.is_a?(Hash)

      case payload[:kind]
      when 'exception' then exception_signal(payload)
      when 'handled_failure' then handled_signal(payload)
      end
    end

    # recordException: {error, server, service, type, message[0,2000], frames}.
    def exception_signal(payload)
      type = payload[:error].to_s
      message = truncate(payload[:message].to_s, MAX_MESSAGE)
      frames = Array(payload[:frames]).first(MAX_FRAMES)
      fp = Fingerprint.compute(kind: 'error', source: 'server', service: @service,
                               type: type, message: message, frames: frames)
      return nil if fp.nil?

      symbol = payload[:symbol].to_s
      text = symbol.empty? ? type : "#{type} at #{symbol}"
      Signal.new(fp, 'error', text, user(payload[:user]), frames, exemplar(payload))
    end

    # recordHandled: {handled_failure, server, service, type, message: symbol}.
    # Frames and message are not part of it, so existing fingerprints never
    # move.
    def handled_signal(payload)
      symbol = payload[:symbol].to_s
      fp = Fingerprint.compute(kind: 'handled_failure', source: 'server', service: @service,
                               type: payload[:error].to_s, message: symbol)
      return nil if fp.nil?

      frames = Array(payload[:frames]).first(MAX_FRAMES)
      Signal.new(fp, 'handled_failure', symbol, user(payload[:user]), frames, exemplar(payload))
    end

    # The sidecar's exemplar line: `Error: message [context, handled]
    # reason=...`. Raw here; templated and scrubbed when it leaves.
    def exemplar(payload)
      line = payload[:error].to_s.dup
      message = truncate(payload[:message].to_s, MAX_MESSAGE)
      line << ': ' << message.tr("\n", ' ') unless message.empty?
      context = payload[:context].to_s
      unless context.empty?
        line << ' [' << context
        line << ', handled' if payload[:kind] == 'exception' && payload[:handled]
        line << ']'
      end
      reason = payload[:reason].to_s
      line << ' reason=' << reason unless reason.empty?
      line
    end

    # Identity travels only in the count's users list, never in a
    # fingerprint, template or bundle.
    def user(raw)
      return nil unless raw.is_a?(Hash)

      email = truncate(Fingerprint.trim((raw[:email] || raw['email']).to_s).downcase, MAX_IDENTITY)
      account = truncate(Fingerprint.trim((raw[:account] || raw['account']).to_s), MAX_IDENTITY)
      out = {}
      out[:email] = email unless email.empty?
      out[:account] = account unless account.empty?
      out.empty? ? nil : out
    end

    def truncate(text, max)
      text.length > max ? text[0, max] : text
    end

    # ---- The table (caller holds @lock)

    def record(signal)
      now = Time.now.to_i
      entry = @window[signal.fp]
      if entry.nil?
        if @window.size >= MAX_FINGERPRINTS
          @overflowed += 1
          return
        end
        entry = @window[signal.fp] = Count.new(signal.kind, 0, now, now, [], 0)
      end
      entry.n += 1
      entry.last = now if now > entry.last
      add_user(entry, signal.user)
      remember(signal)
    end

    def add_user(entry, user)
      return if user.nil? || entry.users.include?(user)

      if entry.users.length >= MAX_USERS
        entry.users_overflow += 1
      else
        entry.users << user
      end
    end

    # The first occurrence's detail, kept across windows so evidence can be
    # answered after the flush that reported it. Bounded, least recently
    # seen first out.
    def remember(signal)
      detail = @details.delete(signal.fp)
      detail ||= Detail.new(signal.kind, signal.text, signal.exemplar, signal.frames)
      detail.frames = signal.frames if detail.frames.empty? && !signal.frames.empty?
      @details[signal.fp] = detail
      @details.shift while @details.size > MAX_DETAILS
    end

    # ---- Lifecycle

    def reset_state
      @pid = Process.pid
      @lock = Mutex.new
      @wake = ConditionVariable.new
      @cycle = Mutex.new
      @window = {}
      @details = {}
      @overflowed = 0
      @sensor_id = "rb-#{SecureRandom.hex(10)}"
      @thread = nil
    end

    def check_fork
      return if @pid == Process.pid

      @fork_lock.synchronize { reset_state if @pid != Process.pid }
    end

    def ensure_running
      check_fork
      return if @stopped || @thread&.alive?

      @fork_lock.synchronize do
        return if @stopped || @thread&.alive?

        @thread = Thread.new { run }
        @thread.name = 'devbench-flush' if @thread.respond_to?(:name=)
        @thread.report_on_exception = false
      end
    end

    def run
      next_at = monotonic + @interval
      until @stopped
        lock = @lock
        lock.synchronize do
          while !@stopped && (remaining = next_at - monotonic).positive?
            @wake.wait(lock, remaining)
          end
        end
        break if @stopped

        next_at = monotonic + @interval
        begin
          @cycle.synchronize { flush_cycle(monotonic + (HTTP_TIMEOUT * 4)) }
        rescue StandardError, SystemStackError
          nil
        end
      end
    end

    # ---- Flushing

    # Phase 1, then whatever the responses asked for: learned rules first
    # (a rule arriving with a request for the shape it governs applies to
    # that request), then log slices, then evidence.
    def flush_cycle(deadline)
      counts, overflowed = @lock.synchronize do
        taken = [@window, @overflowed]
        @window = {}
        @overflowed = 0
        taken
      end
      # Nothing counted: poll only while this process holds traced lines,
      # since need_logs only rides a flush response (the sidecar's poll).
      # Holding none, send nothing.
      if counts.empty?
        return true unless Logs.holding?

        posts = [envelope([], 0)]
      else
        posts = bodies(counts, overflowed)
      end

      ok = true
      wanted = []
      log_keys = []
      rules = nil
      posts.each do |body|
        response = request(:post, @flush_uri, body, flush_headers, deadline)
        if response.nil?
          ok = false
          next
        end
        code = response.code.to_i
        if (200..299).cover?(code)
          answer = answers(response)
          wanted.concat(answer[:evidence])
          log_keys.concat(answer[:logs])
          rules = answer[:redaction] unless answer[:redaction].nil?
        else
          ok = false
          Devbench.warn_once(:"http_#{code}", "Dev Bench refused a flush: HTTP #{code}: #{explain(code, response)}")
        end
      end
      @policy = EgressPolicy.compile(rules) unless rules.nil?
      deliver_logs(log_keys.uniq, deadline) unless log_keys.empty?
      wanted.each { |ask| upload(ask, deadline) }
      ok
    end

    # Counts split so each request stays under MAX_BODY_BYTES (measured, not
    # estimated) and MAX_COUNTS_PER_POST. `overflowed` rides on the first.
    def bodies(counts, overflowed)
      out = []
      current = []
      size = 0
      budget = MAX_BODY_BYTES - envelope([], overflowed).bytesize
      counts.each do |fp, c|
        entry = { fp: fp, n: c.n, first: c.first, last: c.last, kind: c.kind }
        entry[:users] = c.users unless c.users.empty?
        entry[:users_overflow] = c.users_overflow if c.users_overflow.positive?
        bytes = JSON.generate(entry).bytesize + 1
        if !current.empty? && (current.length >= MAX_COUNTS_PER_POST || size + bytes > budget)
          out << envelope(current, out.empty? ? overflowed : 0)
          current = []
          size = 0
        end
        current << entry
        size += bytes
      end
      out << envelope(current, out.empty? ? overflowed : 0) unless current.empty?
      out
    end

    def envelope(counts, overflowed)
      body = { v: 1, tenant: '', source: 'server', service: @service, release: @release,
               sensor_id: @sensor_id, counts: counts }
      body[:overflowed] = overflowed if overflowed.positive?
      JSON.generate(body)
    end

    def flush_headers
      { 'content-type' => 'application/json', 'x-adt-key' => @dsn.key,
        'user-agent' => "devbench-ruby/#{Devbench::VERSION}" }
    end

    def asks(response)
      answers(response)[:evidence]
    end

    # What a flush response asked for: evidence (need_evidence), log slices
    # (need_logs trace keys) and the learned rule set (redaction: an array,
    # or nil when the response carried none — which leaves the rules in
    # force, unlike an empty array, which withdraws them).
    def answers(response)
      none = { evidence: [], logs: [], redaction: nil }
      body = response.body.to_s
      return none if body.empty? || body.bytesize > MAX_RESPONSE_BYTES

      parsed = JSON.parse(body)
      return none unless parsed.is_a?(Hash)

      evidence = parsed['need_evidence']
      logs = parsed['need_logs']
      redaction = parsed['redaction']
      {
        evidence: evidence.is_a?(Array) ? evidence.select { |a| a.is_a?(Hash) } : [],
        logs: logs.is_a?(Array) ? logs.filter_map { |l| log_key(l) } : [],
        redaction: redaction.is_a?(Array) ? redaction : nil
      }
    rescue JSON::ParserError
      # Counts were accepted; an unreadable answer only loses the asks.
      none
    end

    def log_key(ask)
      key = ask.is_a?(Hash) ? ask['trace'] : nil
      key.is_a?(String) && !key.empty? && key.bytesize <= 256 ? key : nil
    end

    # Phase 3: the lines this process holds for each requested trace,
    # redacted with the exemplars' strict egress plus the learned rules,
    # POSTed with the secret key. A key with no lines here sends nothing:
    # another process (a Puma worker, a Sidekiq process) may hold them, and
    # ingest does not let a server key's empty answer settle a request.
    def deliver_logs(keys, deadline)
      policy = @policy
      slices = keys.filter_map do |key|
        lines = Logs.lookup(key, MAX_SLICE_LINES)
        next if lines.empty?

        { trace: key, lines: bound_slice(lines.map { |line| egress_line(line, policy) }) }
      end
      slice_bodies(slices).each do |body|
        response = request(:post, @logs_uri, body, flush_headers, deadline)
        next if response.nil? || (200..299).cover?(response.code.to_i)

        code = response.code.to_i
        Devbench.warn_once(:"logs_http_#{code}", "Dev Bench refused log lines: HTTP #{code}: #{explain(code, response)}")
      end
      nil
    rescue StandardError, SystemStackError
      nil
    end

    # A held line is "[<trace>] <SEVERITY> <message>" (Logs). The bracketed
    # trace is our own framing, kept as is; the rest is redacted exactly as
    # the sidecar redacts a line's body.
    OWN_TRACE = %r{\A\[(v1/[A-Za-z0-9_-]{1,64}/[A-Za-z0-9_-]{1,64}/\d{1,3})\] }

    def egress_line(line, policy)
      own = OWN_TRACE.match(line)
      out = if own.nil?
              Scrub.egress(line, policy, @service)
            else
              rest = line[own.end(0)..]
              redacted = Scrub.egress(rest, policy, @service)
              # A message that already carries the trace (Rails embeds its
              # log tags in a multi-line error) comes back prefixed with it.
              redacted.start_with?("[#{own[1]}] ") ? redacted : "[#{own[1]}] #{redacted}"
            end
      out.bytesize > Logs::MAX_LINE_BYTES ? out.byteslice(0, Logs::MAX_LINE_BYTES).scrub('') : out
    end

    # The most recent lines that fit the store's per-slice byte bound.
    def bound_slice(lines)
      total = 0
      kept = []
      lines.reverse_each do |line|
        total += line.bytesize
        break if total > MAX_SLICE_BYTES

        kept << line
      end
      kept.reverse
    end

    # At most MAX_SLICES_PER_POST slices and MAX_BODY_BYTES per request.
    def slice_bodies(slices)
      out = []
      current = []
      size = 0
      budget = MAX_BODY_BYTES - JSON.generate({ slices: [] }).bytesize
      slices.each do |slice|
        bytes = JSON.generate(slice).bytesize + 1
        if !current.empty? && (current.length >= MAX_SLICES_PER_POST || size + bytes > budget)
          out << JSON.generate({ slices: current })
          current = []
          size = 0
        end
        current << slice
        size += bytes
      end
      out << JSON.generate({ slices: current }) unless current.empty?
      out
    end

    # Phase 2: the sidecar's bundle shape, PUT to the presigned URL with no
    # key (it carries its own authorization).
    def upload(ask, deadline)
      fp = ask['fp']
      url = ask['url']
      return nil unless fp.is_a?(String) && url.is_a?(String) && !url.empty?

      expires = ask['expires'].to_i
      return nil if expires.positive? && expires <= Time.now.to_i

      detail = @lock.synchronize { @details[fp] }
      return nil if detail.nil?

      bundle = {
        v: 1, fp: fp, kind: detail.kind,
        template: Scrub.template_text(detail.text, @policy, fp), service: @service,
        exemplars: detail.exemplar.empty? ? [] : [Scrub.egress(detail.exemplar, @policy, @service)]
      }
      bundle[:frames] = detail.frames unless detail.frames.empty?
      response = request(:put, URI(url), JSON.generate(bundle), { 'content-type' => 'application/json' }, deadline)
      response && (200..299).cover?(response.code.to_i) ? response : nil
    rescue StandardError, SystemStackError
      nil
    end

    # One try, then one retry with jitter on a network error or 5xx, inside
    # the deadline. A 4xx is final. Returns the response or nil.
    def request(method, uri, body, headers, deadline)
      2.times do |i|
        remaining = deadline - monotonic
        return nil if remaining < 0.05

        response, error = attempt(method, uri, body, headers, [HTTP_TIMEOUT, remaining].min)
        return response if error.nil? && response.code.to_i < 500

        if error
          Devbench.warn_once(:unreachable, "could not reach Dev Bench at #{@dsn} (#{error.class}); " \
                                           'counts for that window were dropped')
        end
        jitter = 0.1 + rand * 0.4
        sleep(jitter) if i.zero? && deadline - monotonic > jitter + 0.05
      end
      nil
    end

    def attempt(method, uri, body, headers, timeout)
      response = Net::HTTP.start(uri.hostname, uri.port, use_ssl: uri.scheme == 'https',
                                                         open_timeout: timeout, read_timeout: timeout,
                                                         write_timeout: timeout, ssl_timeout: timeout) do |http|
        klass = method == :post ? Net::HTTP::Post : Net::HTTP::Put
        req = klass.new(uri.request_uri, headers)
        req.body = body
        http.request(req)
      end
      [response, nil]
    rescue StandardError => e
      [nil, e]
    end

    def explain(code, response)
      detail = begin
        JSON.parse(response.body.to_s)['error']
      rescue StandardError
        nil
      end
      case code
      when 401 then 'the key in DEVBENCH_DSN was rejected (wrong, revoked, or for another environment)'
      when 429 then 'rate limited; counts for that window were dropped'
      else detail.is_a?(String) ? detail : (response.message.to_s.empty? ? 'unexpected response' : response.message)
      end
    end

    # Ruby's Mutex has no timed lock; spin briefly instead of waiting forever
    # behind a background flush.
    def acquire(mutex, deadline)
      loop do
        return true if mutex.try_lock
        return false if monotonic >= deadline

        sleep 0.01
      end
    end

    def monotonic
      Process.clock_gettime(Process::CLOCK_MONOTONIC)
    end
  end
end
