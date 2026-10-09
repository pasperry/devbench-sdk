# frozen_string_literal: true

# Server log lines in direct mode, against a real in-process Rails
# application booted with only DEVBENCH_DSN set (test/support/rails_logs_app.rb)
# and a real HTTP server standing in for ingest (FakeIngest). Real middleware
# stack, real Rails.logger (an ActiveSupport::BroadcastLogger on Rails 7.1+),
# real requests. What is asserted is what the buffer holds and, for delivery,
# what arrived over the socket.
#
# Rails is a test-only dependency. Where it is not installed these tests skip
# — unless ADT_REQUIRE_RAILS is set (CI sets it).

require 'minitest/autorun'
require 'logger'
require_relative 'ingest_helper'

RAILS_LOGS_LOAD_ERROR = begin
  require 'rails'
  require 'action_controller/railtie'
  nil
rescue LoadError => e
  e
end

# The customer's environment: one value. Set before the gem and the app
# load, as a deploy's environment is.
RAILS_LOGS_INGEST = FakeIngest.new
%w[DEVBENCH_DSN ADT_DSN DEVBENCH_SERVICE DEVBENCH_ENABLED DEVBENCH_RELEASE].each { |k| ENV.delete(k) }
ENV['DEVBENCH_DSN'] = RAILS_LOGS_INGEST.pair_dsn('adt_client_logs', 'adt_server_logs')
ENV['DEVBENCH_RELEASE'] = 'rel-42'

require 'devbench'
require_relative 'support/rails_logs_app' if RAILS_LOGS_LOAD_ERROR.nil?

module RailsLogsSupport
  def setup
    if RAILS_LOGS_LOAD_ERROR
      flunk "Rails is required here but did not load: #{RAILS_LOGS_LOAD_ERROR.message}" if ENV['ADT_REQUIRE_RAILS']
      skip "Rails not installed (#{RAILS_LOGS_LOAD_ERROR.message})"
    end
    Devbench::Current.clear
    Devbench::Logs.buffer.clear
  end

  def teardown
    Devbench::Current.clear
  end

  def request(method, path, headers = {})
    env = Rack::MockRequest.env_for(path, { method: method }.merge(headers))
    status, headers, body = DevbenchLogsTestApp.call(env)
    body.close if body.respond_to?(:close)
    [status, headers, body]
  end
end

