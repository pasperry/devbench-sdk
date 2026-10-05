import { test } from 'node:test';
import assert from 'node:assert/strict';
import { Sensor, type SensorOptions } from '../src/sensor.js';
import { compute } from '../src/fingerprint.js';
import { resetForTests } from '../src/safe.js';
import { installNetworkCapture, type NetworkCaptureTargets } from '../src/capture/network.js';
import { startTestServer, type TestServer } from './helpers/server.js';
import { xhrClass, type TestXhr } from './helpers/xhr.js';
import type { FlushRequest } from '../src/transport.js';
import type { Count } from '../src/counters.js';
import type { CapturedSignal } from '../src/types.js';

// Real HTTP against a real server that really holds its answer. The threshold
// is lowered so the suite stays fast; the timing path is the production one.
const SLOW_MS = 120;
const DELAY_MS = 250;

const settle = () => new Promise((r) => setTimeout(r, 20));

function sensorFor(server: TestServer, extra: Partial<SensorOptions> = {}): Sensor {
  resetForTests();
  return new Sensor({
    endpoint: `${server.url}/v1/flush`, tenant: 'acme', ingestKey: 'adt_client_test',
    release: 'r1', sensorId: 's1', autoFlush: false, slowRequestMs: SLOW_MS, ...extra,
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

function fp(kind: 'error' | 'silent_failure' | 'degradation', type: string, message: string): string {
  return compute({ kind, source: 'client', type, message }).fp;
}

function xhrRequest(Xhr: { new (): TestXhr }, method: string, url: string): Promise<TestXhr> {
  return new Promise((resolve) => {
    const xhr = new Xhr();
    xhr.open(method, url);
    xhr.addEventListener('loadend', () => resolve(xhr));
    xhr.send();
  });
}

function slowServer(server: TestServer, path: string, status = 200, headers?: Record<string, string>): void {
  server.handle.set(path, () => ({ status, body: '{}', delayMs: DELAY_MS, headers }));
}

test('fetch: a slow request is one degradation per method + route template, with no duration in it', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());
  slowServer(server, '/api/deals/123');
  slowServer(server, '/api/deals/456');

  const s = sensorFor(server);
  const target: NetworkCaptureTargets = { fetch: globalThis.fetch.bind(globalThis) };
  t.after(installNetworkCapture(s, target));

  const res = await target.fetch!(`${server.url}/api/deals/123?x=1`);
  assert.equal(res.status, 200, 'the app must see the real response');
  await target.fetch!(`${server.url}/api/deals/456`);
  await target.fetch!(`${server.url}/api/fast`); // no delay: not slow
  await settle();

  const signals = s.evidenceBundle('x').signals as CapturedSignal[];
  assert.deepEqual(signals.map((x) => [x.kind, x.type, x.message]), [
    ['degradation', 'slow_request', 'GET /api/deals/<id> was slow'],
    ['degradation', 'slow_request', 'GET /api/deals/<id> was slow'],
  ]);

  const counts = await flushed(s, server);
  assert.equal(counts.length, 1, 'two slow deals requests must be one fingerprint');
  assert.equal(counts[0]!.n, 2);
  assert.equal(counts[0]!.kind, 'degradation');
  assert.equal(counts[0]!.fp, fp('degradation', 'slow_request', 'GET /api/deals/<id> was slow'));
});

test('xhr: a slow request is a degradation; a fast one is nothing', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());
  slowServer(server, '/api/inventory');

  const s = sensorFor(server);
  const Xhr = xhrClass();
  t.after(installNetworkCapture(s, { XMLHttpRequest: Xhr }));

  await xhrRequest(Xhr, 'post', `${server.url}/api/inventory`);
  await xhrRequest(Xhr, 'POST', `${server.url}/api/fast`);
  await settle();

  const counts = await flushed(s, server);
  assert.deepEqual(counts.map((c) => [c.fp, c.n]), [
    [fp('degradation', 'slow_request', 'POST /api/inventory was slow'), 1],
  ]);
});

