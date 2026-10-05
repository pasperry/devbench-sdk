# frozen_string_literal: true

# A Sidekiq job's log lines in direct mode: real Sidekiq (its Processor, job
# logger, middleware chains and Sidekiq.logger), a real HTTP server standing
# in for ingest. The job's lines are kept under the job's trace — the one
# the request that enqueued it carried — and delivered when asked.
#
# No Redis (see sidekiq_test.rb). Sidekiq is test-only; without it these
# skip, unless ADT_REQUIRE_SIDEKIQ is set (CI sets it).

require 'minitest/autorun'
require 'logger'
require 'securerandom'
require 'stringio'
require_relative 'ingest_helper'

SIDEKIQ_LOGS_LOAD_ERROR = begin
  require 'sidekiq'
  require 'sidekiq/cli' # Sidekiq.server? is true, as in a real Sidekiq process
  require 'sidekiq/processor'
  require 'sidekiq/fetch'
  nil
rescue LoadError => e
  e
end

SIDEKIQ_LOGS_INGEST = FakeIngest.new
%w[DEVBENCH_DSN ADT_DSN DEVBENCH_SERVICE DEVBENCH_ENABLED].each { |k| ENV.delete(k) }
ENV['DEVBENCH_DSN'] = SIDEKIQ_LOGS_INGEST.pair_dsn('adt_client_jobs', 'adt_server_jobs')

require 'devbench'

if SIDEKIQ_LOGS_LOAD_ERROR.nil?
  SIDEKIQ_TEST_LOG = StringIO.new
  Sidekiq.default_configuration.logger = Sidekiq::Logger.new(SIDEKIQ_TEST_LOG, level: Logger::INFO)
  # What a non-Rails Sidekiq process does (the Railtie does it under Rails).
  Devbench::SidekiqHooks.install

  class LoggingSyncWorker
    include Sidekiq::Job
    sidekiq_options retry: false

    def perform(deal_id)
      logger.info "syncing deal #{deal_id} for pat.secret@example.com"
      raise ArgumentError, 'deal sync failed'
    end
  end
end

module SidekiqLogsSupport
  def setup
    if SIDEKIQ_LOGS_LOAD_ERROR
      flunk "Sidekiq is required here but did not load: #{SIDEKIQ_LOGS_LOAD_ERROR.message}" if ENV['ADT_REQUIRE_SIDEKIQ']
      skip "Sidekiq not installed (#{SIDEKIQ_LOGS_LOAD_ERROR.message})"
    end
    Devbench::Current.clear
    Devbench::Logs.buffer.clear
  end

  def process(job)
    config = Sidekiq.default_configuration
    processor = Sidekiq::Processor.new(config.default_capsule) { |*| nil }
    uow = Sidekiq::BasicFetch::UnitOfWork.new('queue:default', Sidekiq.dump_json(job), config)
    processor.send(:process, uow)
  end

  def job_for(klass, *args, **extra)
    {
      'class' => klass.name, 'args' => args, 'jid' => SecureRandom.hex(12),
      'queue' => 'default', 'retry' => false, 'created_at' => Time.now.to_f
    }.merge(extra.transform_keys(&:to_s))
  end
end

class SidekiqLogCaptureTest < Minitest::Test
  include SidekiqLogsSupport

  def test_sidekiqs_logger_is_hooked_in_direct_mode
    assert Devbench.direct?
    assert Devbench::Logs.active?
    assert Sidekiq.logger.singleton_class.include?(Devbench::Logs::Tee)
  end

  def test_a_jobs_lines_are_kept_under_the_jobs_trace
    assert_raises(ArgumentError) do
      process(job_for(LoggingSyncWorker, 77, adt_trace: 'v1/sessJ/actJ/1'))
    end

    lines = Devbench::Logs.lookup('sessJ/actJ', 100)
    assert_equal ['[v1/sessJ/actJ/1] INFO syncing deal 77 for pat.secret@example.com'], lines
    # Sidekiq's own log is what it always was.
    assert_includes SIDEKIQ_TEST_LOG.string, 'syncing deal 77 for pat.secret@example.com'
  end

  def test_a_job_without_a_trace_keeps_nothing
    assert_raises(ArgumentError) { process(job_for(LoggingSyncWorker, 78)) }
    assert_equal 0, Devbench::Logs.buffer.size
  end
end

class SidekiqLogDeliveryTest < Minitest::Test
  include SidekiqLogsSupport

  def test_a_jobs_lines_are_delivered_under_the_jobs_trace
    assert_raises(ArgumentError) do
      process(job_for(LoggingSyncWorker, 91, adt_trace: 'v1/sessQ/actQ/0'))
    end
    before = SIDEKIQ_LOGS_INGEST.log_deliveries.length
    SIDEKIQ_LOGS_INGEST.need_logs = ['sessQ/actQ']

    assert Devbench.flush!
    delivery = SIDEKIQ_LOGS_INGEST.log_deliveries[before]
    refute_nil delivery, 'no POST /v1/logs arrived'
    assert_equal 'adt_server_jobs', delivery.headers['x-adt-key']
    # No DEVBENCH_SERVICE: a Sidekiq process reports as <app>-sidekiq.
    assert_equal 'app-sidekiq', SIDEKIQ_LOGS_INGEST.flushes.last.json['service']
    assert_equal [{ 'trace' => 'sessQ/actQ', 'lines' => ['[v1/sessQ/actQ/0] INFO syncing deal <num> for <email>'] }],
                 delivery.json['slices']
  end
end
