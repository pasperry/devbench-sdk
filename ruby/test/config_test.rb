# frozen_string_literal: true

require 'minitest/autorun'
require 'open3'
require 'rbconfig'
require 'devbench'

# DSN parsing and configuration (docs/SERVER_SDK_SPEC.md, "Dev Bench naming
# and configuration"), and which transport that selects.
class DSNTest < Minitest::Test
  KEY = 'adt_server_abc123'

  def test_https_dsn_gives_the_base_and_the_key
    dsn = Devbench::DSN.parse("https://#{KEY}@adt-ingest.onrender.com")
    assert_equal 'https://adt-ingest.onrender.com', dsn.base
    assert_equal 'https://adt-ingest.onrender.com/v1/flush', dsn.flush_url
    assert_equal KEY, dsn.key
  end

  def test_a_port_is_kept_and_a_path_or_query_ignored
    dsn = Devbench::DSN.parse(" https://#{KEY}@ingest.example.com:8443/some/path?x=1 \n")
    assert_equal 'https://ingest.example.com:8443', dsn.base
    assert_equal KEY, dsn.key
  end

  def test_the_default_port_is_not_spelled_out
    assert_equal 'https://h.example.com', Devbench::DSN.parse("https://#{KEY}@h.example.com:443").base
  end

  def test_plain_http_is_accepted_only_for_loopback
    assert_equal 'http://127.0.0.1:9999', Devbench::DSN.parse("http://#{KEY}@127.0.0.1:9999").base
    assert_equal 'http://localhost:3000', Devbench::DSN.parse("http://#{KEY}@localhost:3000").base

    error = assert_raises(Devbench::DSN::Invalid) { Devbench::DSN.parse("http://#{KEY}@ingest.example.com") }
    assert_match(/plaintext/, error.message)
  end

  def test_invalid_dsns_say_what_is_wrong_and_never_echo_the_key
    {
      '' => /empty/,
      'not a url at all' => /URL|https/,
      "ftp://#{KEY}@host" => /https/,
      'https://ingest.example.com' => /no key/,
      "https://#{KEY}@" => /host|URL/,
      "#{KEY}@ingest.example.com" => /https/
    }.each do |raw, pattern|
      error = assert_raises(Devbench::DSN::Invalid, raw) { Devbench::DSN.parse(raw) }
      assert_match pattern, error.message, raw
      refute_includes error.message, KEY, "#{raw}: the key leaked into the message"
    end
  end

  # The key is a server secret; a DSN printed in a log line must not carry it.
  def test_the_key_is_not_in_to_s_or_inspect
    dsn = Devbench::DSN.parse("https://#{KEY}@h.example.com")
    refute_includes dsn.to_s, KEY
    refute_includes dsn.inspect, KEY
  end
end

class ConfigurationTest < Minitest::Test
  def config(env)
    Devbench::Configuration.new(env)
  end

  def test_dsn_falls_back_to_adt_dsn
    assert_equal 'https://a@x', config('DEVBENCH_DSN' => 'https://a@x', 'ADT_DSN' => 'https://b@y').dsn
    assert_equal 'https://b@y', config('ADT_DSN' => 'https://b@y').dsn
    assert_equal 'https://b@y', config('DEVBENCH_DSN' => '  ', 'ADT_DSN' => 'https://b@y').dsn
    assert_nil config({}).dsn
  end

  def test_service_defaults_to_app_outside_rails
    assert_equal 'app', config({}).resolved_service
    assert_equal 'billing', config('DEVBENCH_SERVICE' => 'billing').resolved_service
  end

  def test_release_falls_back_through_the_ci_variables_in_order
    assert_equal 'r1', config('DEVBENCH_RELEASE' => 'r1', 'GIT_SHA' => 'g').resolved_release
    assert_equal 'g', config('GIT_SHA' => 'g', 'SOURCE_VERSION' => 's').resolved_release
    assert_equal 's', config('SOURCE_VERSION' => 's', 'RENDER_GIT_COMMIT' => 'r').resolved_release
    assert_equal 'r', config('RENDER_GIT_COMMIT' => 'r').resolved_release
    assert_equal '', config({}).resolved_release
  end

  def test_enabled_unless_explicitly_false
    assert config({}).enabled?
    assert config('DEVBENCH_ENABLED' => 'true').enabled?
    %w[false FALSE 0 no off].each do |off|
      refute config('DEVBENCH_ENABLED' => off).enabled?, off
    end
  end
end

# Which transport a process ends up with, each in a fresh process: the
# choice is made once, from the environment the process started with.
class TransportSelectionTest < Minitest::Test
  LIB = File.expand_path('../lib', __dir__)
  CLEAN = { 'DEVBENCH_DSN' => nil, 'ADT_DSN' => nil, 'DEVBENCH_ENABLED' => nil }.freeze

  def ruby(script, env = {})
    out, err, status = Open3.capture3(CLEAN.merge(env), RbConfig.ruby, '-w', '-I', LIB, '-e', script)
    assert status.success?, "subprocess failed:\n#{err}"
    [out, err]
  end

  def test_no_dsn_is_sidecar_mode
    out, = ruby('require "devbench"; print Devbench.transport')
    assert_equal 'Devbench::SidecarTransport', out
  end

  def test_disabled_is_no_transport_and_no_middleware_work
    script = <<~'RUBY'
      require "devbench"
      app = ->(_env) { ADT.report_handled(KeyError.new("k"), symbol: "S#x"); [200, {}, ["ok"]] }
      _, headers, = Devbench::Middleware.new(app).call("HTTP_X_ADT_TRACE" => "v1/s/a/0")
      print Devbench.transport, " ", headers["x-adt-handled"].inspect
    RUBY
    out, = ruby(script, 'DEVBENCH_ENABLED' => 'false', 'DEVBENCH_DSN' => 'https://k@h.example.com')
    assert_equal 'Devbench::NullTransport nil', out
  end

  # One warning, however many reports, and nothing raised into the app.
  def test_a_broken_dsn_warns_once_and_disables_reporting
    script = <<~'RUBY'
      require "devbench"
      3.times { ADT.capture_exception(RuntimeError.new("x")) }
      ADT.report_handled(KeyError.new("k"), symbol: "S#x")
      print Devbench.transport
    RUBY
    out, err = ruby(script, 'DEVBENCH_DSN' => 'http://secretkey@ingest.example.com')
    assert_equal 'Devbench::NullTransport', out
    warnings = err.lines.grep(/\[devbench\]/)
    assert_equal 1, warnings.size, err
    assert_match(/DEVBENCH_DSN uses http/, warnings.first)
    refute_includes err, 'secretkey'
  end

  def test_configure_from_code_replaces_the_environment
    script = <<~'RUBY'
      require "devbench"
      before = Devbench.transport
      Devbench.configure { |c| c.dsn = "nope" }
      print before, " ", Devbench.transport, " ", Devbench.config.dsn
    RUBY
    out, err = ruby(script)
    assert_equal 'Devbench::SidecarTransport Devbench::NullTransport nope', out
    assert_equal 1, err.lines.grep(/\[devbench\] DEVBENCH_DSN is not a URL|\[devbench\] DEVBENCH_DSN must/).size, err
  end

  def test_a_configure_block_that_raises_does_not_raise_into_the_app
    out, = ruby('require "devbench"; Devbench.configure { raise "boom" }; print :ok')
    assert_equal 'ok', out
  end
end
