# frozen_string_literal: true

require 'minitest/autorun'
require 'json'
require 'devbench'

# Devbench::Session: the token Devbench::SessionMiddleware puts in every HTML
# page. Ingest verifies it with the Go SDK's VerifySession, so a Ruby-minted
# token must be the bytes Go would mint. Both SDKs are held to the shared
# vectors in testdata/session_vectors.json (#181, #563; the Go side is
# sdk/server/go/session_kat_test.go), each row with the clock pinned to its
# own `now`. Same convention as the other vector tests: skip in a vendored
# copy, fail when ADT_REQUIRE_VECTORS=1 (CI, and the public SDK repo's CI).
class SessionTest < Minitest::Test
  SECRET = 'a-tenant-session-signing-secret-32b'
  NOW = 1_700_000_000

  VECTORS_PATH = File.join(ENV['DEVBENCH_TESTDATA'] || File.expand_path('../../../../testdata', __dir__),
                           'session_vectors.json')
  VECTORS = File.exist?(VECTORS_PATH) ? JSON.parse(File.read(VECTORS_PATH)) : nil

  def vectors(section)
    unless VECTORS
      flunk "missing #{VECTORS_PATH}" if ENV['ADT_REQUIRE_VECTORS'] == '1'
      skip 'shared session vectors live in the repo (testdata/), not in a vendored gem'
    end
    rows = VECTORS.fetch(section)
    refute_empty rows, "#{section} is empty; this test would prove nothing"
    rows
  end

  # The clock is the one thing faked: a token embeds its expiry, so a
  # byte-exact comparison needs a fixed now.
  def at(unix)
    fixed = Time.at(unix)
    original = Time.method(:now)
    quietly { Time.define_singleton_method(:now) { fixed } }
    yield
  ensure
    quietly { Time.define_singleton_method(:now, original) }
  end

  # Redefining Time.now is the point here; -w would call it a redefinition.
  def quietly
    verbose = $VERBOSE
    $VERBOSE = nil
    yield
  ensure
    $VERBOSE = verbose
  end

  def test_vector_constants_are_this_sdks
    vectors('mint')
    assert_equal Devbench::Session::MAX_SUBJECT, VECTORS.fetch('max_subject_bytes')
    assert_equal Devbench::Session::DEFAULT_TTL, VECTORS.fetch('default_ttl_seconds')
  end

  def test_mint_vectors_are_reproduced_byte_for_byte
    vectors('mint').each do |v|
      at(v['now']) do
        token = Devbench::Session.mint(v['secret'], v['tenant'], subject: v['subject'], ttl: v['ttl_seconds'])
        assert_equal v['token'], token, "mint: #{v['name']}"
        assert_equal v['subject_kept'].b, Devbench::Session.verify(v['secret'], token).subject.b,
                     "subject kept: #{v['name']}"
      end
    end
  end

  def test_refuse_vectors_are_refused
    vectors('refuse').each do |v|
      assert_raises(ArgumentError, "refuse: #{v['name']}") do
        Devbench::Session.mint(v['secret'], v['tenant'], subject: v['subject'])
      end
    end
  end

  def test_verify_vectors
    vectors('verify').each do |v|
      at(v['now']) do
        case v['result']
        when 'valid'
          got = Devbench::Session.verify(v['secret'], v['token'])
          claims = v.fetch('claims')
          assert_equal [claims['tenant'], claims['subject'].b, claims['expires']],
                       [got.tenant, got.subject.b, got.expires.to_i], "verify: #{v['name']}"
        when 'expired'
          assert_raises(Devbench::Session::Expired, "verify: #{v['name']}") do
            Devbench::Session.verify(v['secret'], v['token'])
          end
        when 'invalid'
          assert_raises(Devbench::Session::Invalid, "verify: #{v['name']}") do
            Devbench::Session.verify(v['secret'], v['token'])
          end
        else
          flunk "unknown result #{v['result'].inspect} in #{v['name']}"
        end
      end
    end
  end

  # Go's base64.RawURLEncoding is the decoder ingest uses: a payload it
  # refuses must be refused here too, even under a good signature. Signed
  # with SECRET so only the payload's encoding is wrong.
  def test_verify_refuses_payload_encodings_go_refuses
    good = 'YWNtZXw0MTAyNDQ4NDAwfHU' # acme|4102448400|u
    {
      'padded' => "#{good}=",
      'standard alphabet' => 'YWNtZXw0MTAyNDQ4NDAwfHU+',
      'one character over' => "#{good}YW", # 25 characters
      'embedded newline' => "YWNtZXw0\nMTAyNDQ4NDAwfHU"
    }.each do |name, payload|
      token = "adts1.#{payload}.#{Devbench::Session.sign(SECRET, payload)}"
      at(NOW) { assert_raises(Devbench::Session::Invalid, name) { Devbench::Session.verify(SECRET, token) } }
    end
    at(NOW) do
      token = "adts1.#{good}.#{Devbench::Session.sign(SECRET, good)}"
      assert_equal 'u', Devbench::Session.verify(SECRET, token).subject
      # A trailing empty part is a fourth part, as in Go's strings.Split.
      assert_raises(Devbench::Session::Invalid) { Devbench::Session.verify(SECRET, "#{token}.") }
    end
  end

  def test_round_trip_on_the_real_clock
    token = Devbench::Session.mint(SECRET, 'acme', subject: 'user-42')
    got = Devbench::Session.verify(SECRET, token)
    assert_equal ['acme', 'user-42'], [got.tenant, got.subject]
    assert_in_delta Time.now.to_i + Devbench::Session::DEFAULT_TTL, got.expires.to_i, 5
  end

  def test_a_non_positive_ttl_falls_back_to_the_default
    at(NOW) do
      default = Devbench::Session.mint(SECRET, 'acme', subject: 'user-42', ttl: Devbench::Session::DEFAULT_TTL)
      [0, -60, nil].each do |ttl|
        token = Devbench::Session.mint(SECRET, 'acme', subject: 'user-42', ttl: ttl)
        assert_equal default, token, "ttl #{ttl.inspect}"
      end
    end
  end

  def test_a_token_cannot_be_verified_under_another_secret
    token = Devbench::Session.mint(SECRET, 'acme')
    assert_raises(Devbench::Session::Invalid) do
      Devbench::Session.verify('a-different-secret-entirely-0000000', token)
    end
  end

  def test_a_swapped_payload_or_flipped_signature_is_rejected
    prefix, payload, sig = Devbench::Session.mint(SECRET, 'acme', subject: 'user-42').split('.')
    forged_payload = Devbench::Session.mint('someone-elses-secret-000000000000', 'globex').split('.')[1]

    assert_raises(Devbench::Session::Invalid) do
      Devbench::Session.verify(SECRET, [prefix, forged_payload, sig].join('.'))
    end

    flipped = sig[0..-2] + (sig[-1] == 'A' ? 'B' : 'A')
    assert_raises(Devbench::Session::Invalid) do
      Devbench::Session.verify(SECRET, [prefix, payload, flipped].join('.'))
    end
  end

  def test_malformed_tokens_are_invalid
    ['', 'garbage', 'adts1', 'adts1.only-two', 'adts2.abc.def', 'adts1..', 'adts1.!!!.xyz', nil, 42].each do |bad|
      assert_raises(Devbench::Session::Invalid, bad.inspect) { Devbench::Session.verify(SECRET, bad) }
    end
  end

  # The payload is pipe-delimited: a pipe in a field would let a crafted
  # subject claim another tenant or expiry.
  def test_the_delimiter_is_refused_in_tenant_and_subject
    assert_raises(ArgumentError) { Devbench::Session.mint(SECRET, 'acme', subject: 'evil|globex|9999999999|') }
    assert_raises(ArgumentError) { Devbench::Session.mint(SECRET, 'acme|x') }
  end

  def test_secret_and_tenant_are_required
    assert_raises(ArgumentError) { Devbench::Session.mint('', 'acme') }
    assert_raises(ArgumentError) { Devbench::Session.mint(nil, 'acme') }
    assert_raises(ArgumentError) { Devbench::Session.mint(SECRET, '  ') }
    assert_raises(Devbench::Session::Invalid) { Devbench::Session.verify('', Devbench::Session.mint(SECRET, 'acme')) }
  end

  # A subject in a binary or other encoding signs its bytes, not a transcoding,
  # and never raises Encoding::CompatibilityError against a non-ASCII tenant
  # (the middleware would swallow that and the page would carry no token).
  def test_subject_encoding_does_not_change_the_token
    at(NOW) do
      subject = 'a' + ('é' * 100) # cut inside a character when read as bytes
      token = Devbench::Session.mint(SECRET, 'acme', subject: subject)
      assert_equal token, Devbench::Session.mint(SECRET, 'acme', subject: subject.b)

      utf8 = Devbench::Session.mint(SECRET, 'café', subject: 'josé')
      assert_equal utf8, Devbench::Session.mint(SECRET, 'café', subject: 'josé'.b)
      assert_equal 'josé'.b, Devbench::Session.verify(SECRET, utf8).subject.b
    end
  end

  # Devbench::SessionMiddleware, the only caller of mint: the token it injects
  # must be one ingest accepts.
  PAGE = '<!doctype html><html><head><title>App</title></head><body>hi</body></html>'

  def through_middleware(content_type, body, **options)
    app = lambda do |_env|
      [200, { 'content-type' => content_type, 'Content-Length' => body.bytesize.to_s }, [body]]
    end
    options = { secret: SECRET, tenant: 'acme' }.merge(options)
    status, headers, out = Devbench::SessionMiddleware.new(app, **options).call({})
    [status, headers, out.to_a.join]
  end

  def test_middleware_injects_a_verifiable_token_into_head
    at(NOW) do
      _status, headers, html = through_middleware('text/html; charset=utf-8', PAGE,
                                                  subject: ->(_env) { 'user-42' })
      token = html[/<meta name="adt-session" content="([^"]+)">/, 1]
      assert_equal Devbench::Session.mint(SECRET, 'acme', subject: 'user-42'), token
      assert_equal %w[acme user-42], Devbench::Session.verify(SECRET, token).to_a.first(2)
      assert_operator html.index('adt-session'), :<, html.index('</head>')
      assert_includes html, '<title>App</title>'
      assert_equal html.bytesize.to_s, headers['Content-Length']
    end
  end

  def test_middleware_leaves_non_html_alone
    json = '{"ok":true}'
    _status, _headers, out = through_middleware('application/json', json)
    assert_equal json, out
  end

  def test_middleware_without_configuration_passes_the_page_through
    _status, _headers, out = through_middleware('text/html', PAGE, secret: nil)
    assert_equal PAGE, out
  end
end
