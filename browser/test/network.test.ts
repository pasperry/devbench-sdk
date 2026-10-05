import { test } from 'node:test';
import assert from 'node:assert/strict';
import { Sensor } from '../src/sensor.js';
import { compute } from '../src/fingerprint.js';
import { isDisabled, resetForTests } from '../src/safe.js';
import { installNetworkCapture, type NetworkCaptureTargets } from '../src/capture/network.js';
import { installErrorCapture } from '../src/capture/errors.js';
import { registerAngularModule, type AngularStatic } from '../src/adapters/angularjs.js';
import { startTestServer, type TestServer } from './helpers/server.js';
import { xhrClass, type TestXhr } from './helpers/xhr.js';
import type { FlushRequest } from '../src/transport.js';
import type { Count } from '../src/counters.js';

const settle = () => new Promise((r) => setTimeout(r, 20));

function sensorFor(server: TestServer, extra: Partial<ConstructorParameters<typeof Sensor>[0]> = {}): Sensor {
  resetForTests();
  return new Sensor({
    endpoint: `${server.url}/v1/flush`, tenant: 'acme', ingestKey: 'adt_client_test',
    release: 'r1', sensorId: 's1', autoFlush: false, ...extra,
  });
}

/** Flushes and returns the counts that reached ingest. */
async function flushed(s: Sensor, server: TestServer): Promise<Count[]> {
  const before = server.received.length;
  await s.flush();
  return server.received
    .slice(before)
    .filter((r) => r.url === '/v1/flush')
    .flatMap((r) => (JSON.parse(r.body) as FlushRequest).counts);
}

function fp(kind: 'error' | 'silent_failure', type: string, message: string): string {
  return compute({ kind, source: 'client', type, message }).fp;
}

/** A page's fetch: the platform's, recording what it was called with and returned. */
function pageFetch() {
  const calls: Array<{ self: unknown; args: unknown[]; returned: unknown }> = [];
  const fn = function (this: unknown, ...args: Parameters<typeof fetch>) {
    const returned = globalThis.fetch(...args);
    calls.push({ self: this, args, returned });
    return returned;
  } as typeof fetch;
  return { calls, fn };
}

function xhrRequest(Xhr: { new (): TestXhr }, method: string, url: string): Promise<TestXhr> {
  return new Promise((resolve) => {
    const xhr = new Xhr();
    xhr.open(method, url);
    xhr.addEventListener('loadend', () => resolve(xhr));
    xhr.send();
  });
}

// ---------------------------------------------------------------- fetch

test('fetch: a 4xx/5xx is recorded with the same fingerprint the AngularJS path uses', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());
  server.handle.set('/api/customers/9912', () => ({ status: 404, body: '{}' }));

  const s = sensorFor(server);
  const target: NetworkCaptureTargets = { fetch: pageFetch().fn };
  t.after(installNetworkCapture(s, target));

  const res = await target.fetch!(`${server.url}/api/customers/9912?token=abc`);
  assert.equal(res.status, 404, 'the app must see the real response');
  await settle();

  const counts = await flushed(s, server);
  assert.equal(counts.length, 1);
  assert.equal(counts[0]!.fp, fp('error', 'http_404', 'GET /api/customers/<id> failed with 404'));
});

test('fetch: a network failure is recorded, and the app still gets the same rejection', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const s = sensorFor(server);
  const page = pageFetch();
  const target: NetworkCaptureTargets = { fetch: page.fn };
  t.after(installNetworkCapture(s, target));

  // Nothing listens on port 9.
  const p = target.fetch!('http://127.0.0.1:9/api/orders', { method: 'post' });
  assert.equal(p, page.calls[0]!.returned, 'the app must get the original promise, not a wrapper');
  await assert.rejects(p, TypeError);
  await settle();

  const counts = await flushed(s, server);
  assert.equal(counts.length, 1);
  assert.equal(counts[0]!.fp, fp('error', 'network_error', 'POST /api/orders failed with network error'));
});

test('fetch: an abort the app asked for is not a failure', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const s = sensorFor(server);
  const target: NetworkCaptureTargets = { fetch: pageFetch().fn };
  t.after(installNetworkCapture(s, target));

  const ctl = new AbortController();
  const p = target.fetch!(`${server.url}/api/slow`, { signal: ctl.signal });
  ctl.abort();
  await assert.rejects(p);
  await settle();

  assert.equal(s.stats().distinctFingerprints, 0);
});

