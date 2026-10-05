# frozen_string_literal: true

# A minimal, real Rails application booted in direct mode — DEVBENCH_DSN
# points at the FakeIngest the test started — for test/rails_logs_test.rb.
# The install is what a customer's is: no initializer, no middleware line,
# only the DSN in the environment.
require 'stringio'
RAILS_LOGS_TEST_LOG = StringIO.new

class DevbenchLogsTestApp < Rails::Application
  config.root = File.expand_path('..', __dir__)
  config.load_defaults "#{Rails::VERSION::MAJOR}.#{Rails::VERSION::MINOR}"
  config.eager_load = false
  config.logger = ActiveSupport::TaggedLogging.new(ActiveSupport::Logger.new(RAILS_LOGS_TEST_LOG))
  config.log_level = :info
  config.log_tags = [:request_id]
  config.secret_key_base = 'x' * 64
  config.hosts.clear
  config.consider_all_requests_local = false
  config.action_dispatch.show_exceptions = :all
  # An app that uses CSP nonces, as Rails' generated initializer suggests.
  config.content_security_policy do |policy|
    policy.script_src :self, 'https://unpkg.com'
  end
  config.content_security_policy_nonce_generator = ->(_request) { 'n0nce-from-the-app' }
  config.content_security_policy_nonce_directives = %w[script-src]
end

DevbenchLogsTestApp.initialize!

class LoggingDealsController < ActionController::Base
  skip_forgery_protection

  # The case triage needs: the app logs what it was doing — with personal
  # data in it, as real apps do — and then fails.
  def update
    ADT.set_user(email: 'pat@example.com', account: 'dealer-1182')
    logger.info "Approving deal #{params[:id]} for pat.secret@example.com card 4111 1111 1111 1111"
    logger.info 'applicant license=D1234567 verified by Acme Ledger'
    logger.warn 'credit check returned no score'
    raise NoMethodError, "undefined method `score' for nil"
  end

  def ok
    logger.info "ok action for request #{params[:n]}"
    render plain: 'ok'
  end

  # A layout's <head>, as the generator leaves it.
  def page
    render inline: "<html><head><title>x</title>\n<%= devbench_script_tag %>\n</head><body></body></html>"
  end
end

DevbenchLogsTestApp.routes.draw do
  put '/deals/:id' => 'logging_deals#update'
  get '/ok' => 'logging_deals#ok'
  get '/page' => 'logging_deals#page'
end
