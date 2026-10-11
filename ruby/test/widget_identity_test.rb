# frozen_string_literal: true

require 'minitest/autorun'
require 'base64'
require 'openssl'
require 'stringio'
require 'devbench'

# Devbench::WidgetIdentity beyond the shared vectors (those are in
# widget_identity_vectors_test.rb), Devbench.set_user's role: and chat:, and
# data-widget-identity on devbench_script_tag outside a Rails render (the
# Rails render, through the real middleware, is in rails_logs_test.rb).
class WidgetIdentityTest < Minitest::Test
  SECRET = 'dbws_k7x2m4q9r3t6v8w1y5z0a2b4c6d8e0f1g3h5j7k9m2n4p6q8'
  PUBLIC = 'adt_client_wipub'

  class View
    include Devbench::ViewHelper
  end

  def setup
    Devbench.reset!
    Devbench::Current.clear
    Devbench.instance_variable_set(:@warned, nil)
  end

  def teardown
    Devbench.reset!
    Devbench::Current.clear
    Devbench.instance_variable_set(:@warned, nil)
  end

  # Checks the MAC with the secret and returns the five payload fields.
  def decode(token, secret = SECRET)
    prefix, payload, mac = token.split('.', -1)
    assert_equal 'dbw1', prefix
    expected = Base64.urlsafe_encode64(OpenSSL::HMAC.digest('SHA256', secret, "dbw1.#{payload}"), padding: false)
    assert_equal expected, mac, 'MAC does not verify with the secret'
    fields = Base64.urlsafe_decode64(payload).force_encoding('UTF-8').split("\n", -1)
    assert_equal 5, fields.size, fields.inspect
    fields
  end

  def test_signs_now_by_default
    before = Time.now.to_i
    token = Devbench::WidgetIdentity.sign(SECRET, email: 'pat@dealer.com')
    after = Time.now.to_i
    issued = Integer(decode(token)[0], 10)
    assert_operator issued, :>=, before
    assert_operator issued, :<=, after
  end

  # Go's strings.ToLower maps U+0130 to "i"; Ruby's full mapping makes two
  # characters. The signers must agree (no vector covers it yet).
  def test_dotted_capital_i_lowercases_as_go_does
    token = Devbench::WidgetIdentity.sign(SECRET, email: "\u0130STANBUL@x.tr", issued_at: 1)
    assert_equal 'istanbul@x.tr', decode(token)[1]
  end

  # ASCII whitespace only: Unicode spaces are data.
  def test_trims_ascii_whitespace_only
    token = Devbench::WidgetIdentity.sign(SECRET, account: "\u00a0acct\u2003 \t", issued_at: 1)
    assert_equal "\u00a0acct\u2003", decode(token)[2]
  end

  CONTROLS = ((0x00..0x1f).to_a << 0x7f).map { |c| c.chr(Encoding::UTF_8) }.freeze

  # Every C0 control character and DEL is refused inside every field (#172;
  # the reference signer, internal/widgetid, and Capability 6).
  def test_refuses_every_control_character_inside_every_field
    capture_warnings do
      CONTROLS.each do |c|
        {
          email: { email: "pat#{c}@dealer.com", account: 'dealer-1182' },
          account: { email: 'pat@dealer.com', account: "dealer#{c}1182" },
          role: { email: 'pat@dealer.com', account: 'dealer-1182', role: "admin#{c}owner" }
        }.each do |field, args|
          assert_nil Devbench::WidgetIdentity.sign(SECRET, **args, issued_at: 1),
                     format('U+%04X inside %s', c.ord, field)
        end
      end
    end
  end

  # At an end, a control the trim does not remove is refused. NUL is the one
  # String#strip would drop (and sign); U+001F the one Python's strip would.
  def test_refuses_a_control_character_the_trim_leaves_at_an_end
    capture_warnings do
      ["\0", "\x1f", "\x7f", "\e"].each do |c|
        {
          email: { email: "pat@dealer.com#{c}" },
          account: { account: "#{c}dealer-1182" },
          role: { account: 'dealer-1182', role: "admin#{c}" }
        }.each do |field, args|
          assert_nil Devbench::WidgetIdentity.sign(SECRET, **args, issued_at: 1), "#{c.inspect} at an end of #{field}"
        end
      end
    end
  end

  # The trimmed whitespace (TAB, LF, VT, FF, CR, space) is C0 too: at the
  # ends it is trimmed, never refused, and the token is the clean identity's.
  def test_trimmed_whitespace_at_the_ends_still_signs
    want = Devbench::WidgetIdentity.sign(SECRET, email: 'pat@dealer.com', account: 'dealer-1182', role: 'admin',
                                                 issued_at: 1)
    refute_nil want
    [' ', "\t", "\n", "\v", "\f", "\r", " \t\n\v\f\r"].each do |ws|
      got = Devbench::WidgetIdentity.sign(SECRET, email: "#{ws}Pat@Dealer.com#{ws}", account: "#{ws}dealer-1182#{ws}",
                                                  role: "#{ws}admin#{ws}", issued_at: 1)
      assert_equal want, got, "#{ws.inspect} around every field"
    end
  end

  def test_non_string_values_are_their_text
    token = Devbench::WidgetIdentity.sign(SECRET, account: 1182, role: :admin, issued_at: 1)
    assert_equal ['1', '', '1182', 'admin', ''], decode(token)
  end

  def test_refuses_what_it_cannot_sign_exactly
    refused = {
      'invalid UTF-8' => { email: "pat\xff@dealer.com".b.force_encoding('UTF-8') },
      'invalid binary' => { account: "acct\xff".b },
      'not a list' => { email: 'pat@dealer.com', chat: :report },
      'a capability that is not text' => { email: 'pat@dealer.com', chat: [1] },
      'a long email (the sensor identity truncates; this must not)' => { email: "#{'a' * 251}@x.io" },
      'a bad time' => { email: 'pat@dealer.com', issued_at: 'yesterday' },
      'before 1970' => { email: 'pat@dealer.com', issued_at: -1 }
    }
    capture_warnings do
      refused.each do |name, args|
        assert_nil Devbench::WidgetIdentity.sign(SECRET, **args), name
      end
    end
  end

  def test_one_warning_per_process_without_the_identity
    out = capture_warnings do
      3.times { Devbench::WidgetIdentity.sign('', email: 'secret.person@dealer.com') }
      Devbench::WidgetIdentity.sign(SECRET, email: 'secret.person@dealer.com', chat: [:nope])
    end
    lines = out.lines.grep(/widget identity/)
    assert_equal 1, lines.size, out
    refute_includes out, 'secret.person'
  end

  def capture_warnings
    original = $stderr
    $stderr = StringIO.new
    yield
    $stderr.string
  ensure
    $stderr = original
  end

  # role: and chat: are the widget's only: the sensor identity (sent with
  # every report) is unchanged.
  def test_set_user_keeps_role_and_chat_out_of_the_sensor_identity
    Devbench.set_user(email: ' Pat@Dealer.com ', account: 'd-1', role: 'admin', chat: [:report])
    assert_equal({ email: 'pat@dealer.com', account: 'd-1' }, Devbench::Current.user)
    assert_equal({ email: ' Pat@Dealer.com ', account: 'd-1', role: 'admin', chat: [:report] },
                 Devbench::Current.widget_user)
  end

  def test_set_user_without_identity_clears_the_widget_user
    Devbench.set_user(email: 'pat@dealer.com', role: 'admin')
    Devbench.set_user(role: 'admin')
    assert_nil Devbench::Current.widget_user
  end

  def test_the_widget_user_ends_with_the_request
    Devbench::Current.with(nil) do
      Devbench.set_user(email: 'pat@dealer.com')
      refute_nil Devbench::Current.widget_user
    end
    assert_nil Devbench::Current.widget_user
  end

  # A later change to the app's array does not change what is signed.
  def test_the_chat_list_is_copied
    chat = [:report]
    Devbench.set_user(email: 'pat@dealer.com', chat: chat)
    chat << :handoff
    assert_equal [:report], Devbench::Current.widget_user[:chat]
  end

  def configure(widget_secret)
    Devbench.reset!
    Devbench.configure do |c|
      c.dsn = "https://#{PUBLIC}:adt_server_wisecret@ingest.example.com"
      c.release = 'r1'
      c.widget_secret = widget_secret
    end
  end

  def widget_attr(html)
    html[/data-widget-identity="([^"]*)"/, 1]
  end

  def test_the_tag_carries_the_signed_user
    configure(SECRET)
    Devbench.set_user(email: 'Pat@Dealer.com', account: 'dealer-1182', role: 'dealer_staff',
                      chat: %i[howto report howto])
    html = View.new.devbench_script_tag
    token = widget_attr(html)
    refute_nil token, html
    assert_equal ['pat@dealer.com', 'dealer-1182', 'dealer_staff', 'howto,report'], decode(token)[1..]
    refute_includes html, SECRET
  end

  def test_no_attribute_without_a_secret
    configure(nil)
    Devbench.set_user(email: 'pat@dealer.com')
    html = View.new.devbench_script_tag
    refute_empty html
    assert_nil widget_attr(html), html
  end

  def test_no_attribute_without_a_user
    configure(SECRET)
    html = View.new.devbench_script_tag
    refute_empty html
    assert_nil widget_attr(html), html
  end

  def test_no_attribute_when_the_signer_refuses
    configure(SECRET)
    Devbench.set_user(email: 'pat@dealer.com', chat: [:everything])
    html = nil
    capture_warnings { html = View.new.devbench_script_tag }
    refute_empty html
    assert_nil widget_attr(html), html
  end

  # The sensor identity truncates a long email; the widget must not sign
  # that truncated (different) person.
  def test_no_attribute_for_an_identity_the_sensor_would_truncate
    configure(SECRET)
    Devbench.set_user(email: "#{'a' * 251}@x.io")
    refute_nil Devbench::Current.user
    html = nil
    capture_warnings { html = View.new.devbench_script_tag }
    refute_empty html
    assert_nil widget_attr(html), html
  end

  def test_the_secret_comes_from_the_environment
    ENV['DEVBENCH_WIDGET_SECRET'] = " #{SECRET}\n"
    assert_equal SECRET, Devbench::Configuration.new.widget_secret
    ENV['DEVBENCH_WIDGET_SECRET'] = ''
    assert_nil Devbench::Configuration.new.widget_secret
  ensure
    ENV.delete('DEVBENCH_WIDGET_SECRET')
  end
end
