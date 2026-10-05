// Real-browser LCP and Event Timing are NOT exercised here: Node has no
// renderer, so entries come from the platform stand-in in helpers/perf.ts,
// shaped as the browser's. What these tests prove is the sensor's handling of
// those entries; that a browser emits them as expected needs a browser.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { Sensor, type SensorOptions } from '../src/sensor.js';
import { compute } from '../src/fingerprint.js';
import { isDisabled, resetForTests } from '../src/safe.js';
import { installVitalsCapture, routeTemplate, type PerfObserverCtor } from '../src/capture/vitals.js';
import { installErrorCapture } from '../src/capture/errors.js';
import { startTestServer, type TestServer } from './helpers/server.js';
import { asTarget, eventEntry, hide, lcpEntry, perfPlatform, testPage } from './helpers/perf.js';
import type { FlushRequest } from '../src/transport.js';
import type { Count } from '../src/counters.js';
import type { CapturedSignal } from '../src/types.js';

function sensorFor(server: TestServer, extra: Partial<SensorOptions> = {}): Sensor {
  resetForTests();
  return new Sensor({
    endpoint: `${server.url}/v1/flush`, tenant: 'acme', ingestKey: 'adt_client_test',
    release: 'r1', sensorId: 's1', autoFlush: false, ...extra,
  });
}

async function flushed(s: Sensor, server: TestServer): Promise<Count[]> {
  const before = server.received.length;
  await s.flush();
  return server.received
    .slice(before)
    .filter((r) => r.url === '/v1/flush')
    .flatMap((r) => (JSON.parse(r.body) as FlushRequest).counts);
}

function fp(type: string, message: string): string {
  return compute({ kind: 'degradation', source: 'client', type, message }).fp;
}

function signals(s: Sensor): Array<[string, string, string]> {
  return (s.evidenceBundle('x').signals as CapturedSignal[]).map((x) => [x.kind, x.type, x.message]);
}

async function withServer(t: { after(fn: () => unknown): void }): Promise<TestServer> {
  const server = await startTestServer();
  t.after(() => server.close());
  return server;
}

// ---------------------------------------------------------------- route templates

test('routeTemplate: path and AngularJS hash routes, ids templated, anchors dropped', () => {
  assert.equal(routeTemplate({ pathname: '/deals/123', hash: '' }), '/deals/<id>');
  assert.equal(routeTemplate({ pathname: '/', hash: '#!/deals/123/edit?tab=2' }), '/#!/deals/<id>/edit');
  assert.equal(routeTemplate({ pathname: '/app/', hash: '#/customers/9f86d081-8a2b-4c6e-9d3f-0123456789ab' }), '/app/#/customers/<uuid>');
  assert.equal(routeTemplate({ pathname: '/help', hash: '#section-2' }), '/help');
  assert.equal(routeTemplate(undefined), '/');
});

// ---------------------------------------------------------------- slow page load

test('LCP: a poor LCP is one slow_page_load, judged at first input, on the templated hash route', async (t) => {
  const server = await withServer(t);
  const s = sensorFor(server);
  const perf = perfPlatform();
  const page = testPage(perf, '/', '#!/deals/123');
  t.after(installVitalsCapture(s, asTarget(page)));

  perf.emit(lcpEntry(1200));
  perf.emit(lcpEntry(4800)); // the final candidate is the LCP
  assert.equal(s.stats().signals, 0, 'LCP must not be judged before it is final');

  page.dispatchEvent(new Event('keydown'));
  assert.deepEqual(signals(s), [['degradation', 'slow_page_load', 'page /#!/deals/<id> loaded slowly']]);

  // Final means final: later candidates, input and hiding change nothing.
  perf.emit(lcpEntry(9000));
  page.dispatchEvent(new Event('click'));
  hide(page);
  assert.equal(perf.connected('largest-contentful-paint').length, 0, 'the LCP observer must stop after judging');

  const counts = await flushed(s, server);
  assert.deepEqual(counts.map((c) => [c.fp, c.n, c.kind]), [
    [fp('slow_page_load', 'page /#!/deals/<id> loaded slowly'), 1, 'degradation'],
  ]);
});

