# frozen_string_literal: true

require 'minitest/autorun'
require 'json'
require 'devbench'

# The request-signal contract (docs/server-sdk/failed-and-slow-requests.md,
# Capability 2b), shared with internal/reqsignal and every SDK: which
# requests are failed or slow, how method and route are normalised, what
# each signal fingerprints to, and the 429 spike rule. If this SDK drifted,
# the same app would report a different set of problems in Ruby than in Go.
#
# Every section of the file runs, and an empty section fails: a vector
# file that lost a section must not pass by having nothing to check.
class RequestSignalVectorsTest < Minitest::Test
  RS = Devbench::RequestSignals

  # DEVBENCH_TESTDATA: the public SDK repo's CI, whose layout differs.
  VECTORS_PATH = File.join(ENV['DEVBENCH_TESTDATA'] || File.expand_path('../../../../testdata', __dir__),
                           'request_signal_vectors.json')
  VECTORS = File.exist?(VECTORS_PATH) ? JSON.parse(File.read(VECTORS_PATH)) : nil

  def setup
    return if VECTORS

    flunk "missing #{VECTORS_PATH}" if ENV['ADT_REQUIRE_VECTORS'] == '1'
    skip 'shared request-signal vectors live in the repo (testdata/), not in a vendored gem'
  end

  def section(name)
    cases = VECTORS[name]
    refute_nil cases, "vector section #{name} is missing"
    refute_empty cases, "vector section #{name} is empty"
    cases
  end

  def test_constants
    c = VECTORS['constants']
    refute_nil c, 'constants section is missing'
    assert_equal c['slow_threshold_ms'], RS::SLOW_THRESHOLD_MS
    assert_equal c['spike_window_seconds'], RS::SPIKE_WINDOW_SECONDS
    assert_equal c['spike_min_count'], RS::SPIKE_MIN_COUNT
    assert_equal c['spike_min_percent'], RS::SPIKE_MIN_PERCENT
    assert_equal c['max_routes_per_window'], RS::MAX_ROUTES_PER_WINDOW
    assert_equal c['max_route_chars'], RS::MAX_ROUTE_CHARS
    assert_equal c['unknown_route'], RS::UNKNOWN_ROUTE
    assert_equal c['other_method'], RS::OTHER_METHOD
  end

  # handled_reported is in every case and must change nothing: classify has
  # no input for it, which is the point (report_handled never suppresses or
  # adds a request signal).
  def test_classify
    section('classify').each do |v|
      got = RS.classify(status: v['status'], duration_ms: v['duration_ms'],
                        exception_reported: v['exception_reported'], streaming: v['streaming'])
      assert_equal v['want'], got.to_s, v['name']
    end
  end

  def test_methods
    section('methods').each do |v|
      assert_equal v['out'], RS.normalize_method(v['in']), v['in'].inspect
    end
  end

  def test_routes
    section('routes').each do |v|
      assert_equal v['out'], RS.normalize_route(v['in']), v['in'].inspect
    end
  end

  # Each case through this SDK's own fingerprint code, as direct mode counts
  # it: the kind, the fingerprint signal, expected_fp, the normalised form,
  # the template text and the exemplar.
  def test_build
    section('build').each do |v|
      r = v['request']
      built = RS.build(signal: r['signal'], method: r['method'], route: r['route'], status: r['status'],
                       duration_ms: r['duration_ms'] || 0, count: r['count'] || 0, total: r['total'] || 0)
      refute_nil built, v['name']
      want = v['signal']
      assert_equal v['kind'], built.kind, "kind for #{v['name']}"
      assert_equal want['kind'], built.kind, "signal kind for #{v['name']}"
      assert_equal want['type'], built.type, "type for #{v['name']}"
      assert_equal want['message'], built.message, "message for #{v['name']}"
      assert_equal v['text'], built.text, "text for #{v['name']}"
      assert_equal v['exemplar'], built.exemplar, "exemplar for #{v['name']}"

      fp = Devbench::Fingerprint.compute(kind: built.kind, source: 'server', service: v['service'],
                                         type: built.type, message: built.message)
      assert_equal v['expected_fp'], fp, "fingerprint for #{v['name']}"

      norm = Devbench::Fingerprint.normalize(built.kind, 'server', v['service'], built.type, built.message, [])
      assert_equal v['normalized']['template'], norm[:template], "template for #{v['name']}"
      assert_equal v['normalized']['type'], norm[:type], "normalized type for #{v['name']}"
    end
  end

  def test_invalid_build
    section('invalid_build').each do |v|
      assert_nil RS.build(signal: v['signal'], method: 'GET', route: '/x', status: v['status'], duration_ms: 5),
                 v['name']
    end
  end

  # Steps run in order against one fresh detector; each step's returned
  # spikes must equal `want` exactly (sorted by method then route).
  def test_spikes
    section('spikes').each do |v|
      detector = RS::SpikeDetector.new
      v['steps'].each_with_index do |step, i|
        got = if step['observe']
                o = step['observe']
                (o['repeat'] || 1).times.flat_map do
                  detector.observe(o['t_ms'], o['method'], o['route'], o['status'])
                end
              elsif step.key?('tick_ms')
                detector.tick(step['tick_ms'])
              elsif step['end']
                detector.finish
              else
                flunk "unknown step in #{v['name']}: #{step.inspect}"
              end
        got = got.map { |s| { 'method' => s.method, 'route' => s.route, 'window' => s.window,
                              'count' => s.count, 'total' => s.total } }
        assert_equal step['want'], got, "#{v['name']}, step #{i}"
      end
    end
  end
