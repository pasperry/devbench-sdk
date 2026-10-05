# frozen_string_literal: true

# A minimal, real Rails application for test/rails_test.rb.
require 'stringio'
ADT_TEST_LOG = StringIO.new

class AdtRailsTestApp < Rails::Application
  # test/, not the working directory, so the Rails.root branch is what is
  # exercised when frames are made relative.
  config.root = File.expand_path('..', __dir__)
  config.load_defaults "#{Rails::VERSION::MAJOR}.#{Rails::VERSION::MINOR}"
  config.eager_load = false
  # Logged like a typical Rails app (TaggedLogging, log_tags [:request_id]) and
  # captured, so the trace tag the Railtie appends can be asserted on.
  config.logger = ActiveSupport::TaggedLogging.new(Logger.new(ADT_TEST_LOG))
  config.log_tags = [:request_id]
  config.secret_key_base = 'x' * 64
  config.hosts.clear
  config.active_job.queue_adapter = :inline
  # Production behaviour: Rails renders the 500 page itself, so nothing
  # escapes the stack and the exception is only visible in env.
  config.consider_all_requests_local = false
  config.action_dispatch.show_exceptions = Rails.gem_version >= Gem::Version.new('7.1') ? :all : true
  config.middleware.insert_before 0, ADT::Middleware
end

# Before the classes below, as in a real app: initialize! applies framework
# defaults (forgery protection among them) to ActionController::Base and its
# existing descendants, which would undo skip_forgery_protection.
AdtRailsTestApp.initialize!

class FailingJob < ActiveJob::Base
  def perform(message)
    raise ArgumentError, message
  end
end

class DealsController < ActionController::Base
  # A typical customer app is an Angular SPA calling a JSON API.
  skip_forgery_protection

  def update
    ADT.set_user(email: ' Pat@Example.COM ', account: 1182)
    raise NoMethodError, "undefined method `name' for nil"
  end

  def handled
    Rails.error.handle(context: { note: 'x' }) { raise KeyError, 'handled by the app' }
    render plain: 'ok'
  end

  def reported
    Rails.error.report(IndexError.new('reported by the app'), handled: true)
    render plain: 'ok'
  end

  def enqueue
    FailingJob.perform_later('job inside a request')
    render plain: 'ok'
  end

  def ok
    render plain: 'ok'
  end
end

# CSRF failure: a 422, not a failure to report.
class FormsController < ActionController::Base
  protect_from_forgery with: :exception

  def update
    render plain: 'unreachable without a token'
  end
end

AdtRailsTestApp.routes.draw do
  put '/deals/:id' => 'deals#update'
  get '/handled' => 'deals#handled'
  get '/reported' => 'deals#reported'
  get '/enqueue' => 'deals#enqueue'
  get '/ok' => 'deals#ok'
  put '/forms/:id' => 'forms#update'
end
