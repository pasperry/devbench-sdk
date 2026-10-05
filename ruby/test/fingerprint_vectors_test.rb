# frozen_string_literal: true

require 'minitest/autorun'
require 'json'
require 'devbench'
require 'devbench/fingerprint'

# The fingerprint contract, shared with the Go sidecar and the browser SDK
# (docs/SERVER_SDK_SPEC.md, "Conformance"). Direct mode fingerprints
# in-process; if it disagreed with the sidecar by one byte, a customer moving
# between modes would see every issue split in two.
#
# The vectors live at the repo root, outside this gem: a vendored copy skips,
# and ADT_REQUIRE_VECTORS=1 (CI) turns a missing file into a failure.
class FingerprintVectorsTest < Minitest::Test
  # DEVBENCH_TESTDATA: the public SDK repo's CI, whose layout differs.
  VECTORS_PATH = File.join(ENV['DEVBENCH_TESTDATA'] || File.expand_path('../../../../testdata', __dir__), 'fingerprint_vectors.json')
  VECTORS = File.exist?(VECTORS_PATH) ? JSON.parse(File.read(VECTORS_PATH)) : nil

  def setup
    return if VECTORS

    flunk "missing #{VECTORS_PATH}" if ENV['ADT_REQUIRE_VECTORS'] == '1'
    skip 'shared fingerprint vectors live in the repo (testdata/), not in a vendored gem'
  end

  def compute(signal)
    Devbench::Fingerprint.compute(
      kind: signal['kind'], source: signal['source'], service: signal['service'].to_s,
      type: signal['type'].to_s, message: signal['message'].to_s, frames: signal['frames'] || []
    )
  end

  def test_every_vector_reproduces_exactly
    refute_empty VECTORS, 'vector file is empty'

    VECTORS.each do |v|
      assert_equal v['expected_fp'], compute(v['signal']), "fingerprint for #{v['name']}"
    end
  end

  # The intermediate form too, so a mismatch says which part diverged.
  def test_every_normalized_form_matches
    VECTORS.each do |v|
      s = v['signal']
      norm = Devbench::Fingerprint.normalize(s['kind'], s['source'], s['service'].to_s, s['type'].to_s,
                                             s['message'].to_s, s['frames'] || [])
      want = v['normalized']
      assert_equal want['template'], norm[:template], "template for #{v['name']}"
      assert_equal want['frames'] || [], norm[:frames], "frames for #{v['name']}"
      assert_equal want['type'], norm[:type], "type for #{v['name']}"
    end
  end
end

# Where Go's regexp and Ruby's differ, pinned against values computed by the
# Go implementation (internal/fingerprint), so the port cannot quietly drift
# back to Ruby semantics.
class FingerprintSemanticsTest < Minitest::Test
  F = Devbench::Fingerprint

  # Go's \b is ASCII: "é" is not a word character, so "é1" has a boundary
  # before the digit. Ruby's \b would see none and leave the number.
  def test_word_boundaries_are_ascii_like_go
    assert_equal 'é<num> x2', F.template('é1 x2')
    assert_equal 'Ünïcödé <dur> µs <dur>', F.template('Ünïcödé 12ms µs 3µs')
  end

  # Go's \s has no \v; a vertical tab is not collapsed.
  def test_vertical_tab_is_not_go_whitespace
    assert_equal "a\vb c", F.template("a\vb \t c")
  end

  # strings.TrimSpace trims Unicode spaces, String#strip does not.
  def test_unicode_spaces_are_trimmed_like_go
    assert_equal 'Boom', F.template(" Boom　")
  end

  def test_dependency_frames_and_line_numbers_do_not_count
    app = [{ function: 'A#b', file: '/srv/app/app/models/a.rb', line: 1 }]
    with_gem = [{ function: 'x', file: '/usr/local/bundle/gems/rack-3/lib/rack.rb', line: 9 }] +
               [{ function: 'A#b', file: '/home/ci/build/app/models/a.rb', line: 77 }]
    a = F.compute(kind: 'error', source: 'server', type: 'E', frames: app)
    b = F.compute(kind: 'error', source: 'server', type: 'E', frames: with_gem)
    assert_equal a, b
  end

  # Only the top five application frames count; deeper ones vary between
  # call paths that share a cause.
  def test_only_five_frames_count
    frames = (1..7).map { |i| { function: "f#{i}", file: "app/x#{i}.rb", line: i } }
    deeper = frames.first(5) + [{ function: 'other', file: 'app/other.rb', line: 1 }]
    assert_equal F.compute(kind: 'error', source: 'server', type: 'E', frames: frames),
                 F.compute(kind: 'error', source: 'server', type: 'E', frames: deeper)
    refute_equal F.compute(kind: 'error', source: 'server', type: 'E', frames: frames),
                 F.compute(kind: 'error', source: 'server', type: 'E', frames: frames.first(4))
  end

  def test_asset_digests_are_stripped
    assert_equal 'assets/application.js', F.normalize_path('https://x.example.com/assets/application-3f9a2b0c4d5e6f70.js')
    assert_equal 'js/index.js', F.normalize_path('/js/index-BRxHn9vj.js')
    assert_equal 'js/feature-flags.js', F.normalize_path('/js/feature-flags.js')
  end

  def test_nothing_to_fingerprint_is_nil
    assert_nil F.compute(kind: 'error', source: 'server')
    assert_nil F.compute(kind: '', source: 'server', type: 'E')
  end

  def test_invalid_utf8_does_not_raise
    refute_nil F.compute(kind: 'error', source: 'server', type: 'E', message: "bad \xff byte".b)
  end
end