end

# What the vectors do not pin, held here.
class RequestSignalBoundsTest < Minitest::Test
  RS = Devbench::RequestSignals

  # At most 512 (method, route) keys per window; further keys are not
  # tracked that window, and the next window starts empty.
  def test_at_most_512_routes_per_window
    d = RS::SpikeDetector.new
    t = 1_758_799_981_000
    512.times { |i| d.observe(t, 'GET', "/r#{i}", 200) }
    10.times { d.observe(t, 'GET', '/one-too-many', 429) }
    assert_empty d.finish, 'a 513th key is not tracked'

    10.times { d.observe(t, 'GET', '/one-too-many', 429) }
    assert_equal ['/one-too-many'], d.finish.map(&:route), 'a new window tracks it again'
  end

  # Every server thread observes while the flush thread ticks: no count is
  # lost and no window is closed twice.
  def test_concurrent_observers_count_exactly
    d = RS::SpikeDetector.new
    t = 1_758_799_981_000
    ticker = Thread.new do
      2000.times do
        assert_empty d.tick(t), 'a tick inside the window closed it'
        Thread.pass
      end
    end
    workers = 8.times.map do
      Thread.new { 20_000.times { |n| d.observe(t, 'GET', "/r#{n % 64}", 429) } }
    end
    workers.each(&:join)
    ticker.join

    spikes = d.finish
    assert_equal 64, spikes.length
    assert_equal 160_000, spikes.sum(&:total)
    assert_equal 160_000, spikes.sum(&:count)
  end

  def test_invalid_utf8_and_non_strings_do_not_raise
    assert_equal 'OTHER', RS.normalize_method(nil)
    assert_equal '*', RS.normalize_route(nil)
    refute_nil RS.normalize_route("/bad\xff".b)
  end

  # A forked child starts with an empty window: the parent reports its own.
  def test_a_forked_child_does_not_report_the_parents_window
    skip 'fork is not available on this platform' unless Process.respond_to?(:fork)

    d = RS::SpikeDetector.new
    10.times { d.observe(1_758_799_981_000, 'GET', '/a', 429) }
    reader, writer = IO.pipe
    pid = fork do
      reader.close
      writer.write(d.finish.length.to_s)
      writer.close
      exit!(0)
    end
    writer.close
    child = reader.read
    Process.wait(pid)
    assert_equal '0', child
    assert_equal 1, d.finish.length, 'the parent still has its window'
  end
end