test('LCP: a good page load records nothing', async (t) => {
  const server = await withServer(t);
  const s = sensorFor(server);
  const perf = perfPlatform();
  const page = testPage(perf, '/deals/1');
  t.after(installVitalsCapture(s, asTarget(page)));

  perf.emit(lcpEntry(900), lcpEntry(3999));
  page.dispatchEvent(new Event('click'));
  assert.equal(s.stats().signals, 0);
});

test('LCP: judged when the page is hidden, including a candidate not yet delivered', async (t) => {
  const server = await withServer(t);
  const s = sensorFor(server);
  const perf = perfPlatform();
  const page = testPage(perf, '/reports/77');
  t.after(installVitalsCapture(s, asTarget(page)));

  perf.emit(lcpEntry(1500));
  perf.queue(lcpEntry(6100)); // queued by the browser, observer task not yet run
  hide(page);
  assert.deepEqual(signals(s), [['degradation', 'slow_page_load', 'page /reports/<id> loaded slowly']]);
});

test('LCP: two loads of the same route with different ids are one fingerprint', async (t) => {
  const server = await withServer(t);
  const s = sensorFor(server);
  for (const hash of ['#!/deals/123', '#!/deals/456']) {
    const perf = perfPlatform();
    const page = testPage(perf, '/', hash);
    t.after(installVitalsCapture(s, asTarget(page)));
    perf.emit(lcpEntry(5000));
    page.dispatchEvent(new Event('keydown'));
  }
  const counts = await flushed(s, server);
  assert.deepEqual(counts.map((c) => c.n), [2]);
});

test('LCP: a page loaded in a background tab is not judged', async (t) => {
  const server = await withServer(t);
  const s = sensorFor(server);
  const perf = perfPlatform();
  const page = testPage(perf, '/deals/1');
  page.document.visibilityState = 'hidden';
  t.after(installVitalsCapture(s, asTarget(page)));

  perf.emit(lcpEntry(30_000));
  page.document.visibilityState = 'visible';
  page.dispatchEvent(new Event('keydown'));
  assert.equal(s.stats().signals, 0);
});

// The case that matters most and is easiest to lose: a user waits for a slow
// page and leaves without touching it. The sensor's hide-time flush must carry
// the slow load, so it has to be judged before that flush, not after.
test('LCP: a slow load judged at hide leaves in the sensor\'s hide-time flush', async (t) => {
  const server = await withServer(t);
  const perf = perfPlatform();
  const page = testPage(perf, '/', '#!/inventory');

  const g = globalThis as Record<string, unknown>;
  g.document = page.document;
  let s: Sensor;
  try {
    s = sensorFor(server, { autoFlush: true, flushIntervalMs: 60_000 });
  } finally {
    delete g.document;
  }
  t.after(() => s.stop());
  t.after(installVitalsCapture(s, asTarget(page)));

  perf.emit(lcpEntry(7000));
  hide(page);

  const deadline = Date.now() + 2000;
  while (server.received.length === 0 && Date.now() < deadline) await new Promise((r) => setTimeout(r, 5));
  assert.equal(server.received.length, 1, 'the hide-time flush did not carry the slow page load');
  const body = JSON.parse(server.received[0]!.body) as FlushRequest;
  assert.deepEqual(body.counts.map((c) => c.fp), [fp('slow_page_load', 'page /#!/inventory loaded slowly')]);
});

// ---------------------------------------------------------------- slow interactions

