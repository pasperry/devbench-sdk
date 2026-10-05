# frozen_string_literal: true

require 'minitest/autorun'
require 'adt'
require_relative 'sidecar_helper'

# Unhandled-exception capture in plain Ruby (docs/SERVER_SDK_SPEC.md,
# Capability 2). The Rails hooks are in test/rails_test.rb.

# Stand-in for a Rails constant on the default skip list: a real class with
# the real name. The skip list matches by name precisely so the gem never has
# to load these. test/rails_test.rb uses the real one.
module ActiveRecord
  class RecordNotFound < StandardError; end unless const_defined?(:RecordNotFound)
end

module Raising
  RAISE_LINE = __LINE__ + 3

  def raised(error)
    raise error
  rescue Exception => e # rubocop:disable Lint/RescueException
    e
  end
end

class BacktraceTest < Minitest::Test
  def test_parses_the_ruby_3_3_format
    frames = ADT::Backtrace.frames(["/srv/app/app/models/deal.rb:42:in `block in save'"], '/srv/app')
    assert_equal [{ function: 'block in save', file: 'app/models/deal.rb', line: 42 }], frames
  end

  # 3.4 switched to a straight quote and put the owner in the label.
  def test_parses_the_ruby_3_4_format
    frames = ADT::Backtrace.frames(["/srv/app/app/models/deal.rb:42:in 'block in Deal#save'"], '/srv/app/')
    assert_equal [{ function: 'block in Deal#save', file: 'app/models/deal.rb', line: 42 }], frames
  end

  def test_files_outside_the_root_stay_absolute
    line = "/gems/activerecord-8.0.1/lib/active_record/base.rb:7:in 'ActiveRecord::Base#save!'"
    frame = ADT::Backtrace.frames([line], '/srv/app').first
    assert_equal '/gems/activerecord-8.0.1/lib/active_record/base.rb', frame[:file]
    assert_equal 'ActiveRecord::Base#save!', frame[:function]
  end

  # A sibling directory sharing the root's prefix is not under the root.
  def test_a_prefix_is_not_a_parent_directory
    frame = ADT::Backtrace.frames(["/srv/app2/x.rb:1:in `y'"], '/srv/app').first
    assert_equal '/srv/app2/x.rb', frame[:file]
  end

  def test_lines_without_a_label_and_unparseable_lines_are_kept
    frames = ADT::Backtrace.frames(['/srv/app/bin/run:3', '<internal:kernel>'], '/srv/app')
    assert_equal({ function: '', file: 'bin/run', line: 3 }, frames[0])
    assert_equal({ function: '', file: '<internal:kernel>', line: 0 }, frames[1])
  end

  def test_no_backtrace_is_no_frames
    assert_equal [], ADT::Backtrace.frames(nil, '/srv/app')
  end
end

