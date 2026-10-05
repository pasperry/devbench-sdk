# frozen_string_literal: true

# In-process log capture (docs/SERVER_SDK_SPEC.md, "In-process log capture
# (direct mode)"): real ::Logger objects writing to real IO, real threads,
# a real fork. Nothing of ours is stubbed; the clock is injected where age
# eviction is tested (time is on the permitted list), and a clock that
# raises is how a fault inside the hook is provoked.

require 'minitest/autorun'
require 'logger'
require 'stringio'
require 'devbench'

module LogsTestSupport
  def setup
    @saved_buffer = Devbench::Logs.buffer
    Devbench::Logs.buffer = Devbench::Logs::Buffer.new
    Devbench::Logs.activate!
    Devbench::Current.clear
    Thread.current[Devbench::Logs::LAST] = nil
  end

  def teardown
    Devbench::Logs.buffer = @saved_buffer
    Devbench::Logs.deactivate!
    Devbench::Current.clear
  end

  def trace(raw)
    Devbench::Trace.parse(raw) || flunk("bad trace #{raw}")
  end

  # The request scope the middleware and the Sidekiq hook open.
  def within(raw, &block)
    Devbench::Current.with(trace(raw), &block)
  end

  def held(key, limit = 1000)
    Devbench::Logs.lookup(key, limit)
  end

  def plain_logger(io = StringIO.new, level: Logger::DEBUG)
    logger = Logger.new(io)
    logger.level = level
    logger.formatter = proc { |sev, _t, _p, msg| "#{sev}: #{msg}\n" }
    logger
  end
end

class LogBufferTest < Minitest::Test
  include LogsTestSupport

  def test_lines_are_kept_per_key_in_order
    buffer = Devbench::Logs::Buffer.new
    buffer.push('s/a', 'one')
    buffer.push('s/b', 'other')
    buffer.push('s/a', 'two')

    assert_equal %w[one two], buffer.lookup('s/a', 10)
    assert_equal %w[other], buffer.lookup('s/b', 10)
    assert_equal [], buffer.lookup('s/none', 10)
  end

  def test_lookup_returns_the_most_recent_lines_up_to_the_limit
    buffer = Devbench::Logs::Buffer.new
    5.times { |i| buffer.push('s/a', "line #{i}") }
    assert_equal ['line 3', 'line 4'], buffer.lookup('s/a', 2)
  end

  def test_the_line_count_bound_evicts_oldest_first
    buffer = Devbench::Logs::Buffer.new(max_lines: 3)
    buffer.push('s/a', 'a1')
    buffer.push('s/b', 'b1')
    buffer.push('s/a', 'a2')
    buffer.push('s/b', 'b2')

    assert_equal 3, buffer.size
    assert_equal %w[a2], buffer.lookup('s/a', 10)
    assert_equal %w[b1 b2], buffer.lookup('s/b', 10)
  end

  def test_the_byte_bound_evicts_oldest_first
    buffer = Devbench::Logs::Buffer.new(max_bytes: 10)
    buffer.push('s/a', 'aaaa')
    buffer.push('s/a', 'bbbb')
    buffer.push('s/a', 'cccc')

    assert_equal %w[bbbb cccc], buffer.lookup('s/a', 10)
    assert_equal 8, buffer.bytes
  end

  def test_nothing_older_than_the_age_bound_is_kept
    now = 1000.0
    buffer = Devbench::Logs::Buffer.new(max_age: 900, clock: -> { now })
    buffer.push('s/a', 'old')
    now += 600
    buffer.push('s/a', 'newer')
    now += 301 # 'old' is now 901 s old

    assert_equal %w[newer], buffer.lookup('s/a', 10)
    now += 600
    assert_equal [], buffer.lookup('s/a', 10)
    assert_equal 0, buffer.size
  end

  def test_the_default_bounds_are_the_specs
    assert_equal 10_000, Devbench::Logs::MAX_LINES
    assert_equal 4 * 1024 * 1024, Devbench::Logs::MAX_BYTES
    assert_equal 15 * 60, Devbench::Logs::MAX_AGE
    assert_equal 4096, Devbench::Logs::MAX_LINE_BYTES
  end

  # 10,001 real lines through a real logger: the oldest is gone, the
  # newest 10,000 remain.
  def test_ten_thousand_lines_through_a_logger
    logger = plain_logger(File.open(File::NULL, 'w'))
    Devbench::Logs.install(logger)
    within('v1/sessN/actN/0') { 10_001.times { |i| logger.info("line #{i}") } }

    lines = held('sessN/actN', 20_000)
    assert_equal 10_000, lines.length
    assert_equal '[v1/sessN/actN/0] INFO line 1', lines.first
    assert_equal '[v1/sessN/actN/0] INFO line 10000', lines.last
  end

  def test_many_threads_push_without_losing_or_mixing_lines
    logger = plain_logger(File.open(File::NULL, 'w'))
    Devbench::Logs.install(logger)
    threads = 8.times.map do |t|
      Thread.new do
        within("v1/sessT/act#{t}/0") { 200.times { |i| logger.info("t#{t} #{i}") } }
      end
    end
    threads.each(&:join)

    8.times do |t|
      lines = held("sessT/act#{t}")
      assert_equal 200, lines.length, "thread #{t}"
      assert(lines.all? { |l| l.include?(" t#{t} ") }, "thread #{t} holds another thread's line")
      assert_equal "[v1/sessT/act#{t}/0] INFO t#{t} 199", lines.last
    end
  end

  # Puma and Sidekiq fork after boot. A child must not answer for its
  # parent's lines, and the parent keeps its own.
  def test_a_forked_child_starts_with_an_empty_buffer
    skip 'fork unavailable' unless Process.respond_to?(:fork)

    logger = plain_logger(File.open(File::NULL, 'w'))
    Devbench::Logs.install(logger)
    within('v1/sessF/actF/0') { logger.info('parent line') }

    reader, writer = IO.pipe
    pid = fork do
      reader.close
      before = Devbench::Logs.lookup('sessF/actF', 10)
      within('v1/sessF/actF/0') { logger.info('child line') }
      after = Devbench::Logs.lookup('sessF/actF', 10)
      writer.write(Marshal.dump([before, after]))
      writer.close
      exit!(0)
    end
    writer.close
    before, after = Marshal.load(reader.read) # rubocop:disable Security/MarshalLoad
    Process.wait(pid)

    assert_equal [], before, "the child inherited the parent's lines"
    assert_equal ['[v1/sessF/actF/0] INFO child line'], after
    assert_equal ['[v1/sessF/actF/0] INFO parent line'], held('sessF/actF')
  end