test('interactions: one record per slow interaction, none for fast or non-interaction entries', async (t) => {
  const server = await withServer(t);
  const s = sensorFor(server);
  const perf = perfPlatform();
  const page = testPage(perf, '/', '#!/reports/2024');
  t.after(installVitalsCapture(s, asTarget(page)));

  const [obs] = perf.connected('event');
  assert.deepEqual(obs!.options, { type: 'event', buffered: true, durationThreshold: 500 });

  perf.emit(
    // One click: three entries, one interaction.
    eventEntry('pointerdown', 520, 7), eventEntry('pointerup', 520, 7), eventEntry('click', 560, 7),
    // Another slow interaction.
    eventEntry('keydown', 600, 8),
    // Slow, but not an interaction (no interactionId): not something a user did.
    eventEntry('mouseover', 900, 0),
  );
  perf.emit(eventEntry('click', 200, 9)); // fast: filtered before delivery
  perf.emit(eventEntry('click', 560, 7)); // a late entry of interaction 7

  assert.deepEqual(signals(s), [
    ['degradation', 'slow_interaction', 'interaction on /#!/reports/<id> was slow'],
    ['degradation', 'slow_interaction', 'interaction on /#!/reports/<id> was slow'],
  ]);
  const counts = await flushed(s, server);
  assert.deepEqual(counts.map((c) => [c.fp, c.n]), [
    [fp('slow_interaction', 'interaction on /#!/reports/<id> was slow'), 2],
  ]);
});

test('interactions: the route is read when the interaction is reported', async (t) => {
  const server = await withServer(t);
  const s = sensorFor(server);
  const perf = perfPlatform();
  const page = testPage(perf, '/', '#!/deals');
  t.after(installVitalsCapture(s, asTarget(page)));

  perf.emit(eventEntry('click', 640, 1));
  page.location.hash = '#!/deals/55/edit';
  perf.emit(eventEntry('click', 640, 2));

  assert.deepEqual(signals(s).map((x) => x[2]), [
    'interaction on /#!/deals was slow',
    'interaction on /#!/deals/<id>/edit was slow',
  ]);
});

// ---------------------------------------------------------------- options

test('thresholds: defaults 4000 / 500, custom values apply, 0 disables each', async (t) => {
  const server = await withServer(t);
  const d = sensorFor(server);
  assert.equal(d.slow.pageLoad, 4000);
  assert.equal(d.slow.interaction, 500);
  assert.equal(sensorFor(server, { slowPageLoadMs: -1, slowInteractionMs: Infinity }).slow.pageLoad, 4000);
  assert.equal(sensorFor(server, { slowInteractionMs: Infinity }).slow.interaction, 500);

  // Custom: a 2.5s LCP is slow at 2000, a 300ms interaction is slow at 250.
  const s = sensorFor(server, { slowPageLoadMs: 2000, slowInteractionMs: 250 });
  const perf = perfPlatform();
  const page = testPage(perf, '/x');
  t.after(installVitalsCapture(s, asTarget(page)));
  assert.equal(perf.connected('event')[0]!.options!.durationThreshold, 250);
  perf.emit(lcpEntry(2500), eventEntry('click', 300, 1));
  page.dispatchEvent(new Event('click'));
  assert.deepEqual(signals(s).map((x) => x[1]).sort(), ['slow_interaction', 'slow_page_load']);

  // Disabled: nothing observed at all.
  const off = sensorFor(server, { slowPageLoadMs: 0, slowInteractionMs: 0 });
  const perfOff = perfPlatform();
  const pageOff = testPage(perfOff, '/x');
  t.after(installVitalsCapture(off, asTarget(pageOff)));
  assert.equal(perfOff.observers.length, 0);
  perfOff.emit(lcpEntry(60_000), eventEntry('click', 5000, 1));
  pageOff.dispatchEvent(new Event('click'));
  assert.equal(off.stats().signals, 0);

  // The Event Timing minimum is 16ms; a lower setting must not be passed on.
  const low = sensorFor(server, { slowInteractionMs: 5 });
  const perfLow = perfPlatform();
  t.after(installVitalsCapture(low, asTarget(testPage(perfLow))));
  assert.equal(perfLow.connected('event')[0]!.options!.durationThreshold, 16);
});

// ---------------------------------------------------------------- feature detection

