# frozen_string_literal: true

require_relative 'middleware'
require_relative 'rails_hooks'
require_relative 'sidekiq_hooks'
require_relative 'view_helper'

module Devbench
  # Hooks Dev Bench into Rails at boot. Loaded by lib/devbench.rb only when
  # Rails::Railtie is defined, so the gem still loads, with zero
  # dependencies, in plain Ruby.
  class Railtie < ::Rails::Railtie
    # Inserts Devbench::Middleware first in the stack, as sentry-rails
    # inserts its own, so `gem 'devbench'` is the whole install.
    #
    # Unless the app already did: every install before 0.5 has
    # `config.middleware.insert_before 0, ADT::Middleware` in
    # config/application.rb, and ADT::Middleware is this class. Inserting
    # again would put it in the stack twice.
    #
    # Whether the app inserted it is only knowable once every operation the
    # app recorded has been replayed onto the real stack — including ones
    # from config/initializers, which run after this initializer. So the
    # check is itself recorded as an operation, and in the list Rails
    # replays last (delete/move operations), where it sees the finished
    # stack. Rails >= 7.1 records operations as lambdas taking the stack;
    # 7.0 as [method, args, block] it public_sends, which `then` serves.
    initializer 'devbench.middleware', before: :build_middleware_stack do |app|
      Railtie.auto_insert(app.config.middleware)
    end

    # Puts the request's trace on every Rails log line (SERVER_SDK_SPEC,
    # Capability 1, requirement 2), so the sidecar can hand triage the server
    # lines of one user action. Appended to whatever the app already tags
    # with — e.g. [:request_id] — never replacing it. Before the middleware
    # stack is built, which is when Rails::Rack::Logger reads log_tags.
    initializer 'devbench.log_tags', before: :build_middleware_stack do |app|
      tags = Array(app.config.log_tags)
      app.config.log_tags = tags + [Devbench::LOG_TAG] unless tags.include?(Devbench::LOG_TAG)
    rescue StandardError, SystemStackError
      nil
    end

    # <%= devbench_script_tag %> in any view or layout.
    initializer 'devbench.view_helper' do
      ActiveSupport.on_load(:action_view) { include Devbench::ViewHelper }
    end

    # rake devbench:test — check the DSN without waiting for a flush.
    rake_tasks do
      namespace :devbench do
        desc 'Send one test exception straight to Dev Bench and print the HTTP result'
        task test: :environment do
          abort('Dev Bench test failed (see above)') unless Devbench.test!
        end
      end
    end

    config.after_initialize do
      # A diagnostics gem that can fail a boot is worse than none.
      begin
        next unless Devbench.enabled?

        Devbench::RailsHooks.install_error_reporter(::Rails.error) if ::Rails.respond_to?(:error)
        if defined?(::ActiveSupport::Notifications)
          Devbench::RailsHooks.install_active_job(::ActiveSupport::Notifications)
        end
        # Native Sidekiq jobs never reach the ActiveJob hook. after_initialize
        # runs after every gem is required, so Gemfile order does not matter.
        Devbench::SidekiqHooks.install if defined?(::Sidekiq)

        # Server log lines for triage, kept in-process (direct mode only).
        # After the app's initializers, so the logger they configured is the
        # one hooked.
        Devbench::Railtie.capture_logs
      rescue StandardError, SystemStackError
        nil
      end
    end

    class << self
      # Rails.logger, plus Sidekiq's logger when Sidekiq is loaded. A no-op
      # unless reporting is direct.
      def capture_logs
        loggers = [::Rails.logger]
        loggers << ::Sidekiq.logger if defined?(::Sidekiq) && ::Sidekiq.respond_to?(:logger)
        Devbench.capture_logs(loggers.compact)
      rescue StandardError, SystemStackError
        false
      end

      # Records the conditional insert on a MiddlewareStackProxy. Returns
      # true when recorded.
      def auto_insert(proxy)
        operations = proxy.send(:delete_operations)
        check = ->(stack) { insert_unless_present(stack) }
        operations << (lambda_operations?(operations) ? check : [:then, [], check])
        true
      rescue StandardError, SystemStackError
        # A Rails whose proxy no longer looks like this: insert plainly. Two
        # copies (if the app also inserted one) would cost a second scope
        # per request, never a second report — capture dedupes per object.
        begin
          proxy.insert_before(0, Devbench::Middleware)
        rescue StandardError, SystemStackError
          nil
        end
        false
      end

      def insert_unless_present(stack)
        return stack unless Devbench.enabled?

        present = stack.middlewares.any? { |m| m.klass.equal?(Devbench::Middleware) }
        stack.unshift(Devbench::Middleware) unless present
        stack
      end

      private

      def lambda_operations?(operations)
        return operations.first.is_a?(Proc) unless operations.empty?

        ::Rails.gem_version >= Gem::Version.new('7.1')
      end
    end
  end
end