end

class LogTeeTest < Minitest::Test
  include LogsTestSupport

  # What the app writes is byte-for-byte what it wrote without us.
  def test_the_apps_output_is_unchanged
    plain_io = StringIO.new
    hooked_io = StringIO.new
    plain = plain_logger(plain_io)
    hooked = plain_logger(hooked_io)
    assert_equal :tee, Devbench::Logs.install(hooked)

    [plain, hooked].each do |logger|
      within('v1/sessU/actU/0') do
        logger.info('Started PUT "/deals/9"')
        logger.warn { 'from a block' }
        logger.debug('progname') { 'block with progname' }
        logger.add(Logger::ERROR, nil, 'progname only')
        logger.error(RuntimeError.new('an exception'))
        logger.info(nil)
      end
      logger.info('untraced')
    end

    assert_equal plain_io.string, hooked_io.string
    refute_empty hooked_io.string
  end

  def test_traced_lines_are_kept_under_the_trace_and_untraced_are_not
    logger = plain_logger
    Devbench::Logs.install(logger)

    logger.info('before any request')
    within('v1/sessA/actA/2') do
      logger.info('Started PUT "/deals/9"')
      logger.warn { 'from a block' }
      logger.add(Logger::ERROR, nil, 'progname only')
      logger.error(RuntimeError.new('an exception'))
    end
    within('v1/sessA/actB/0') { logger.info('another action') }
    logger.info('after the request')

    assert_equal ['[v1/sessA/actA/2] INFO Started PUT "/deals/9"',
                  '[v1/sessA/actA/2] WARN from a block',
                  '[v1/sessA/actA/2] ERROR progname only',
                  "[v1/sessA/actA/2] ERROR an exception (RuntimeError)"], held('sessA/actA')
    assert_equal ['[v1/sessA/actB/0] INFO another action'], held('sessA/actB')
  end

  # The key is session/intent — the sidecar's TraceKey — so every hop of
  # one action is found by one lookup.
  def test_every_hop_of_an_action_is_under_one_key
    logger = plain_logger
    Devbench::Logs.install(logger)
    within('v1/sessH/actH/0') { logger.info('hop zero') }
    within('v1/sessH/actH/1') { logger.info('hop one') }

    assert_equal ['[v1/sessH/actH/0] INFO hop zero', '[v1/sessH/actH/1] INFO hop one'], held('sessH/actH')
  end

  def test_a_block_message_is_evaluated_once
    logger = plain_logger
    Devbench::Logs.install(logger)
    calls = 0
    within('v1/sessB/actB/0') { logger.info { calls += 1; 'counted' } }

    assert_equal 1, calls
    assert_equal ['[v1/sessB/actB/0] INFO counted'], held('sessB/actB')
  end

  # A line the app's logger filtered out is not kept, and its block is not
  # evaluated.
  def test_the_apps_level_is_respected
    logger = plain_logger(level: Logger::INFO)
    Devbench::Logs.install(logger)
    evaluated = false
    within('v1/sessL/actL/0') do
      logger.debug('debug line')
      logger.debug { evaluated = true; 'debug block' }
      logger.info('info line')
    end

    refute evaluated
    assert_equal ['[v1/sessL/actL/0] INFO info line'], held('sessL/actL')
  end

  def test_a_long_line_is_cut_to_four_kib_of_valid_utf8
    logger = plain_logger(File.open(File::NULL, 'w'))
    Devbench::Logs.install(logger)
    within('v1/sessW/actW/0') { logger.info("é#{'x' * 5000}") }
    within('v1/sessW/actW/0') { logger.info("#{'x' * 4078}é€€€") } # cut mid-character

    held('sessW/actW').each do |line|
      assert_operator line.bytesize, :<=, 4096
      assert line.valid_encoding?, line[-10..].inspect
    end
  end

  def test_invalid_bytes_do_not_break_capture
    logger = plain_logger(File.open(File::NULL, 'w'))
    Devbench::Logs.install(logger)
    within('v1/sessX/actX/0') { logger.info("bad \xff bytes".b) }

    line = held('sessX/actX').first
    refute_nil line
    assert line.valid_encoding?
  end

  def test_installing_twice_hooks_once
    io = StringIO.new
    logger = plain_logger(io)
    Devbench::Logs.install(logger)
    Devbench::Logs.install(logger)
    within('v1/sessD/actD/0') { logger.info('once') }

    assert_equal 1, held('sessD/actD').length
    assert_equal 1, io.string.lines.length
  end

  def test_nothing_is_kept_while_capture_is_off
    logger = plain_logger
    Devbench::Logs.install(logger)
    Devbench::Logs.deactivate!
    within('v1/sessO/actO/0') { logger.info('off') }

    assert_equal [], held('sessO/actO')
  end

  # A fault inside the hook — here a clock that raises on every call —
  # never reaches the logging call, and the app's line is still written.
  def test_a_fault_in_capture_never_raises_into_the_logging_call
    io = StringIO.new
    logger = plain_logger(io)
    Devbench::Logs.install(logger)
    Devbench::Logs.buffer = Devbench::Logs::Buffer.new(clock: -> { raise IOError, 'clock broke' })

    within('v1/sessE/actE/0') { logger.info('still written') }

    assert_equal "INFO: still written\n", io.string
  end

  # Mutex#synchronize raises ThreadError inside a signal handler; logging
  # from a trap must not raise because of us.
  def test_logging_from_a_signal_handler_does_not_raise
    skip 'no USR2' unless Signal.list.key?('USR2')

    logger = plain_logger(StringIO.new)
    Devbench::Logs.install(logger)
    raised = :not_run
    previous = trap('USR2') do
      logger.info('from a trap')
      raised = nil
    rescue Exception => e # rubocop:disable Lint/RescueException
      raised = e
    end
    within('v1/sessS/actS/0') do
      Process.kill('USR2', Process.pid)
      deadline = Time.now + 2
      sleep 0.01 while raised == :not_run && Time.now < deadline
    end
    trap('USR2', previous || 'DEFAULT')

    assert_nil raised
  end

  def test_an_object_that_is_not_a_logger_is_left_alone
    assert_nil Devbench::Logs.install(Object.new)
    assert_nil Devbench::Logs.install(nil)
  end