# The `exception` control message.
class CaptureExceptionTest < Minitest::Test
  include SidecarHelper
  include Raising

  def setup
    ADT::Current.clear
    start_sidecar
  end

  def teardown
    stop_sidecar
    ADT::Current.clear
    ADT::Reporter.app_root = nil
    ADT.ignore_exceptions.replace(ADT::Reporter::DEFAULT_IGNORED.dup)
  end

  def test_explicit_capture_sends_the_full_message
    ADT::Reporter.app_root = File.expand_path('..', __dir__)
    error = raised(NoMethodError.new("undefined method `name' for nil"))

    ADT::Current.with(ADT::Trace.parse('v1/sessX/actY/2')) do
      ADT.set_user(email: 'Pat@Example.com', account: 'acct-1182')
      assert_nil ADT.capture_exception(error, symbol: 'Billing::Sync#run')
    end

    msg = sidecar_message
    assert_equal 1, msg['v']
    assert_equal 'exception', msg['kind']
    assert_equal 'explicit', msg['context']
    assert_equal true, msg['handled']
    assert_equal 'NoMethodError', msg['error']
    assert_equal "undefined method `name' for nil", msg['message']
    assert_equal 'Billing::Sync#run', msg['symbol']
    assert_equal 'v1/sessX/actY/2', msg['trace']
    assert_equal({ 'email' => 'pat@example.com', 'account' => 'acct-1182' }, msg['user'])

    innermost = msg['frames'].first
    assert_equal 'test/exceptions_test.rb', innermost['file'], 'in-app frames are relative to the app root'
    assert_equal Raising::RAISE_LINE, innermost['line']
    assert_match(/raised/, innermost['function'])
  end

  # Without Rails, the root is the working directory.
  def test_frames_are_relative_to_the_working_directory_without_rails
    Dir.chdir(File.expand_path('..', __dir__)) do
      ADT.capture_exception(raised(RuntimeError.new('x')))
    end

    assert_equal 'test/exceptions_test.rb', sidecar_message['frames'].first['file']
  end

  def test_symbol_may_be_empty_and_user_is_omitted
    ADT.capture_exception(raised(RuntimeError.new('x')))

    msg = sidecar_message
    assert_equal '', msg['symbol']
    refute msg.key?('user')
    refute msg.key?('trace')
  end

  def test_message_is_at_most_2000_characters
    ADT.capture_exception(raised(RuntimeError.new('x' * 10_000)))

    message = sidecar_message['message']
    assert_operator message.length, :<=, 2000
    assert_operator message.length, :>=, 1990
  end

  def recurse(depth)
    depth.zero? ? raise(ArgumentError, 'deep') : recurse(depth - 1)
  end

  def test_frames_are_innermost_first_and_at_most_50
    error = begin
      recurse(200)
    rescue ArgumentError => e
      e
    end
    ADT.capture_exception(error)

    frames = sidecar_message['frames']
    assert_equal 50, frames.length
    assert_match(/recurse/, frames.first['function'])
    assert_equal error.backtrace.first.split(':')[1].to_i, frames.first['line']
  end

  # A JSON encoder that raises on one bad byte would lose the whole report.
  def test_invalid_utf8_in_the_message_is_still_reported
    ADT.capture_exception(raised(RuntimeError.new("bad \xFF byte".b)))

    assert_match(/bad .* byte/, sidecar_message['message'])
  end

  # One exception, one report, however many hooks it passes through.
  def test_the_same_exception_is_reported_once
    error = raised(RuntimeError.new('once'))
    ADT.capture_exception(error)
    ADT.capture_exception(error)
    ADT::Reporter.capture(error, context: 'job', handled: false)

    assert_equal 1, sidecar_messages.length
  end

  def test_a_frozen_exception_is_reported_once_and_does_not_raise
    error = raised(RuntimeError.new('frozen')).freeze
    assert_nil ADT.capture_exception(error)
    assert_nil ADT.capture_exception(error)

    assert_equal 1, sidecar_messages.length
  end

  def test_distinct_exceptions_are_each_reported
    ADT.capture_exception(raised(RuntimeError.new('a')))
    ADT.capture_exception(raised(RuntimeError.new('a')))

    assert_equal 2, sidecar_messages.length
  end

  class NotFoundSubclass < ActiveRecord::RecordNotFound; end
  class Forbidden < StandardError; end
  class ForbiddenSubclass < Forbidden; end

  # Status codes are not exceptions.
  def test_the_skip_list_matches_class_and_ancestors_by_name
    ADT.capture_exception(raised(ActiveRecord::RecordNotFound.new('404')))
    ADT.capture_exception(raised(NotFoundSubclass.new('404')))

    assert_empty sidecar_messages
  end

  def test_the_skip_list_is_extendable
    ADT.ignore_exceptions << 'CaptureExceptionTest::Forbidden'
    ADT.capture_exception(raised(ForbiddenSubclass.new('403')))
    ADT.capture_exception(raised(RuntimeError.new('500')))

    assert_equal ['RuntimeError'], sidecar_messages.map { |m| m['error'] }
  end

  def test_process_control_is_not_reported
    ADT.capture_exception(raised(SystemExit.new))
    ADT.capture_exception(raised(Interrupt.new))

    assert_empty sidecar_messages
  end

  def test_an_anonymous_class_reports_its_named_ancestor
    ADT.capture_exception(raised(Class.new(ArgumentError).new('anon')))

    assert_equal 'ArgumentError', sidecar_message['error']
  end

  # A real exception class whose accessors fail. Exists in the wild; this
  # code runs on their failure path.
  class Hostile < StandardError
    def message
      raise NoMethodError, 'broken message'
    end

    def backtrace
      raise SystemStackError, 'broken backtrace'
    end
  end

  # Losing the message or the stack must not lose the report, and must not
  # raise: the class is the fingerprint.
  def test_a_misbehaving_exception_is_still_reported
    assert_nil ADT.capture_exception(Hostile.new, symbol: 'X#y')

    msg = sidecar_message
    assert_equal 'CaptureExceptionTest::Hostile', msg['error']
    assert_equal [], msg['frames']
    refute msg.key?('message')
  end

  def test_never_raises_when_the_sidecar_is_absent
    ADT::Reporter.socket_path = '/nonexistent/definitely/not/here.sock'
    assert_nil ADT.capture_exception(raised(RuntimeError.new('x')))
    assert_nil ADT.capture_exception(nil)
    assert_nil ADT.capture_exception('not an exception')
  end

  # A sidecar that accepts and then stops reading must cost a bounded delay,
  # not a hung request. The payload is sized past the socket buffers so a
  # blocking write would wait forever.
  def test_a_stalled_sidecar_does_not_hang_the_caller
    path = File.join(@sidecar_dir, 'stalled.sock')
    server = UNIXServer.new(path)
    ADT::Reporter.socket_path = path
    held = []
    acceptor = Thread.new { loop { held << server.accept } }

    error = RuntimeError.new('x' * 2000)
    error.set_backtrace(Array.new(50) { |i| "/#{'d' * 1000}/f#{i}.rb:1:in '#{'m' * 1000}'" })

    started = Process.clock_gettime(Process::CLOCK_MONOTONIC)
    worker = Thread.new { ADT.capture_exception(error) }
    finished = worker.join(5)
    elapsed = Process.clock_gettime(Process::CLOCK_MONOTONIC) - started

    acceptor.kill.join
    held.each(&:close)
    server.close

    refute_nil finished, 'capture_exception blocked on a sidecar that stopped reading'
    assert_operator elapsed, :<, 2
  end
