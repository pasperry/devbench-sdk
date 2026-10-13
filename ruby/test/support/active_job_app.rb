# frozen_string_literal: true

# A minimal, real Rails application with ActiveJob on several real adapters,
# for test/active_job_test.rb and test/active_job_logs_test.rb. The install
# is what a customer's is: the gem, nothing else — the Railtie installs the
# ActiveJob hooks at boot.
#
# Adapters, each a real one:
#   * async   — ActiveJob's own thread pool (one thread, so "the next job on
#               the same worker thread" is deterministic): serialize at
#               enqueue, ActiveJob::Base.execute on another thread, exactly
#               the path Solid Queue and GoodJob take from their workers.
#   * inline  — serialize and execute at once, on the request's thread.
#   * sidekiq — Sidekiq's own adapter (when the caller loaded Sidekiq):
#               a JobWrapper pushed through Sidekiq::Client.
require 'stringio'
ACTIVE_JOB_TEST_LOG = StringIO.new unless defined?(ACTIVE_JOB_TEST_LOG)

class DevbenchActiveJobTestApp < Rails::Application
  config.root = File.expand_path('..', __dir__)
  config.load_defaults "#{Rails::VERSION::MAJOR}.#{Rails::VERSION::MINOR}"
  config.eager_load = false
  config.logger = ActiveSupport::TaggedLogging.new(ActiveSupport::Logger.new(ACTIVE_JOB_TEST_LOG))
  config.log_level = :info
  config.log_tags = [:request_id]
  config.secret_key_base = 'x' * 64
  config.hosts.clear
  config.consider_all_requests_local = false
  config.action_dispatch.show_exceptions = :all
  config.active_job.queue_adapter = :inline
end

DevbenchActiveJobTestApp.initialize!

# What each job saw while it ran, for assertions about the thread's context.
module JobProbe
  @lock = Mutex.new
  @seen = Queue.new

  def self.record(job)
    @seen << { job: job.class.name, trace: Devbench::Current.trace&.to_s, user: Devbench::Current.user,
               thread: Thread.current.object_id }
  end

  def self.next_seen(timeout = 5)
    @seen.pop(timeout: timeout)
  end

  def self.clear
    @seen.clear
  end
end

ASYNC_ADAPTER = ActiveJob::QueueAdapters::AsyncAdapter.new(min_threads: 1, max_threads: 1)

class ApplicationJob < ActiveJob::Base
end

# The demo's R5: a confirmation job that logs what it was doing, then fails.
class OrderConfirmationJob < ApplicationJob
  self.queue_adapter = ASYNC_ADAPTER

  def perform(order_id)
    JobProbe.record(self)
    logger.info "Sending confirmation for order #{order_id} to pat.secret@example.com"
    raise ArgumentError, "confirmation failed for order #{order_id}"
  end
end

class InlineConfirmationJob < ApplicationJob
  self.queue_adapter = :inline

  def perform(order_id)
    JobProbe.record(self)
    logger.info "Inline confirmation for order #{order_id}"
    raise ArgumentError, "inline confirmation failed for order #{order_id}"
  end
end

# Sets a user, reports a handled failure, and succeeds: on a reused pool
# thread, neither may outlive it.
class IdentityJob < ApplicationJob
  self.queue_adapter = ASYNC_ADAPTER

  def perform
    Devbench.set_user(email: 'leak@example.com', account: 'acct-leak')
    Devbench.report_handled(RuntimeError.new('handled in a job'), symbol: 'IdentityJob#perform')
    JobProbe.record(self)
  end
end

class RecordingJob < ApplicationJob
  self.queue_adapter = ASYNC_ADAPTER

  def perform
    JobProbe.record(self)
  end
end

# Fails once, is re-enqueued by retry_on from the worker thread, then fails
# for good: the second attempt must still carry the enqueuing request's trace.
class RetriedJob < ApplicationJob
  self.queue_adapter = ASYNC_ADAPTER
  retry_on KeyError, attempts: 2, wait: 0

  def perform
    JobProbe.record(self)
    raise KeyError, "attempt #{executions}"
  end
end

# Only where the caller loaded Sidekiq (and with it Sidekiq's ActiveJob
# adapter): ActiveJob on Sidekiq runs inside Sidekiq's server middleware.
if defined?(::Sidekiq)
  class SidekiqAdapterJob < ApplicationJob
    self.queue_adapter = :sidekiq

    def perform(order_id)
      JobProbe.record(self)
      logger.info "Sidekiq confirmation for order #{order_id}"
      raise ArgumentError, "sidekiq confirmation failed for order #{order_id}"
    end
  end
end

class EnqueueController < ActionController::Base
  skip_forgery_protection

  def async
    Devbench.set_user(email: 'buyer@example.com', account: 'acct-1')
    OrderConfirmationJob.perform_later(params[:id].to_i)
    render plain: 'queued'
  end

  def inline
    Devbench.set_user(email: 'buyer@example.com', account: 'acct-1')
    InlineConfirmationJob.perform_later(params[:id].to_i)
    render plain: 'unreachable: the inline job raises'
  end

  def retried
    RetriedJob.perform_later
    render plain: 'queued'
  end

  def sidekiq
    SidekiqAdapterJob.perform_later(params[:id].to_i)
    render plain: 'queued'
  end
end

DevbenchActiveJobTestApp.routes.draw do
  post '/orders/:id/async' => 'enqueue#async'
  post '/orders/:id/inline' => 'enqueue#inline'
  post '/retried' => 'enqueue#retried'
  post '/orders/:id/sidekiq' => 'enqueue#sidekiq'
end