test('unsupported platforms: silently nothing, and the sensor stays enabled', async (t) => {
  const server = await withServer(t);
  const s = sensorFor(server);

  // No PerformanceObserver at all.
  const bare = testPage(null);
  assert.doesNotThrow(() => installVitalsCapture(s, asTarget(bare))());

  // Node's real PerformanceObserver: it exists, but has neither entry type.
  const nodePO = globalThis.PerformanceObserver as unknown as PerfObserverCtor;
  assert.ok(nodePO.supportedEntryTypes, 'precondition: Node exposes supportedEntryTypes');
  assert.equal(nodePO.supportedEntryTypes!.indexOf('largest-contentful-paint'), -1);
  const nodePage = Object.assign(testPage(null), { PerformanceObserver: nodePO });
  t.after(installVitalsCapture(s, asTarget(nodePage)));
  nodePage.dispatchEvent(new Event('keydown'));

  // An older engine: the type is unknown to it.
  const perfOld = perfPlatform(['mark', 'measure', 'resource']);
  t.after(installVitalsCapture(s, asTarget(testPage(perfOld))));
  assert.equal(perfOld.observers.length, 0);

  // No supportedEntryTypes (pre-2019 engines): not trusted with observe({type}).
  const NoList = function () { /* platform without the static list */ } as unknown;
  t.after(installVitalsCapture(s, asTarget(Object.assign(testPage(null), { PerformanceObserver: NoList }))));

  // An engine that lists the type but rejects the options.
  class Rejecting {
    static supportedEntryTypes = ['largest-contentful-paint', 'event'];
    observe(): void { throw new TypeError('Failed to execute observe: invalid option'); }
    disconnect(): void {}
  }
  t.after(installVitalsCapture(s, asTarget(Object.assign(testPage(null), { PerformanceObserver: Rejecting }))));

  assert.equal(isDisabled(), false, 'feature detection must not shut the sensor down');
  assert.equal(s.stats().signals, 0);
  s.record('error', 'TypeError', 'still recording');
  assert.equal(s.stats().signals, 1);
});

test('a malformed entry list never throws into the page', async (t) => {
  const server = await withServer(t);
  const s = sensorFor(server);
  let callback: ((list: { getEntries(): unknown[] }) => void) | null = null;
  class Hostile {
    static supportedEntryTypes = ['event'];
    constructor(cb: (list: { getEntries(): unknown[] }) => void) { callback = cb; }
    observe(): void {}
    disconnect(): void {}
  }
  t.after(installVitalsCapture(s, asTarget(Object.assign(testPage(null), { PerformanceObserver: Hostile }))));
  assert.ok(callback);
  assert.doesNotThrow(() => callback!({ getEntries: () => { throw new Error('engine bug'); } }));
  assert.doesNotThrow(() => callback!({ getEntries: () => [null] }));
  resetForTests();
});

// ---------------------------------------------------------------- install / teardown

test('installErrorCapture installs vitals capture, and its teardown removes it', async (t) => {
  const server = await withServer(t);
  const s = sensorFor(server);
  const perf = perfPlatform();
  const page = testPage(perf, '/deals/9');

  const uninstall = installErrorCapture(s, asTarget(page));
  assert.equal(perf.connected('largest-contentful-paint').length, 1, 'LCP observer not installed');
  assert.equal(perf.connected('event').length, 1, 'Event Timing observer not installed');
  perf.emit(lcpEntry(8000)); // a slow candidate, not yet judged

  uninstall();
  assert.equal(perf.connected('largest-contentful-paint').length + perf.connected('event').length, 0);
  perf.emit(lcpEntry(9000), eventEntry('click', 900, 3));
  page.dispatchEvent(new Event('keydown'));
  hide(page);
  assert.equal(s.stats().signals, 0);
});

test('installErrorCapture records a slow interaction end to end', async (t) => {
  const server = await withServer(t);
  const s = sensorFor(server);
  const perf = perfPlatform();
  const page = testPage(perf, '/', '#!/deals/12');
  t.after(installErrorCapture(s, asTarget(page)));

  perf.emit(eventEntry('click', 720, 4));
  const counts = await flushed(s, server);
  assert.deepEqual(counts.map((c) => c.fp), [fp('slow_interaction', 'interaction on /#!/deals/<id> was slow')]);
});
