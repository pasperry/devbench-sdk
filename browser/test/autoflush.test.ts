import { test } from 'node:test';
import assert from 'node:assert/strict';
import { Sensor, type SensorOptions } from '../src/sensor.js';
import { isDisabled, resetForTests } from '../src/safe.js';
import { startTestServer } from './helpers/server.js';
import type { FlushRequest } from '../src/transport.js';

function sensorFor(url: string, extra: Partial<SensorOptions> = {}): Sensor {
  resetForTests();
  return new Sensor({ endpoint: `${url}/v1/flush`, tenant: 'acme', ingestKey: 'adt_client_test', release: 'r1', sensorId: 's1', ...extra });
}

async function waitFor(cond: () => boolean, ms: number): Promise<boolean> {
  const until = Date.now() + ms;
  while (Date.now() < until) {
    if (cond()) return true;
    await new Promise((r) => setTimeout(r, 5));
  }
  return cond();
}

const pause = (ms: number) => new Promise((r) => setTimeout(r, ms));

/** Records every call the transport makes, synchronously, and answers 200 {}. */
function recordingFetch() {
  const calls: Array<{ url: string; init: RequestInit | undefined }> = [];
  const fetchImpl = ((url: string, init?: RequestInit) => {
    calls.push({ url: String(url), init });
    return Promise.resolve(new Response('{}', { status: 200 }));
  }) as typeof fetch;
  return { calls, fetchImpl };
}

/**
 * Installs a browser-shaped `document` and window-level event target on
 * globalThis for the duration of `fn`. The sensor reads these at construction,
 * exactly as it reads them in a page.
 */
async function withPage(fn: (page: { doc: EventTarget & { visibilityState: string }; win: EventTarget }) => Promise<void> | void) {
  const g = globalThis as Record<string, unknown>;
  const doc = Object.assign(new EventTarget(), { visibilityState: 'visible' });
  const win = new EventTarget();
  g.document = doc;
  g.addEventListener = win.addEventListener.bind(win);
  g.removeEventListener = win.removeEventListener.bind(win);
  try {
    await fn({ doc, win });
  } finally {
    delete g.document;
    delete g.addEventListener;
    delete g.removeEventListener;
  }
}

// The bug a real install hit: nobody wrote the setInterval, so nothing was ever sent.
test('the sensor flushes on its own, with no integrator timer', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const s = sensorFor(server.url, { flushIntervalMs: 20 });
  t.after(() => s.stop());
  s.record('error', 'TypeError', 'boom');

  assert.ok(await waitFor(() => server.received.length >= 1, 2000), 'no flush arrived without a manual flush() call');
  const body = JSON.parse(server.received[0]!.body) as FlushRequest;
  assert.equal(body.counts[0]!.n, 1);
});

test('a quiet page sends nothing however often the loop ticks', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const s = sensorFor(server.url, { flushIntervalMs: 5 });
  t.after(() => s.stop());
  await pause(100);
  assert.equal(server.received.length, 0);
});

test('stop() ends the loop', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const s = sensorFor(server.url, { flushIntervalMs: 10 });
  s.stop();
  s.record('error', 'TypeError', 'boom');
  await pause(120);
  assert.equal(server.received.length, 0, 'a stopped sensor flushed anyway');
});

test('autoFlush: false leaves flushing to the caller', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const s = sensorFor(server.url, { autoFlush: false, flushIntervalMs: 10 });
  s.record('error', 'TypeError', 'boom');
  await pause(120);
  assert.equal(server.received.length, 0);
  await s.flush();
  assert.equal(server.received.length, 1);
});

// A sensor's timer must never be the thing keeping a process alive: in Node
// that is a test runner that never exits, and the loop is not ours to hold open.
test('the flush timer does not keep the process alive', () => {
  const live = () => process.getActiveResourcesInfo().filter((r) => r === 'Timeout').length;
  const before = live();
  const s = sensorFor('http://127.0.0.1:9');
  assert.equal(live(), before, 'the sensor added a ref\'d timer');
  s.stop();
});

test('outside a browser, construction installs nothing that throws', () => {
  assert.equal(typeof (globalThis as Record<string, unknown>).document, 'undefined');
  const s = sensorFor('http://127.0.0.1:9');
  assert.equal(isDisabled(), false);
  assert.doesNotThrow(() => s.stop());
  assert.doesNotThrow(() => s.stop(), 'stop() is idempotent');
});

// The last moment a count can leave the page. The request must have *started*
// before the handler returns — a fetch begun a tick later may never go out —
// and must be keepalive so the browser lets it outlive the page.
test('pagehide flushes synchronously, with keepalive', async () => {
  await withPage(({ win }) => {
    const { calls, fetchImpl } = recordingFetch();
    const s = sensorFor('http://127.0.0.1:9', { fetchImpl, flushIntervalMs: 60_000 });
    try {
      s.record('error', 'TypeError', 'boom');
      win.dispatchEvent(new Event('pagehide'));

      assert.equal(calls.length, 1, 'no request started before the pagehide handler returned');
      assert.equal(calls[0]!.url, 'http://127.0.0.1:9/v1/flush');
      assert.equal(calls[0]!.init?.keepalive, true);
    } finally {
      s.stop();
    }
  });
});

test('visibilitychange to hidden flushes; to visible does not', async () => {
  await withPage(({ doc }) => {
    const { calls, fetchImpl } = recordingFetch();
    const s = sensorFor('http://127.0.0.1:9', { fetchImpl, flushIntervalMs: 60_000 });
    try {
      s.record('error', 'TypeError', 'boom');

      doc.visibilityState = 'visible';
      doc.dispatchEvent(new Event('visibilitychange'));
      assert.equal(calls.length, 0, 'becoming visible is not a reason to flush');

      doc.visibilityState = 'hidden';
      doc.dispatchEvent(new Event('visibilitychange'));
      assert.equal(calls.length, 1);
      assert.equal(calls[0]!.init?.keepalive, true);
    } finally {
      s.stop();
    }
  });
});

test('stop() removes the page-lifecycle listeners', async () => {
  await withPage(({ doc, win }) => {
    const { calls, fetchImpl } = recordingFetch();
    const s = sensorFor('http://127.0.0.1:9', { fetchImpl, flushIntervalMs: 60_000 });
    s.stop();
    s.record('error', 'TypeError', 'boom');

    win.dispatchEvent(new Event('pagehide'));
    doc.visibilityState = 'hidden';
    doc.dispatchEvent(new Event('visibilitychange'));
    assert.equal(calls.length, 0);
  });
});
