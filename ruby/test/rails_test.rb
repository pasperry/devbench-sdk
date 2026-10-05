# frozen_string_literal: true

# The Rails hooks, against a real in-process Rails application: real
# middleware stack, real Rails.error, real ActiveSupport::Notifications, real
# ActiveJob. Nothing here is stubbed; what is asserted is what crossed the
# unix socket.
#
# Rails is a test-only dependency and never a gem dependency. Where it is not
# installed these tests skip — unless ADT_REQUIRE_RAILS is set (CI sets it),
# in which case a missing Rails is a failure rather than a silent pass.

require 'minitest/autorun'
require 'logger'
require 'open3'
require 'rbconfig'

RAILS_LOAD_ERROR = begin
  require 'rails'
  require 'action_controller/railtie'
  require 'active_job/railtie'
  nil
rescue LoadError => e
  e
end

# After Rails, as Bundler.require does in config/application.rb.
require 'adt'
require_relative 'sidecar_helper'
require_relative 'ingest_helper'

# Loaded by absolute path (require_relative), as an app's own files are,
# so its frames can be checked against Rails.root.
require_relative 'support/rails_app' if RAILS_LOAD_ERROR.nil?

class RailsHooksTest < Minitest::Test
  include SidecarHelper

  def setup
    if RAILS_LOAD_ERROR
      flunk "Rails is required here but did not load: #{RAILS_LOAD_ERROR.message}" if ENV['ADT_REQUIRE_RAILS']
      skip "Rails not installed (#{RAILS_LOAD_ERROR.message}); the Railtie hooks are untested in this run"
    end
    ADT::Current.clear
    start_sidecar
  end

  def teardown
    stop_sidecar
    ADT::Current.clear
  end

  # Closes the body, as every real server does: since Rails 7.1 the request's
  # log tags are popped (and request instrumentation finished) on close.
  # Without it, tags from one request leak onto the next.
  def request(method, path, headers = {})
    env = Rack::MockRequest.env_for(path, { method: method }.merge(headers))
    status, headers, body = AdtRailsTestApp.call(env)
    body.close if body.respond_to?(:close)
    [status, headers, body]
  end

  def test_the_railtie_loaded
    assert defined?(ADT::Railtie), 'ADT::Railtie must load when Rails is present'
  end

  # No DSN, no browser DSN: the helper renders nothing at all.
  def test_the_script_tag_renders_nothing_without_a_dsn
    status, _, body = request('GET', '/page')
    html = +''
    body.each { |part| html << part }
    assert_equal 200, status
    assert_equal '<head></head>', html
  end

  # Log capture is direct mode's; with the sidecar reading the log stream,
  # the gem keeps nothing and joins nothing.
  def test_sidecar_mode_does_not_capture_logs
    refute Devbench::Logs.active?
    if Rails.logger.respond_to?(:broadcasts)
      assert_equal 0, Rails.logger.broadcasts.count { |l| l.is_a?(Devbench::Logs::CaptureLogger) }
    end
    refute Rails.logger.singleton_class.include?(Devbench::Logs::Tee)
  end

  # The test app inserts ADT::Middleware itself, as every pre-0.5 install
  # does; the Railtie's own insert must then not add a second one.
  def test_an_app_that_inserted_the_middleware_has_it_once
    classes = AdtRailsTestApp.middleware.middlewares.map(&:klass)
    assert_equal 1, classes.count { |k| k.equal?(Devbench::Middleware) }, classes.inspect
    assert classes.first.equal?(Devbench::Middleware), 'still outermost'
  end

  # Capability 1, requirement 2: the trace reaches Rails' own log lines, so
  # the sidecar can index them — after the app's own tags, which survive.
  def test_rails_log_lines_carry_the_trace_after_the_apps_own_tags
    ADT_TEST_LOG.truncate(0)
    ADT_TEST_LOG.rewind
    request('GET', '/ok', 'HTTP_X_ADT_TRACE' => 'v1/sessL/actL/0')
    request('GET', '/ok')

    started = ADT_TEST_LOG.string.lines.grep(/Started GET "\/ok"/)
    assert_equal 2, started.size, ADT_TEST_LOG.string
    assert_match %r{\A\[[0-9a-f-]{36}\] \[v1/sessL/actL/0\] Started GET}, started[0]
    assert_match %r{\A\[[0-9a-f-]{36}\] Started GET}, started[1], 'no trace, no empty tag'
    assert_equal 1, AdtRailsTestApp.config.log_tags.count(ADT::LOG_TAG), 'appended once'
  end

  # The common case: a controller raises, Rails renders its 500 page, and
  # nothing ever escapes to a middleware's rescue.
  def test_a_controller_exception_rendered_as_500_is_reported_once
    status, = request('PUT', '/deals/9', 'HTTP_X_ADT_TRACE' => 'v1/sessR/actS/0')

    assert_equal 500, status
    msg = sidecar_message
    assert_equal 'exception', msg['kind']
    assert_equal 'request', msg['context']
    assert_equal false, msg['handled']
    assert_equal 'NoMethodError', msg['error']
    assert_equal "undefined method `name' for nil", msg['message']
    assert_equal 'DealsController#update', msg['symbol']
    assert_equal 'v1/sessR/actS/0', msg['trace']
    assert_equal({ 'email' => 'pat@example.com', 'account' => '1182' }, msg['user'])

    innermost = msg['frames'].first
    assert_equal 'support/rails_app.rb', innermost['file'], 'frames are relative to Rails.root'
    assert_match(/update/, innermost['function'])
  end

  def test_a_routing_error_is_not_reported
    status, = request('GET', '/no/such/route')

    assert_equal 404, status
    assert_empty sidecar_messages
  end

  def test_a_csrf_failure_is_not_reported
    status, = request('PUT', '/forms/1')

    assert_equal 422, status
    assert_empty sidecar_messages
  end

  def test_rails_error_handle_is_reported_as_handled
    status, = request('GET', '/handled')

    assert_equal 200, status
    msg = sidecar_message
    assert_equal 'rails_error', msg['context']
    assert_equal true, msg['handled']
    assert_equal 'KeyError', msg['error']
    assert_equal 'DealsController#handled', msg['symbol']
  end

  def test_rails_error_report_passes_handled_through
    request('GET', '/reported')

    msg = sidecar_message
    assert_equal 'rails_error', msg['context']
    assert_equal true, msg['handled']
    assert_equal 'IndexError', msg['error']
  end

  def test_a_failing_job_is_reported_once
    assert_raises(ArgumentError) { FailingJob.perform_now('job failed') }

    msg = sidecar_message
    assert_equal 'job', msg['context']
    assert_equal false, msg['handled']
    assert_equal 'ArgumentError', msg['error']
    assert_equal 'job failed', msg['message']
    assert_equal 'FailingJob#perform', msg['symbol']
  end

  # One exception passes the job hook, Rails.error, and the request's 500:
  # three hooks, one report.
  def test_a_job_failing_inside_a_request_is_reported_once
    status, = request('GET', '/enqueue')

    assert_equal 500, status
    msg = sidecar_message
    assert_equal 'job inside a request', msg['message']
    assert_equal 'FailingJob#perform', msg['symbol']
  end

  def test_a_clean_request_sends_nothing
    status, = request('GET', '/ok')

    assert_equal 200, status
    assert_empty sidecar_messages
  end

  # Installing twice (a reloaded initializer) must not double-subscribe.
  def test_installing_again_is_a_no_op
    # Counted directly: dedupe would hide a second subscription from the
    # socket. Rails.error has no public subscriber list.
    listeners = -> { ActiveSupport::Notifications.notifier.listeners_for('perform.active_job').size }
    subscribers = -> { Rails.error.instance_variable_get(:@subscribers).size }
    before = [listeners.call, subscribers.call]

    assert ADT::RailsHooks.install_error_reporter(Rails.error)
    assert ADT::RailsHooks.install_active_job(ActiveSupport::Notifications)

    assert_equal before, [listeners.call, subscribers.call]
  end

  # Since Rails 7.1, a notification subscriber that raises fails the job it
  # instruments. Ours must not, whatever the payload.
  # Rails >= 7.1 reports a rendered 500 from its executor, before the
  # middleware sees it. It is still a request failure.
  def test_the_executors_request_report_is_labelled_request
    Rails.error.report(RuntimeError.new('from the executor'), handled: false,
                                                             source: 'application.action_dispatch')

    assert_equal 'request', sidecar_message['context']
  end

  def test_hooks_never_raise_into_rails
    ActiveSupport::Notifications.instrument('perform.active_job', exception_object: 'not an exception', job: nil)
    ActiveSupport::Notifications.instrument('perform.active_job', 'not a hash')
    Rails.error.report(RuntimeError.new('odd context'), handled: true, context: { controller: Object.new })

    messages = sidecar_messages
    assert_includes messages.map { |m| m['message'] }, 'odd context'

    # Our job hook reports nothing for payloads that are not real failures.
    assert_empty messages.select { |m| m['context'] == 'job' }

    # Rails >= 8.1 reports a notification subscriber that raised to
    # Rails.error, and the bogus payloads above break Rails' *own*
    # ActiveJob log subscriber — so those reports are Rails' failures,
    # correctly forwarded, not ours. Anything else would be a hook of ours
    # raising into Rails.
    others = messages.reject { |m| m['message'] == 'odd context' }
    others.each do |m|
      assert_equal 'rails_error', m['context']
      assert_match %r{active_job/log_subscriber\.rb\z}, m['frames'].first['file'],
                   "an unexpected report — did one of our hooks raise? #{m.inspect}"
    end
  end