class RailsLogCaptureTest < Minitest::Test
  include RailsLogsSupport

  def test_direct_mode_and_the_capture_logger_joined_the_broadcast
    assert Devbench.direct?, 'DEVBENCH_DSN alone must select direct mode'
    assert Devbench::Logs.active?
    if Rails.gem_version >= Gem::Version.new('7.1')
      assert_kind_of ActiveSupport::BroadcastLogger, Rails.logger
      assert_equal 1, Rails.logger.broadcasts.count { |l| l.is_a?(Devbench::Logs::CaptureLogger) }
    end
    assert_equal Logger::INFO, Rails.logger.level, 'joining must not change the app level'
    refute Rails.logger.debug?
  end

  def test_a_requests_lines_are_kept_under_its_trace
    status, = request('PUT', '/deals/9', 'HTTP_X_ADT_TRACE' => 'v1/sessR/actR/0')
    assert_equal 500, status

    lines = Devbench::Logs.lookup('sessR/actR', 1000)
    joined = lines.join("\n")
    assert_match %r{\A\[v1/sessR/actR/0\] INFO Started PUT "/deals/9"}, lines.first
    assert_includes joined, 'INFO Approving deal 9 for pat.secret@example.com'
    assert_includes joined, 'WARN credit check returned no score'
    assert_includes joined, "undefined method `score' for nil"
    assert(lines.all? { |l| l.start_with?('[v1/sessR/actR/0] ') }, joined)
  end

  # Rails logs the exception and its backtrace as one message with the
  # request's tags after every newline (DebugExceptions#log_array). Each
  # line is held on its own, with the trace once and no tag fragments (#24).
  def test_the_exception_is_held_one_line_each_with_the_trace_once
    request('PUT', '/deals/9', 'HTTP_X_ADT_TRACE' => 'v1/sessT/actT/0')

    lines = Devbench::Logs.lookup('sessT/actT', 1000)
    joined = lines.join("\n")
    errors = lines.select { |l| l.start_with?('[v1/sessT/actT/0] ERROR ') }
    assert_equal "[v1/sessT/actT/0] ERROR NoMethodError (undefined method `score' for nil):", errors.first, joined
    # A backtrace frame (which ones depends on Rails' backtrace cleaner).
    assert_match %r{\A\[v1/sessT/actT/0\] ERROR \S+\.rb:\d+:in .\w+'\z}, errors[1], joined
    assert_operator errors.length, :<=, Devbench::Logs::MAX_MESSAGE_LINES
    lines.each do |line|
      refute_includes line, "\n"
      assert_equal 1, line.scan('v1/sessT/actT/0').size, line
      refute_match(/[0-9a-f]{8}-[0-9a-f]{4}-/, line, 'a request-id tag fragment')
      refute_match(/ERROR\s*\z/, line, 'a blank line')
    end
  end

  # What the app's own logger wrote is what it writes without Dev Bench:
  # its tags, its format, the PII it chose to log — untouched.
  def test_the_apps_own_log_is_unchanged
    RAILS_LOGS_TEST_LOG.truncate(0)
    RAILS_LOGS_TEST_LOG.rewind
    request('PUT', '/deals/9', 'HTTP_X_ADT_TRACE' => 'v1/sessU/actU/0')

    log = RAILS_LOGS_TEST_LOG.string
    assert_match %r{^\[[0-9a-f-]{36}\] \[v1/sessU/actU/0\] Approving deal 9 for pat.secret@example.com card 4111 1111 1111 1111$},
                 log
    assert_equal 1, log.scan('Approving deal 9').size, 'a line was written twice'
  end

  def test_untraced_requests_keep_nothing
    request('GET', '/ok?n=1')
    assert_equal 0, Devbench::Logs.buffer.size
  end

  # Puma threads: concurrent requests, each line under its own request's
  # trace.
  def test_concurrent_requests_keep_their_own_lines
    threads = 6.times.map do |i|
      Thread.new do
        5.times { |n| request('GET', "/ok?n=#{i}-#{n}", 'HTTP_X_ADT_TRACE' => "v1/sessC/act#{i}/0") }
      end
    end
    threads.each(&:join)

    6.times do |i|
      mine = Devbench::Logs.lookup("sessC/act#{i}", 1000).grep(/ok action/)
      assert_equal 5, mine.size, "act#{i}: #{mine.inspect}"
      assert(mine.all? { |l| l.include?("ok action for request #{i}-") }, "act#{i} holds another request's line")
    end
  end
end

# <%= devbench_script_tag %> through a real ActionView render: the public
# part of DEVBENCH_DSN only, the release, the app's CSP nonce.
class RailsScriptTagTest < Minitest::Test
  include RailsLogsSupport

  def page
    status, headers, body = request('GET', '/page')
    html = +''
    body.each { |part| html << part }
    assert_equal 200, status
    [html, headers]
  end

  def test_renders_the_pinned_sensor_with_the_public_dsn_and_the_apps_nonce
    html, headers = page
    tag = html[%r{<script [^>]*></script>}]
    refute_nil tag, html
    assert_equal %(<script src="https://unpkg.com/devbench@#{Devbench::VERSION}/dist/devbench.min.js" ) +
                 %(data-dsn="http://adt_client_logs@127.0.0.1:#{RAILS_LOGS_INGEST.port}" data-release="rel-42" ) +
                 %(nonce="n0nce-from-the-app" defer></script>), tag
    assert_includes headers['content-security-policy'].to_s, "'nonce-n0nce-from-the-app'"
  end

  def test_the_secret_never_reaches_the_html
    html, = page
    refute_includes html, 'adt_server_logs'
  end
end

# The whole loop in one Rails process: a request logs and fails, ingest asks
# for that trace's lines (and sends a learned rule with the ask), and the
# answer arrives redacted.
class RailsLogDeliveryTest < Minitest::Test
  include RailsLogsSupport

  def setup
    super
    RAILS_LOGS_INGEST.need_logs = []
    RAILS_LOGS_INGEST.redaction = nil
  end

  def test_a_failed_requests_lines_reach_ingest_redacted
    status, = request('PUT', '/deals/9', 'HTTP_X_ADT_TRACE' => 'v1/sessD/actD/0')
    assert_equal 500, status
    before = RAILS_LOGS_INGEST.log_deliveries.length
    RAILS_LOGS_INGEST.need_logs = ['sessD/actD', 'sessNobody/actNone']
    RAILS_LOGS_INGEST.redaction = [{ 'kind' => 'field', 'target' => 'license' }]

    assert Devbench.flush!
    delivery = RAILS_LOGS_INGEST.log_deliveries[before]
    refute_nil delivery, 'no POST /v1/logs arrived'
    assert_equal 'adt_server_logs', delivery.headers['x-adt-key']
    slices = delivery.json['slices']
    assert_equal ['sessD/actD'], slices.map { |s| s['trace'] }, 'nothing is sent for a key with no lines'

    lines = slices.first['lines']
    text = lines.join("\n")
    # The diagnostic lines triage reads...
    assert_includes text, '[v1/sessD/actD/0] INFO Started PUT <str>'
    assert_includes text, '[v1/sessD/actD/0] WARN credit check returned no score'
    assert_includes text, 'NoMethodError (undefined method `score'
    assert_includes text, 'INFO Approving deal <num> for <email> card <num> <num> <num> <num>'
    # ...without the personal data the app logged, and with the learned rule.
    assert_includes text, 'applicant license=<redacted:field> verified by <redacted:name>'
    %w[pat.secret@example.com 4111 D1234567 Acme].each { |leak| refute_includes delivery.body, leak }
    assert(lines.all? { |l| l.start_with?('[v1/sessD/actD/0] ') && !l.start_with?('[v1/sessD/actD/0] [v1/') }, text)
    # The exception arrives as readable lines (#24): its message, then its
    # frames, one per line, no tag fragments; the slice bounds hold.
    assert_includes lines, "[v1/sessD/actD/0] ERROR NoMethodError (undefined method `score' for nil):"
    assert(lines.any? { |l| l.match?(%r{\A\[v1/sessD/actD/0\] ERROR \S+\.rb:<num>:in }) }, text)
    refute_includes text, '<uuid>]'
    lines.each { |l| assert_equal 1, l.scan('v1/sessD/actD/0').size, l }
    assert_operator lines.length, :<=, 200
    assert_operator lines.sum(&:bytesize), :<=, 64 * 1024
  end
end
