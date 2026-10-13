# frozen_string_literal: true

# A minimal, real Rails application for test/rails_requests_test.rb
# (Capability 2b): production-like error rendering, the README's
# middleware wiring, and controllers that answer 5xx without raising, take
# a pinned amount of time, stream, and rate-limit.
require 'logger'

# Inside the app (after Devbench::Middleware): ?ms= makes the request take
# exactly that long on the pinned clock, routed or not.
class AdvanceRequestsClock
  def initialize(app)
    @app = app
  end

  def call(env)
    ms = env['QUERY_STRING'].to_s[/(?:\A|&)ms=(\d+)/, 1]
    REQUESTS_CLOCK.advance(ms.to_f) if ms
    @app.call(env)
  end
end

class AdtRequestsTestApp < Rails::Application
  config.root = File.expand_path('..', __dir__)
  config.load_defaults "#{Rails::VERSION::MAJOR}.#{Rails::VERSION::MINOR}"
  config.eager_load = false
  config.logger = Logger.new(nil)
  config.secret_key_base = 'x' * 64
  config.hosts.clear
  config.consider_all_requests_local = false
  config.action_dispatch.show_exceptions = Rails.gem_version >= Gem::Version.new('7.1') ? :all : true
  config.middleware.insert_before 0, ADT::Middleware
  config.middleware.use AdvanceRequestsClock
end

AdtRequestsTestApp.initialize!

class IgnoredByTheAppError < StandardError; end

class DealsController < ActionController::Base
  skip_forgery_protection

  before_action do
    ADT.set_user(email: 'pat@example.com', account: 'dealer-1') if params[:user]
  end

  # A 5xx the app returns without raising: the common "upstream is down" path.
  def update
    head params.fetch(:code, 503).to_i
  end

  def raises
    raise NoMethodError, "undefined method `name' for nil"
  end

  # Rails.error.report inside the request is an exception reported.
  def reported
    Rails.error.report(IndexError.new('reported by the app'), handled: true)
    head 500
  end

  def handled
    ADT.report_handled(KeyError.new('handled'), symbol: 'DealsController#handled', reason: 'test')
    head params.fetch(:code, 503).to_i
  end

  def ignored
    raise IgnoredByTheAppError, 'the app said to ignore this'
  end

  def limited
    head 429
  end

  def ok
    render plain: 'ok'
  end
end

class StreamsController < ActionController::Base
  include ActionController::Live

  def events
    response.status = params.fetch(:code, 200).to_i
    response.headers['Content-Type'] = 'text/event-stream'
    sse = ActionController::Live::SSE.new(response.stream)
    sse.write('one')
    sse.write('two')
  ensure
    sse&.close
  end
end

AdtRequestsTestApp.routes.draw do
  put '/deals/:id' => 'deals#update'
  get '/raises' => 'deals#raises'
  get '/reported' => 'deals#reported'
  get '/handled' => 'deals#handled'
  get '/ignored' => 'deals#ignored'
  get '/limited' => 'deals#limited'
  get '/ok' => 'deals#ok'
  get '/events' => 'streams#events'
end