end

# The gem must still load, with no Rails, in plain Ruby.
class PlainRubyLoadTest < Minitest::Test
  def test_no_railtie_without_rails
    require 'open3'
    lib = File.expand_path('../lib', __dir__)
    script = 'require "adt"; print defined?(ADT::Railtie).inspect, " ", defined?(Rails).inspect'
    out, err, status = Open3.capture3(RbConfig.ruby, '-w', '-I', lib, '-e', script)

    assert status.success?, err
    assert_equal 'nil nil', out
    assert_empty err.lines.grep(/#{Regexp.escape(lib)}/), "warnings from the gem:\n#{err}"
  end
end

# The Railtie's own middleware insert, in fresh Rails processes: the 0.5
# install is `gem 'devbench'` and nothing else, and a pre-0.5 install that
# inserted it by hand — in application.rb or in an initializer — must not
# end up with two.
class RailtieMiddlewareTest < Minitest::Test
  LIB = File.expand_path('../lib', __dir__)

  def setup
    return unless RAILS_LOAD_ERROR

    flunk "Rails is required here but did not load: #{RAILS_LOAD_ERROR.message}" if ENV['ADT_REQUIRE_RAILS']
    skip "Rails not installed (#{RAILS_LOAD_ERROR.message})"
  end

  # Boots a minimal app with `insertion` in its class body, sends one request
  # whose controller reports a handled failure, and prints how many
  # Devbench::Middleware the stack holds, where the first is, and the
  # handled header (which only the middleware sets).
  def boot(insertion, env = {})
    script = <<~RUBY
      require 'rails'
      require 'action_controller/railtie'
      require 'devbench'

      class App < Rails::Application
        config.root = Dir.pwd
        config.load_defaults "\#{Rails::VERSION::MAJOR}.\#{Rails::VERSION::MINOR}"
        config.eager_load = false
        config.logger = Logger.new(nil)
        config.secret_key_base = 'x' * 64
        config.hosts.clear
        #{insertion}
      end
      App.initialize!

      class PingController < ActionController::Base
        def show
          ADT.report_handled(KeyError.new('k'), symbol: 'PingController#show')
          render plain: 'ok'
        end
      end
      App.routes.draw { get '/ping' => 'ping#show' }

      classes = App.middleware.middlewares.map(&:klass)
      env = Rack::MockRequest.env_for('/ping', 'HTTP_X_ADT_TRACE' => 'v1/s/a/0')
      status, headers, = App.call(env)
      print classes.count { |k| k.equal?(Devbench::Middleware) }, ' ',
            classes.index { |k| k.equal?(Devbench::Middleware) }.inspect, ' ',
            status, ' ', headers['x-adt-handled'].inspect
    RUBY
    env = { 'ADT_SIDECAR_SOCKET' => '/nonexistent/devbench.sock' }.merge(env)
    out, err, status = Open3.capture3(env, RbConfig.ruby, '-w', '-I', LIB, '-e', script)
    assert status.success?, "subprocess failed:\n#{err}"
    assert_empty err.lines.grep(/#{Regexp.escape(LIB)}/), "warnings from the gem:\n#{err}"
    out
  end

  def test_the_railtie_inserts_the_middleware_first
    assert_equal '1 0 200 "1"', boot('')
  end

  def test_an_insert_in_application_rb_is_not_doubled
    assert_equal '1 0 200 "1"', boot('config.middleware.insert_before 0, ADT::Middleware')
  end

  # config/initializers run after the Railtie's initializer, so a check made
  # there would miss this one.
  def test_an_insert_from_an_initializer_is_not_doubled
    assert_equal '1 0 200 "1"', boot(<<~RUBY)
      initializer 'app.devbench', after: :load_config_initializers do |app|
        app.config.middleware.insert_before 0, ADT::Middleware
      end
    RUBY
  end

  # DEVBENCH_SERVICE's default under Rails: the application's module,
  # underscored, as `rails new acme_shop` names it.
  def test_the_service_defaults_to_the_rails_application_name
    script = <<~'RUBY'
      require 'rails'
      require 'devbench'
      module AcmeShop
        class Application < Rails::Application
          config.root = Dir.pwd
          config.eager_load = false
          config.logger = Logger.new(nil)
          config.secret_key_base = 'x' * 64
        end
      end
      AcmeShop::Application.initialize!
      print Devbench.config.resolved_service, ' ',
            Devbench::Configuration.new('DEVBENCH_SERVICE' => 'web').resolved_service
    RUBY
    out, err, status = Open3.capture3({ 'DEVBENCH_SERVICE' => nil }, RbConfig.ruby, '-w', '-I', LIB, '-e', script)
    assert status.success?, err
    assert_equal 'acme_shop web', out
  end

  def test_disabled_inserts_nothing
    assert_equal '0 nil 200 nil', boot('', 'DEVBENCH_ENABLED' => 'false')
  end

  # rake devbench:test, through Rails' own task loading, against a real
  # local ingest.
  def rake_test(status)
    ingest = FakeIngest.new
    ingest.flush_status = status
    script = <<~'RUBY'
      require 'rails'
      require 'rake'
      require 'devbench'
      class App < Rails::Application
        config.root = Dir.pwd
        config.eager_load = false
        config.logger = Logger.new(nil)
        config.secret_key_base = 'x' * 64
      end
      App.initialize!
      App.load_tasks
      Rake::Task['devbench:test'].invoke
    RUBY
    env = { 'DEVBENCH_DSN' => ingest.dsn, 'DEVBENCH_SERVICE' => 'web', 'DEVBENCH_ENABLED' => nil }
    out, err, st = Open3.capture3(env, RbConfig.ruby, '-w', '-I', LIB, '-e', script)
    [st.success?, out + err, ingest.flushes]
  ensure
    ingest&.close
  end

  # The whole 0.5 install: gem + DSN, no middleware line, no initializer. A
  # controller's 500 and a Rails.error.handle reach ingest directly.
  def test_gem_plus_dsn_reports_a_rails_500_directly
    ingest = FakeIngest.new
    script = <<~'RUBY'
      require 'rails'
      require 'action_controller/railtie'
      require 'devbench'
      class App < Rails::Application
        config.root = Dir.pwd
        config.load_defaults "#{Rails::VERSION::MAJOR}.#{Rails::VERSION::MINOR}"
        config.eager_load = false
        config.logger = Logger.new(nil)
        config.secret_key_base = 'x' * 64
        config.hosts.clear
        config.consider_all_requests_local = false
      end
      App.initialize!
      class BoomController < ActionController::Base
        def show
          Devbench.set_user(email: 'Pat@Example.com')
          Rails.error.handle { raise KeyError, 'handled' }
          raise ArgumentError, 'boom'
        end
      end
      App.routes.draw { get '/boom' => 'boom#show' }
      status, = App.call(Rack::MockRequest.env_for('/boom'))
      print status, ' ', Devbench.transport.class, ' ', Devbench.flush!
    RUBY
    env = { 'DEVBENCH_DSN' => ingest.dsn, 'DEVBENCH_SERVICE' => nil, 'DEVBENCH_ENABLED' => nil }
    out, err, st = Open3.capture3(env, RbConfig.ruby, '-w', '-I', LIB, '-e', script)
    assert st.success?, err
    assert_empty err.lines.grep(/#{Regexp.escape(LIB)}/), "warnings from the gem:\n#{err}"
    assert_equal '500 Devbench::DirectTransport true', out

    flushes = ingest.flushes
    assert_equal 1, flushes.length
    counts = flushes.first.json['counts']
    assert_equal %w[error error], counts.map { |c| c['kind'] }.sort
    assert_equal 'app', flushes.first.json['service'], 'top-level App class: no module name to use'
    assert_includes counts.map { |c| c['users'] }, [{ 'email' => 'pat@example.com' }]
  ensure
    ingest&.close
  end

  def test_rake_devbench_test_checks_the_dsn_and_records_nothing
    ok, out, flushes = rake_test(200)
    assert ok, out
    assert_match(/HTTP 200: accepted — tenant "acme"/, out)
    assert_match(/service "web"/, out)
    assert_empty flushes, 'the self-test must not create a fingerprint (it would become an issue)'
  end

  def test_rake_devbench_test_fails_on_a_rejected_key
    ok, out, = rake_test(401)
    refute ok, 'a rejected key must fail the task'
    assert_match(/HTTP 401: the key in DEVBENCH_DSN was rejected/, out)
  end

  # Placed deliberately further in by the app: left where the app put it.
  def test_an_app_placement_elsewhere_is_respected
    count, index, = boot('config.middleware.insert_after ActionDispatch::RequestId, ADT::Middleware').split
    assert_equal '1', count
    refute_equal '0', index
  end
end