end

# The request hook: exceptions escaping the app, and the ones Rails rendered
# into a 500 page itself.
class MiddlewareExceptionTest < Minitest::Test
  include SidecarHelper

  def setup
    ADT::Current.clear
    start_sidecar
  end

  def teardown
    stop_sidecar
    ADT::Current.clear
    ADT.ignore_exceptions.replace(ADT::Reporter::DEFAULT_IGNORED.dup)
  end

  def env_for(controller: 'admin/deal_notes', action: 'update')
    {
      ADT::Trace::RACK_HEADER => 'v1/sessM/actN/0',
      'action_dispatch.request.path_parameters' => { controller: controller, action: action }
    }
  end

  def failing_app(error)
    lambda do |_env|
      ADT.set_user(email: 'pat@example.com')
      raise error
    end
  end

  # ADT observes; it never swallows. The same object must come out, with the
  # backtrace the application raised it with.
  def test_reports_and_re_raises_the_same_exception
    original = RuntimeError.new('kaboom')
    raised = assert_raises(RuntimeError) do
      ADT::Middleware.new(failing_app(original)).call(env_for)
    end

    assert_same original, raised
    assert_match(/exceptions_test\.rb:\d+:in .*failing_app/, raised.backtrace.first,
                 'the backtrace was replaced')
    assert_nil raised.cause

    msg = sidecar_message
    assert_equal 'exception', msg['kind']
    assert_equal 'request', msg['context']
    assert_equal false, msg['handled']
    assert_equal 'RuntimeError', msg['error']
    assert_equal 'kaboom', msg['message']
    assert_equal 'Admin::DealNotesController#update', msg['symbol']
    assert_equal 'v1/sessM/actN/0', msg['trace']
    assert_equal({ 'email' => 'pat@example.com' }, msg['user'], 'the user set during the request')
    refute_empty msg['frames']
  end

  def test_the_request_scope_still_ends_after_an_exception
    assert_raises(RuntimeError) do
      ADT::Middleware.new(failing_app(RuntimeError.new('x'))).call(env_for)
    end
    assert_nil ADT::Current.trace
    assert_nil ADT::Current.user
  end

  Controller = Struct.new(:action_name)
  class DealsController < Controller; end

  # Rails' ShowExceptions renders the 500 itself; nothing escapes.
  def test_reports_an_exception_rails_rendered_itself
    error = RuntimeError.new('rendered as 500')
    app = lambda do |env|
      env['action_controller.instance'] = DealsController.new('show')
      env['action_dispatch.exception'] = error
      [500, { 'content-type' => 'text/html' }, ['<h1>Sorry</h1>']]
    end

    status, _, body = ADT::Middleware.new(app).call(env_for)

    assert_equal 500, status
    assert_equal ['<h1>Sorry</h1>'], body
    msg = sidecar_message
    assert_equal 'request', msg['context']
    assert_equal 'rendered as 500', msg['message']
    assert_equal 'MiddlewareExceptionTest::DealsController#show', msg['symbol'],
                 'the controller instance wins over path parameters'
  end

  def test_symbol_is_empty_when_rails_has_not_routed
    assert_raises(RuntimeError) do
      ADT::Middleware.new(failing_app(RuntimeError.new('x'))).call({})
    end

    assert_equal '', sidecar_message['symbol']
  end

  def test_an_ignored_exception_is_re_raised_but_not_reported
    assert_raises(ActiveRecord::RecordNotFound) do
      ADT::Middleware.new(failing_app(ActiveRecord::RecordNotFound.new)).call(env_for)
    end

    assert_empty sidecar_messages
  end

  def test_an_exit_passes_through_unreported
    assert_raises(SystemExit) do
      ADT::Middleware.new(->(_env) { exit 3 }).call(env_for)
    end

    assert_empty sidecar_messages
  end

  # An exception that escaped and was also left in env is the same failure.
  def test_escaping_and_left_in_env_is_reported_once
    error = RuntimeError.new('both')
    app = lambda do |env|
      env['action_dispatch.exception'] = error
      raise error
    end
    outer = ADT::Middleware.new(ADT::Middleware.new(app))

    assert_raises(RuntimeError) { outer.call(env_for) }
    assert_equal 1, sidecar_messages.length
  end

  class Unprintable
    def to_s
      raise 'no'
    end
  end

  # An exception in our reporting must never replace the app's exception.
  def test_a_failure_while_reporting_never_replaces_the_apps_exception
    ADT.ignore_exceptions << Unprintable.new
    original = CaptureExceptionTest::Hostile.new

    raised = assert_raises(CaptureExceptionTest::Hostile) do
      ADT::Middleware.new(failing_app(original)).call(env_for)
    end
    assert_same original, raised
    # A skip list that cannot be read skips nothing: better a report too many
    # than a failure lost.
    assert_equal 'CaptureExceptionTest::Hostile', sidecar_message['error']
  end

  def test_the_apps_exception_survives_an_absent_sidecar
    ADT::Reporter.socket_path = '/nonexistent/definitely/not/here.sock'
    original = ArgumentError.new('the real problem')

    raised = assert_raises(ArgumentError) do
      ADT::Middleware.new(failing_app(original)).call(env_for)
    end
    assert_same original, raised
  end
end