end

# Rails 7.1+: Rails.logger is an ActiveSupport::BroadcastLogger, and the
# capture logger joins it through broadcast_to. ActiveSupport is a test-only
# dependency; without it these skip (unless ADT_REQUIRE_RAILS).
ACTIVE_SUPPORT_LOAD_ERROR = begin
  require 'active_support'
  require 'active_support/logger'
  require 'active_support/broadcast_logger'
  require 'active_support/tagged_logging'
  nil
rescue LoadError => e
  e
end

class BroadcastLoggerCaptureTest < Minitest::Test
  include LogsTestSupport

  def setup
    if ACTIVE_SUPPORT_LOAD_ERROR || !defined?(::ActiveSupport::BroadcastLogger)
      flunk 'ActiveSupport >= 7.1 is required here' if ENV['ADT_REQUIRE_RAILS']
      skip 'ActiveSupport::BroadcastLogger (Rails 7.1+) not installed'
    end
    super
  end

  def rails_logger(io, level: Logger::INFO)
    inner = ActiveSupport::TaggedLogging.new(ActiveSupport::Logger.new(io))
    inner.level = level
    ActiveSupport::BroadcastLogger.new(inner)
  end

  def test_joins_the_broadcast_and_keeps_traced_lines
    io = StringIO.new
    logger = rails_logger(io)
    assert_equal :broadcast, Devbench::Logs.install(logger)
    assert_equal 2, logger.broadcasts.length

    within('v1/sessR/actR/0') do
      logger.tagged('req-1') { logger.info('Started GET "/ok"') }
      logger.info { 'Completed 200 OK' }
    end
    logger.info('untraced')

    assert_equal ['[v1/sessR/actR/0] INFO Started GET "/ok"', '[v1/sessR/actR/0] INFO Completed 200 OK'],
                 held('sessR/actR')
    assert_equal "[req-1] Started GET \"/ok\"\nCompleted 200 OK\nuntraced\n", io.string
  end

  # BroadcastLogger#level is the minimum of its loggers and #debug? asks
  # whether any is at debug: joining it must turn on nothing the app has off.
  def test_joining_changes_no_level_and_follows_the_apps
    logger = rails_logger(StringIO.new, level: Logger::WARN)
    Devbench::Logs.install(logger)

    assert_equal Logger::WARN, logger.level
    refute logger.info?
    refute logger.debug?

    within('v1/sessV/actV/0') { logger.info('below the app level') }
    assert_equal [], held('sessV/actV')

    logger.level = Logger::DEBUG
    assert logger.debug?
    within('v1/sessV/actV/0') { logger.debug('now on') }
    assert_equal ['[v1/sessV/actV/0] DEBUG now on'], held('sessV/actV')

    logger.broadcasts.first.level = Logger::ERROR # set on the app's logger directly
    assert_equal Logger::ERROR, logger.level
    refute logger.warn?
  end

  # Rails.logger.silence must silence capture too.
  def test_silence_applies
    logger = rails_logger(StringIO.new)
    Devbench::Logs.install(logger)
    within('v1/sessQ/actQ/0') { logger.silence { logger.info('silenced') } }

    assert_equal [], held('sessQ/actQ')
  end

  # A tagged block runs once per logger that has #tagged; ours has none.
  def test_a_tagged_block_runs_once
    logger = rails_logger(StringIO.new)
    Devbench::Logs.install(logger)
    runs = 0
    within('v1/sessG/actG/0') { logger.tagged('t') { runs += 1 } }

    assert_equal 1, runs
  end

  def test_installing_twice_joins_once
    logger = rails_logger(StringIO.new)
    Devbench::Logs.install(logger)
    Devbench::Logs.install(logger)

    assert_equal 2, logger.broadcasts.length
  end

  # In a Sidekiq process Rails.logger may broadcast to Sidekiq.logger, and
  # both are hooked. One write is one line; a real repeat is two.
  def test_a_line_reaching_both_hooks_is_kept_once
    sidekiq = plain_logger(StringIO.new, level: Logger::INFO)
    logger = rails_logger(StringIO.new)
    logger.broadcast_to(sidekiq)
    Devbench::Logs.install(logger)
    Devbench::Logs.install(sidekiq)

    within('v1/sessK/actK/0') do
      logger.info('via Rails.logger')
      logger.info('via Rails.logger')
      sidekiq.info('via Sidekiq.logger')
      sidekiq.info('via Sidekiq.logger')
    end

    assert_equal ['[v1/sessK/actK/0] INFO via Rails.logger', '[v1/sessK/actK/0] INFO via Rails.logger',
                  '[v1/sessK/actK/0] INFO via Sidekiq.logger', '[v1/sessK/actK/0] INFO via Sidekiq.logger'],
                 held('sessK/actK')
  end
end
