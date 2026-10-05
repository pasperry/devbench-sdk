# frozen_string_literal: true

require 'minitest/autorun'
require 'devbench'

# devbench_script_tag outside a Rails render (the Rails render, with a CSP
# nonce, is in rails_logs_test.rb; the no-DSN Rails render in rails_test.rb).
class ViewHelperTest < Minitest::Test
  PUBLIC = 'adt_client_vhpub'
  SECRET = 'adt_server_vhsecret'

  class View
    include Devbench::ViewHelper
  end

  class NonceView
    include Devbench::ViewHelper

    def content_security_policy_nonce
      'abc"123'
    end
  end

  def teardown
    Devbench.reset!
  end

  def configure(dsn, release: 'r1', enabled: true)
    Devbench.reset!
    Devbench.configure do |c|
      c.dsn = dsn
      c.release = release
      c.enabled = enabled
    end
  end

  def test_the_tag_carries_the_public_part_only
    configure("https://#{PUBLIC}:#{SECRET}@ingest.example.com")
    html = View.new.devbench_script_tag

    assert_equal %(<script src="https://unpkg.com/devbench@#{Devbench::VERSION}/dist/devbench.min.js" ) +
                 %(data-dsn="https://#{PUBLIC}@ingest.example.com" data-release="r1" defer></script>), html
    refute_includes html, SECRET
  end

  def test_the_src_is_pinned_to_the_gems_version
    assert_equal "https://unpkg.com/devbench@#{Devbench::VERSION}/dist/devbench.min.js", Devbench::ViewHelper.src
    assert_match(/\A\d+\.\d+\.\d+\z/, Devbench::VERSION)
  end

  def test_nothing_without_a_browser_dsn
    configure(nil)
    assert_equal '', View.new.devbench_script_tag
    configure("https://#{SECRET}@ingest.example.com") # 0.5 single key: a server secret
    assert_equal '', View.new.devbench_script_tag
    configure('not a dsn')
    assert_equal '', View.new.devbench_script_tag
    configure("https://#{PUBLIC}:#{SECRET}@ingest.example.com", enabled: false)
    assert_equal '', View.new.devbench_script_tag
  end

  def test_attributes_are_escaped
    configure("https://#{PUBLIC}:#{SECRET}@ingest.example.com", release: %(v1"><script>alert(1)</script>))
    html = NonceView.new.devbench_script_tag

    assert_includes html, 'data-release="v1&quot;&gt;&lt;script&gt;alert(1)&lt;/script&gt;"'
    assert_includes html, 'nonce="abc&quot;123"'
    assert_equal 1, html.scan('<script').size
  end
end