test('fetch: x-adt-handled on a success is a silent failure; on an error it is not', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());
  server.handle.set('/api/profile', (req) => ({
    status: req.method === 'PUT' ? 200 : 422, body: '{}', headers: { 'x-adt-handled': '1' },
  }));

  const s = sensorFor(server);
  const target: NetworkCaptureTargets = { fetch: pageFetch().fn };
  t.after(installNetworkCapture(s, target));

  await target.fetch!(`${server.url}/api/profile`, { method: 'PUT' });
  await target.fetch!(`${server.url}/api/ok`); // plain 200, no header
  await target.fetch!(`${server.url}/api/profile`, { method: 'POST' });
  await settle();

  const counts = await flushed(s, server);
  const fps = counts.map((c) => c.fp).sort();
  assert.deepEqual(fps, [
    fp('silent_failure', 'handled_without_feedback', 'PUT /api/profile returned 200 after the server handled a failure'),
    fp('error', 'http_422', 'POST /api/profile failed with 422'),
  ].sort());
});

test('fetch: wrapped, never replaced — same this, same arguments, same throw', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const s = sensorFor(server);
  const page = pageFetch();
  const target: NetworkCaptureTargets = { fetch: page.fn };
  t.after(installNetworkCapture(s, target));

  const init = { method: 'GET', headers: { 'x-app': '1' } };
  await target.fetch!(`${server.url}/api/a`, init);
  assert.equal(page.calls[0]!.self, target, 'this was not forwarded');
  assert.equal(page.calls[0]!.args.length, 2);
  assert.equal(page.calls[0]!.args[1], init, 'init was not forwarded as-is');

  const boom = new Error('sync throw from the platform');
  const throwing: NetworkCaptureTargets = { fetch: (() => { throw boom; }) as typeof fetch };
  t.after(installNetworkCapture(s, throwing));
  assert.throws(() => throwing.fetch!('/x'), (e) => e === boom);
});

test('fetch: a broken sensor never breaks the app\'s request', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());
  server.handle.set('/api/x', () => ({ status: 500, body: '"payload"' }));

  resetForTests();
  const s = new Sensor({
    endpoint: `${server.url}/v1/flush`, tenant: 'acme', ingestKey: 'k', autoFlush: false,
    now: () => { throw new Error('clock exploded'); },
  });
  const target: NetworkCaptureTargets = { fetch: pageFetch().fn };
  t.after(installNetworkCapture(s, target));

  const res = await target.fetch!(`${server.url}/api/x`);
  assert.equal(await res.text(), '"payload"');
  await settle();
  assert.equal(isDisabled(), true, 'the internal failure should have disabled the sensor');
  resetForTests();
});

// The loop: ingest fails -> a failure is recorded -> it is flushed -> ingest
// fails -> ... for the life of the page. In a browser the transport's fetch
// *is* the page's fetch, so the capture sees every flush.
test('the sensor\'s own ingest and evidence requests are never recorded', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const target: NetworkCaptureTargets = { fetch: pageFetch().fn };
  const s = sensorFor(server, { fetchImpl: (...a: Parameters<typeof fetch>) => target.fetch!(...a) });
  t.after(installNetworkCapture(s, target));

  // Ingest answers 500; then asks for evidence at a URL that answers 403.
  let ingestCalls = 0;
  server.handle.set('/v1/flush', () => {
    ingestCalls++;
    if (ingestCalls <= 2) return { status: 500, body: '{}' };
    return {
      status: 200,
      body: JSON.stringify({ need_evidence: [{ fp: 'x', claim: 'c', url: `${server.url}/bucket/obj?X-Sig=abc`, expires: 4_102_444_800 }] }),
    };
  });
  server.handle.set('/bucket/obj', () => ({ status: 403, body: '' }));

  s.record('error', 'TypeError', 'boom');
  await s.flush(); // 500, retried once: two ingest calls
  await settle();
  assert.equal(ingestCalls, 2);
  assert.equal(s.stats().distinctFingerprints, 0, 'a failed flush was recorded as an app failure');

  s.record('error', 'TypeError', 'boom');
  await s.flush(); // 200 + evidence request -> PUT -> 403
  await settle();
  assert.ok(server.received.some((r) => r.method === 'PUT' && r.url.startsWith('/bucket/obj')), 'evidence was not uploaded');
  assert.equal(s.stats().distinctFingerprints, 0, 'a failed evidence upload was recorded as an app failure');

  await s.flush();
  assert.equal(ingestCalls, 3, 'the sensor flushed its own failures');

  // Identity is origin + path: a query string does not make it someone else's.
  server.handle.set('/v1/flush', () => ({ status: 500, body: '{}' }));
  await target.fetch!(`${server.url}/v1/flush?attempt=2`, { method: 'POST' });
  await settle();
  assert.equal(s.stats().distinctFingerprints, 0);

  // XHR to the endpoint is ours too.
  const Xhr = xhrClass();
  t.after(installNetworkCapture(s, { XMLHttpRequest: Xhr }));
  await xhrRequest(Xhr, 'POST', `${server.url}/v1/flush`);
  await settle();
  assert.equal(s.stats().distinctFingerprints, 0);
});

