# frozen_string_literal: true

require 'minitest/autorun'
require 'devbench'

# Devbench::Session: the token Devbench::SessionMiddleware puts in every HTML
# page. Ingest verifies it with the Go SDK's VerifySession, so a Ruby-minted
# token must be the bytes Go would mint. There are no shared testdata/ vectors
# for session tokens yet (#146): KAT below is the Go SDK's own known-answer
# table (sdk/server/go/session_kat_test.go, minted by Go); change a row only
# together with its Go twin.
class SessionTest < Minitest::Test
  SECRET = 'a-tenant-session-signing-secret-32b'
  KAT_NOW = 1_700_000_000
  KAT_EXPIRY = 1_700_003_600

  # [name, subject, subject as kept (at most 128 bytes, whole characters), token]
  KAT = [
    ['plain subject', 'user-42', 'user-42',
     'adts1.YWNtZXwxNzAwMDAzNjAwfHVzZXItNDI.HPdOJfrw2tnU5pQmLcQ5847z3r0yZUFj5sWFUpAU0Zg'],
    ['no subject', '', '',
     'adts1.YWNtZXwxNzAwMDAzNjAwfA.fyO1jBICsrQrfXC_hDEAyieu2os3fSH2ZqwFUfnNSJY'],
    ['ascii over the bound keeps 128 bytes', 'x' * 200, 'x' * 128,
     'adts1.YWNtZXwxNzAwMDAzNjAwfHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4.c9UIiw5LEm3XVeOQOK19XVznTEAUXWbaC5SaYHfGSRg'],
    ['two-byte characters ending on the bound', 'é' * 100, 'é' * 64,
     'adts1.YWNtZXwxNzAwMDAzNjAwfMOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOp.4veV5h1Pe1wXkPuIvSTClWySEroW3o3rMhmYb2W5HZA'],
    ['two-byte character straddling the bound', 'a' + ('é' * 100), 'a' + ('é' * 63),
     'adts1.YWNtZXwxNzAwMDAzNjAwfGHDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6k.kEZibMkUk7hrvzUdgOM9ZxPMY4kDXyP_FI_5BvWZjl0'],
    ['three-byte character straddling the bound', '€' * 60, '€' * 42,
     'adts1.YWNtZXwxNzAwMDAzNjAwfOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrA.WVG9xXu5n2KEON0WELOMrGslRZxlUPdIF6XFODlCQ-g'],
    ['four-byte character straddling the bound', 'a' + ('😀' * 40), 'a' + ('😀' * 31),
     'adts1.YWNtZXwxNzAwMDAzNjAwfGHwn5iA8J-YgPCfmIDwn5iA8J-YgPCfmIDwn5iA8J-YgPCfmIDwn5iA8J-YgPCfmIDwn5iA8J-YgPCfmIDwn5iA8J-YgPCfmIDwn5iA8J-YgPCfmIDwn5iA8J-YgPCfmIDwn5iA8J-YgPCfmIDwn5iA8J-YgPCfmIDwn5iA.3v9yN5wgqPCZhiE0zVleLmrlcUKAPlh_waGoCy4PuWU'],
  ].freeze

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

  def test_mint_matches_the_go_sdk_byte_for_byte
    at(KAT_NOW) do
      KAT.each do |name, subject, _kept, token|
        assert_equal token, Devbench::Session.mint(SECRET, 'acme', subject: subject, ttl: 3600), name
      end
    end
  end

  def test_verify_reads_back_go_minted_tokens
    at(KAT_NOW) do
      KAT.each do |name, _subject, kept, token|
        got = Devbench::Session.verify(SECRET, token)
        assert_equal 'acme', got.tenant, name
        assert_equal kept.b, got.subject.b, name
        assert_equal KAT_EXPIRY, got.expires.to_i, name
      end
    end
  end

  # The same line Go draws: the expiry second verifies, the next one does not.
  def test_expiry_boundary
    token = KAT.first.last
    at(KAT_EXPIRY) { assert_equal 'acme', Devbench::Session.verify(SECRET, token).tenant }
    at(KAT_EXPIRY + 1) do
      assert_raises(Devbench::Session::Expired) { Devbench::Session.verify(SECRET, token) }
    end
  end

  def test_round_trip_on_the_real_clock
    token = Devbench::Session.mint(SECRET, 'acme', subject: 'user-42')
    got = Devbench::Session.verify(SECRET, token)
    assert_equal ['acme', 'user-42'], [got.tenant, got.subject]
    assert_in_delta Time.now.to_i + Devbench::Session::DEFAULT_TTL, got.expires.to_i, 5
  end

  def test_a_non_positive_ttl_falls_back_to_the_default
    at(KAT_NOW) do
      [0, -60, nil].each do |ttl|
        token = Devbench::Session.mint(SECRET, 'acme', subject: 'user-42', ttl: ttl)
        assert_equal KAT.first.last, token, "ttl #{ttl.inspect}"
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
    at(KAT_NOW) do
      _name, subject, _kept, token = KAT[4]
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
    at(KAT_NOW) do
      _status, headers, html = through_middleware('text/html; charset=utf-8', PAGE,
                                                  subject: ->(_env) { 'user-42' })
      token = html[/<meta name="adt-session" content="([^"]+)">/, 1]
      assert_equal KAT.first.last, token
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