test('xhr: duration is measured from send, not open', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const s = sensorFor(server);
  const Xhr = xhrClass();
  t.after(installNetworkCapture(s, { XMLHttpRequest: Xhr }));

  // Opened, then sent well after the threshold: the wait before send is the
  // app's, not the request's.
  const xhr = new Xhr();
  xhr.open('GET', `${server.url}/api/fast`);
  await new Promise((r) => setTimeout(r, DELAY_MS));
  await new Promise<void>((r) => { xhr.addEventListener('loadend', () => r(), { once: true }); xhr.send(); });
  await settle();
  assert.equal(s.stats().distinctFingerprints, 0, 'time between open() and send() was counted');
});

// The documented decision: one bad request is one problem.
test('a slow request that fails is recorded as the failure only', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());
  slowServer(server, '/api/invoices', 500);

  const s = sensorFor(server);
  const Xhr = xhrClass();
  const target: NetworkCaptureTargets = { fetch: globalThis.fetch.bind(globalThis), XMLHttpRequest: Xhr };
  t.after(installNetworkCapture(s, target));

  await target.fetch!(`${server.url}/api/invoices`, { method: 'POST' });
  await xhrRequest(Xhr, 'POST', `${server.url}/api/invoices`);
  await settle();

  const counts = await flushed(s, server);
  assert.deepEqual(counts.map((c) => [c.fp, c.n]), [
    [fp('error', 'http_500', 'POST /api/invoices failed with 500'), 2],
  ]);
});

test('a slow response carrying x-adt-handled is the silent failure only', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());
  slowServer(server, '/api/profile', 200, { 'x-adt-handled': '1' });

  const s = sensorFor(server);
  const target: NetworkCaptureTargets = { fetch: globalThis.fetch.bind(globalThis) };
  t.after(installNetworkCapture(s, target));

  await target.fetch!(`${server.url}/api/profile`, { method: 'PUT' });
  await settle();

  const counts = await flushed(s, server);
  assert.deepEqual(counts.map((c) => c.kind), ['silent_failure']);
});

test('slowRequestMs: 0 disables slow-request detection', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());
  slowServer(server, '/api/deals/1');

  const s = sensorFor(server, { slowRequestMs: 0 });
  const target: NetworkCaptureTargets = { fetch: globalThis.fetch.bind(globalThis) };
  t.after(installNetworkCapture(s, target));

  await target.fetch!(`${server.url}/api/deals/1`);
  await settle();
  assert.equal(s.stats().distinctFingerprints, 0);
});

test('slowRequestMs defaults to 3000, and a nonsense value falls back to it', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  assert.equal(sensorFor(server, { slowRequestMs: undefined }).slow.request, 3000);
  assert.equal(sensorFor(server, { slowRequestMs: -5 }).slow.request, 3000);
  assert.equal(sensorFor(server, { slowRequestMs: Number.NaN }).slow.request, 3000);
  assert.equal(sensorFor(server, { slowRequestMs: 750 }).slow.request, 750);

  // And at the default, a 250ms request is not slow.
  slowServer(server, '/api/deals/1');
  const s = sensorFor(server, { slowRequestMs: undefined });
  const target: NetworkCaptureTargets = { fetch: globalThis.fetch.bind(globalThis) };
  t.after(installNetworkCapture(s, target));
  await target.fetch!(`${server.url}/api/deals/1`);
  await settle();
  assert.equal(s.stats().distinctFingerprints, 0);
});

test('the sensor\'s own slow ingest is never recorded as a slow request', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());
  server.handle.set('/v1/flush', () => ({ status: 200, body: '{}', delayMs: DELAY_MS }));

  const Xhr = xhrClass();
  const target: NetworkCaptureTargets = { fetch: globalThis.fetch.bind(globalThis), XMLHttpRequest: Xhr };
  // The transport goes through the page's (wrapped) fetch, as in a browser.
  const s = sensorFor(server, { fetchImpl: (...a: Parameters<typeof fetch>) => target.fetch!(...a) });
  t.after(installNetworkCapture(s, target));

  s.record('error', 'TypeError', 'boom');
  await s.flush();
  await xhrRequest(Xhr, 'POST', `${server.url}/v1/flush`);
  await settle();
  assert.equal(s.stats().distinctFingerprints, 0, 'a slow flush was recorded as an app degradation');
});