// ---------------------------------------------------------------- XHR

test('xhr: a 5xx is recorded and the app still sees it', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());
  server.handle.set('/api/invoices', () => ({ status: 500, body: '{}' }));

  const s = sensorFor(server);
  const Xhr = xhrClass();
  t.after(installNetworkCapture(s, { XMLHttpRequest: Xhr }));

  let appSaw = 0;
  const xhr = new Xhr();
  const done = new Promise<void>((r) => xhr.addEventListener('loadend', () => r()));
  xhr.onload = () => { appSaw = xhr.status; };
  xhr.open('post', `${server.url}/api/invoices`);
  xhr.send('{}');
  await done;
  await settle();

  assert.equal(appSaw, 500);
  assert.equal(xhr.openArgCount, 2, 'open() must receive exactly the arguments it was given');
  const counts = await flushed(s, server);
  assert.equal(counts.length, 1);
  assert.equal(counts[0]!.fp, fp('error', 'http_500', 'POST /api/invoices failed with 500'));
});

test('xhr: a network failure is recorded', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const s = sensorFor(server);
  const Xhr = xhrClass();
  t.after(installNetworkCapture(s, { XMLHttpRequest: Xhr }));

  await xhrRequest(Xhr, 'GET', 'http://127.0.0.1:9/api/customers/7');
  await settle();

  const counts = await flushed(s, server);
  assert.equal(counts.length, 1);
  assert.equal(counts[0]!.fp, fp('error', 'network_error', 'GET /api/customers/<id> failed with network error'));
});

test('xhr: an abort the app asked for is not a failure', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const s = sensorFor(server);
  const Xhr = xhrClass();
  t.after(installNetworkCapture(s, { XMLHttpRequest: Xhr }));

  const xhr = new Xhr();
  xhr.open('GET', `${server.url}/api/slow`);
  xhr.send();
  xhr.abort();
  await settle();

  assert.equal(s.stats().distinctFingerprints, 0);
});

test('xhr: x-adt-handled on a success is a silent failure', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());
  server.handle.set('/api/profile', () => ({ status: 200, body: '{}', headers: { 'X-ADT-Handled': '2' } }));

  const s = sensorFor(server);
  const Xhr = xhrClass();
  t.after(installNetworkCapture(s, { XMLHttpRequest: Xhr }));

  await xhrRequest(Xhr, 'PUT', `${server.url}/api/profile`);
  await xhrRequest(Xhr, 'GET', `${server.url}/api/ok`);
  await settle();

  const counts = await flushed(s, server);
  assert.deepEqual(counts.map((c) => c.fp), [
    fp('silent_failure', 'handled_without_feedback', 'PUT /api/profile returned 200 after the server handled a failure'),
  ]);
});

test('teardown restores fetch and the XHR prototype, and stops recording', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());
  server.handle.set('/api/x', () => ({ status: 500, body: '{}' }));

  const s = sensorFor(server);
  const page = pageFetch();
  const Xhr = xhrClass();
  const { open, send, abort } = Xhr.prototype;
  const target: NetworkCaptureTargets = { fetch: page.fn, XMLHttpRequest: Xhr };

  const uninstall = installErrorCapture(s, target);
  assert.notEqual(target.fetch, page.fn, 'installErrorCapture did not install fetch capture');
  assert.notEqual(Xhr.prototype.open, open, 'installErrorCapture did not install XHR capture');
  assert.equal(s.capturesNetwork(), true);

  uninstall();
  assert.equal(target.fetch, page.fn);
  assert.equal(Xhr.prototype.open, open);
  assert.equal(Xhr.prototype.send, send);
  assert.equal(Xhr.prototype.abort, abort);
  assert.equal(s.capturesNetwork(), false);

  await target.fetch!(`${server.url}/api/x`);
  await xhrRequest(Xhr, 'GET', `${server.url}/api/x`);
  await settle();
  assert.equal(s.stats().distinctFingerprints, 0);
});

// The documented install passes `window`; this is a compile-time check that it
// still type-checks with the network targets added.
export function documentedInstallTypeChecks(s: Sensor, w: Window): () => void {
  return installErrorCapture(s, w);
}

// ---------------------------------------------------------------- AngularJS

