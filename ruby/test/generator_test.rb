# frozen_string_literal: true

# `bin/rails generate devbench`, run for real: each test writes a temporary
# Rails app skeleton (config/application.rb, environment, bin/rails, a
# layout, maybe a CSP initializer), runs its bin/rails in a subprocess, and
# asserts on the files it left — including that nothing else changed, that
# a second run changes nothing, and that the app still boots with the CSP
# the generator wrote (the second run boots it).
#
# Rails is a test-only dependency; without it these skip, unless
# ADT_REQUIRE_RAILS is set (CI sets it).

require 'minitest/autorun'
require 'fileutils'
require 'open3'
require 'rbconfig'
require 'tmpdir'

GENERATOR_RAILS_ERROR = begin
  require 'rails'
  nil
rescue LoadError => e
  e
end

class GeneratorTest < Minitest::Test
  LIB = File.expand_path('../lib', __dir__)
  TAG = '<%= devbench_script_tag %>'

  # The layout `rails new` writes (Rails 8), trimmed.
  ERB_LAYOUT = <<~ERB
    <!DOCTYPE html>
    <html>
      <head>
        <title><%= content_for(:title) || "Acme" %></title>
        <meta name="viewport" content="width=device-width,initial-scale=1">
        <%= csrf_meta_tags %>
        <%= csp_meta_tag %>
        <%= stylesheet_link_tag :app, "data-turbo-track": "reload" %>
        <%= javascript_importmap_tags %>
      </head>

      <body>
        <%= yield %>
      </body>
    </html>
  ERB

  HAML_LAYOUT = <<~HAML
    !!!
    %html
      %head
        %title Acme
        = csrf_meta_tags
        = stylesheet_link_tag 'application'
      %body
        = yield
  HAML

  SLIM_LAYOUT = <<~SLIM
    doctype html
    html
      head
        title Acme
        = csrf_meta_tags
        = javascript_importmap_tags

      body
        = yield
  SLIM

  # `rails new`'s initializer: everything commented out.
  CSP_COMMENTED = <<~RUBY
    # Be sure to restart your server when you modify this file.

    # Rails.application.configure do
    #   config.content_security_policy do |policy|
    #     policy.default_src :self, :https
    #     policy.script_src  :self, :https
    #   end
    # end
  RUBY

  # The same, switched on, as an app with a CSP has it.
  CSP_ACTIVE = <<~RUBY
    # Be sure to restart your server when you modify this file.

    Rails.application.configure do
      config.content_security_policy do |policy|
        policy.default_src :self, :https
        policy.font_src    :self, :https, :data
        policy.img_src     :self, :https, :data
        policy.object_src  :none
        policy.script_src  :self, :https # importmap and Turbo
        policy.style_src   :self, :https
        # policy.connect_src :self
        # Specify URI for violation reports
        # policy.report_uri "/csp-violation-report-endpoint"
      end

      config.content_security_policy_nonce_generator = ->(request) { request.session.id.to_s }
      config.content_security_policy_nonce_directives = %w(script-src)
    end
  RUBY

  def setup
    if GENERATOR_RAILS_ERROR
      flunk "Rails is required here: #{GENERATOR_RAILS_ERROR.message}" if ENV['ADT_REQUIRE_RAILS']
      skip "Rails not installed (#{GENERATOR_RAILS_ERROR.message})"
    end
    @root = Dir.mktmpdir('devbench-gen')
    write('config/boot.rb', "$LOAD_PATH.unshift(#{LIB.inspect})\n")
    write('config/application.rb', <<~RUBY)
      require 'rails'
      require 'action_controller/railtie'
      require 'devbench'

      module GenApp
        class Application < Rails::Application
          config.load_defaults "\#{Rails::VERSION::MAJOR}.\#{Rails::VERSION::MINOR}"
          config.eager_load = false
          config.secret_key_base = 'x' * 64
          config.logger = Logger.new(nil)
        end
      end
    RUBY
    write('config/environment.rb', "require_relative 'application'\nRails.application.initialize!\n")
    write('config/routes.rb', "Rails.application.routes.draw do\nend\n")
    write('bin/rails', <<~RUBY)
      APP_PATH = File.expand_path('../config/application', __dir__)
      require_relative '../config/boot'
      require 'rails/commands'
    RUBY
    FileUtils.mkdir_p(File.join(@root, 'log'))
    FileUtils.mkdir_p(File.join(@root, 'tmp'))
  end

  def teardown
    FileUtils.rm_rf(@root) if @root
  end

  def write(path, text)
    full = File.join(@root, path)
    FileUtils.mkdir_p(File.dirname(full))
    File.write(full, text)
  end

  def read(path)
    File.read(File.join(@root, path))
  end

  # Every file under the app and its content, minus log/ and tmp/ (Rails'
  # own runtime output).
  def snapshot
    Dir.glob('**/*', File::FNM_DOTMATCH, base: @root)
       .reject { |p| p.start_with?('log/', 'tmp/') || File.directory?(File.join(@root, p)) }
       .sort.to_h { |p| [p, read(p)] }
  end

  def generate(env = {})
    clean = { 'DEVBENCH_DSN' => nil, 'ADT_DSN' => nil, 'BUNDLE_GEMFILE' => nil, 'RUBYOPT' => nil }
    out, err, status = Open3.capture3(clean.merge(env), RbConfig.ruby, 'bin/rails', 'generate', 'devbench', chdir: @root)
    assert status.success?, "bin/rails generate devbench failed:\n#{out}\n#{err}"
    out
  end

  # Generates, checks only `changed` files differ, then generates again and
  # checks nothing at all changes.
  def generate_twice(changed, env = {})
    before = snapshot
    out = generate(env)
    after = snapshot
    assert_equal before.keys, after.keys, 'files were created or removed'
    differing = after.keys.reject { |k| before[k] == after[k] }
    assert_equal changed.sort, differing.sort, out

    again = generate(env)
    assert_equal after, snapshot, "a second run changed something:\n#{again}"
    [out, again]
  end

  def test_erb_layout_gets_the_tag_before_head_and_a_commented_csp_is_left_alone
    write('app/views/layouts/application.html.erb', ERB_LAYOUT)
    write('config/initializers/content_security_policy.rb', CSP_COMMENTED)

    out, again = generate_twice(['app/views/layouts/application.html.erb'])

    assert_equal ERB_LAYOUT.sub("    <%= javascript_importmap_tags %>\n",
                                "    <%= javascript_importmap_tags %>\n    #{TAG}\n"), read('app/views/layouts/application.html.erb')
    assert_includes out, 'application.html.erb'
    assert_match(/defines no policy/, out)
    assert_includes out, 'Next:'
    assert_includes out, 'Set DEVBENCH_DSN'
    assert_includes out, 'bin/rails devbench:test'
    assert_match(/identical.*application\.html\.erb/, again)
  end

  def test_an_inline_head_gets_the_tag_inline
    write('app/views/layouts/application.html.erb', "<html><head><title>x</title></head><body><%= yield %></body></html>\n")
    generate_twice(['app/views/layouts/application.html.erb'])

    assert_equal "<html><head><title>x</title>#{TAG}</head><body><%= yield %></body></html>\n",
                 read('app/views/layouts/application.html.erb')
  end

  def test_haml_layout_gets_the_tag_as_the_last_child_of_head
    write('app/views/layouts/application.html.haml', HAML_LAYOUT)
    generate_twice(['app/views/layouts/application.html.haml'])

    assert_equal HAML_LAYOUT.sub("    = stylesheet_link_tag 'application'\n",
                                 "    = stylesheet_link_tag 'application'\n    = devbench_script_tag\n"),
                 read('app/views/layouts/application.html.haml')
  end

  def test_slim_layout_gets_the_tag_as_the_last_child_of_head
    write('app/views/layouts/application.html.slim', SLIM_LAYOUT)
    generate_twice(['app/views/layouts/application.html.slim'])

    assert_equal SLIM_LAYOUT.sub("    = javascript_importmap_tags\n",
                                 "    = javascript_importmap_tags\n    = devbench_script_tag\n"),
                 read('app/views/layouts/application.html.slim')
  end

  def test_no_layout_prints_what_to_add_and_writes_nothing
    write('app/views/layouts/admin.html.erb', ERB_LAYOUT)
    before = snapshot
    out = generate

    assert_equal before, snapshot
    assert_includes out, "Add this to your layout, just before </head>:\n\n    #{TAG}"
  end

  def test_an_active_csp_allows_the_sensor_ingest_and_storage
    write('app/views/layouts/application.html.erb', ERB_LAYOUT)
    write('config/initializers/content_security_policy.rb', CSP_ACTIVE)
    env = { 'DEVBENCH_DSN' => 'https://adt_client_genpub:adt_server_gensecret@ingest.acme.example:8443' }

    out, = generate_twice(['app/views/layouts/application.html.erb', 'config/initializers/content_security_policy.rb'], env)

    csp = read('config/initializers/content_security_policy.rb')
    assert_includes csp, %(    policy.script_src  :self, :https, "https://unpkg.com" # importmap and Turbo\n)
    # connect_src was absent, so it fell back to default_src: keep that, and
    # add the sources Dev Bench reads from DEVBENCH_DSN when the app boots.
    assert_includes csp, %(    policy.default_src :self, :https\n    policy.connect_src :self, :https, *Devbench.csp_connect_sources\n)
    assert_includes csp, '    # policy.connect_src :self', 'a commented line was edited'
    refute_includes csp, 'adt_server_gensecret', 'the secret was written into the app'
    refute_includes csp, 'ingest.acme.example', 'a host frozen at generate time'
    assert_match(/added https:\/\/unpkg.com to script_src/, out)
    assert_match(/added connect_src/, out)

    # The policy the app actually serves follows the DSN it boots with...
    header = csp_header(env)
    assert_includes header, 'script-src \'self\' https: https://unpkg.com'
    assert_includes header, 'connect-src \'self\' https: https://ingest.acme.example:8443 https://*.storage.supabase.co'
    # ...and is left exactly as it was where Dev Bench is not configured.
    assert_includes csp_header({ 'DEVBENCH_DSN' => nil }), "connect-src 'self' https:;"
  end

  def csp_header(env)
    header, err, status = Open3.capture3(env, RbConfig.ruby, 'bin/rails', 'runner',
                                         'print Rails.application.config.content_security_policy.build(nil)',
                                         chdir: @root)
    assert status.success?, err
    header
  end

  def test_an_existing_connect_src_is_extended_with_the_runtime_sources
    write('app/views/layouts/application.html.erb', ERB_LAYOUT)
    write('config/initializers/content_security_policy.rb', <<~RUBY)
      Rails.application.config.content_security_policy do |p|
        p.script_src :self, "https://unpkg.com"
        p.connect_src :self,
                      "wss://cable.acme.example" # websockets
      end
    RUBY

    generate_twice(['app/views/layouts/application.html.erb', 'config/initializers/content_security_policy.rb'])

    assert_equal <<~RUBY, read('config/initializers/content_security_policy.rb')
      Rails.application.config.content_security_policy do |p|
        p.script_src :self, "https://unpkg.com"
        p.connect_src :self,
                      "wss://cable.acme.example", *Devbench.csp_connect_sources # websockets
      end
    RUBY
  end

  def test_no_csp_file_is_left_absent
    write('app/views/layouts/application.html.erb', ERB_LAYOUT)
    out, = generate_twice(['app/views/layouts/application.html.erb'])

    refute File.exist?(File.join(@root, 'config/initializers/content_security_policy.rb'))
    assert_match(/not found: no CSP to change/, out)
  end
end