/** Registers the real adapter and returns its interceptor. */
function angularInterceptor(s: Sensor) {
  const captured: { factory?: (q: unknown) => any } = {};
  const angular: AngularStatic = {
    module() {
      const mod: any = {
        factory(name: string, fn: unknown[]) {
          if (name === 'adtHttpInterceptor') captured.factory = fn[fn.length - 1] as (q: unknown) => any;
          return mod;
        },
        config() { return mod; },
        decorator() { return mod; },
        run() { return mod; },
      };
      return mod;
    },
  };
  registerAngularModule(angular, s);
  return captured.factory!({ reject: (v: unknown) => Promise.reject(v) });
}

/**
 * $http's shape over XMLHttpRequest: interceptors around $httpBackend, which
 * creates an XHR per request and completes on onload/onerror — before the
 * XHR's own loadend, as in a browser.
 */
function $http(interceptor: any, Xhr: { new (): TestXhr }, config: { method: string; url: string; data?: unknown }) {
  return new Promise((resolve, reject) => {
    const cfg = interceptor.request({ ...config });
    const xhr = new Xhr();
    xhr.open(cfg.method, cfg.url, true);
    xhr.onload = () => {
      const res = {
        config: cfg, status: xhr.status,
        data: xhr.responseText ? JSON.parse(xhr.responseText) : undefined,
        headers: (name: string) => xhr.getResponseHeader(name),
      };
      if (xhr.status >= 200 && xhr.status < 300) resolve(interceptor.response(res));
      else interceptor.responseError(res).then(resolve, reject);
    };
    xhr.onerror = () => interceptor.responseError({ config: cfg, status: -1 }).then(resolve, reject);
    xhr.send(cfg.data ? JSON.stringify(cfg.data) : undefined);
  });
}

test('angular + global capture: one failed $http request is exactly one count', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());
  server.handle.set('/api/invoices', () => ({ status: 500, body: '{}' }));

  const s = sensorFor(server);
  const Xhr = xhrClass();
  t.after(installNetworkCapture(s, { XMLHttpRequest: Xhr }));
  const interceptor = angularInterceptor(s);

  await assert.rejects($http(interceptor, Xhr, { method: 'POST', url: `${server.url}/api/invoices` }));
  await settle();

  const counts = await flushed(s, server);
  assert.equal(counts.length, 1, JSON.stringify(counts));
  assert.equal(counts[0]!.n, 1, 'one failure was counted twice');
  assert.equal(counts[0]!.fp, fp('error', 'http_500', 'POST /api/invoices failed with 500'));
});

test('angular + global capture: one handled-header response is exactly one silent failure', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());
  server.handle.set('/api/profile', () => ({ status: 200, body: '{}', headers: { 'x-adt-handled': '1' } }));

  const s = sensorFor(server);
  const Xhr = xhrClass();
  t.after(installNetworkCapture(s, { XMLHttpRequest: Xhr }));
  const interceptor = angularInterceptor(s);

  await $http(interceptor, Xhr, { method: 'PUT', url: `${server.url}/api/profile`, data: { name: 'x' } });
  await settle();

  const counts = await flushed(s, server);
  assert.equal(counts.length, 1);
  assert.equal(counts[0]!.n, 1);
  assert.equal(counts[0]!.kind, 'silent_failure');
});

test('angular without global capture still records the failure itself', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());
  server.handle.set('/api/invoices', () => ({ status: 500, body: '{}' }));

  const s = sensorFor(server);
  const interceptor = angularInterceptor(s);

  await assert.rejects($http(interceptor, xhrClass(), { method: 'POST', url: `${server.url}/api/invoices` }));
  await settle();

  const counts = await flushed(s, server);
  assert.equal(counts.length, 1);
  assert.equal(counts[0]!.n, 1);
});

test('angular + global capture: readback still runs through $http', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());
  server.handle.set('/api/customers/9912', (req) => ({
    status: 200, body: req.method === 'PUT' ? '{}' : JSON.stringify({ email: 'old@example.com' }),
  }));

  const s = sensorFor(server);
  const Xhr = xhrClass();
  t.after(installNetworkCapture(s, { XMLHttpRequest: Xhr }));
  const interceptor = angularInterceptor(s);

  await $http(interceptor, Xhr, { method: 'PUT', url: `${server.url}/api/customers/9912`, data: { email: 'new@example.com' } });
  await $http(interceptor, Xhr, { method: 'GET', url: `${server.url}/api/customers/9912` });
  await settle();

  const counts = await flushed(s, server);
  assert.deepEqual(counts.map((c) => c.fp), [
    fp('silent_failure', 'write_readback_divergence', 'GET /api/customers/<id> reported success but email did not change'),
  ]);
});
