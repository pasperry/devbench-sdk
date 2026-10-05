import { test } from 'node:test';
import assert from 'node:assert/strict';
import { autoInit } from '../src/autoinit.js';
import { close, getSensor, type InitScope } from '../src/init.js';
import { resetForTests } from '../src/safe.js';
import { startTestServer, type TestServer } from './helpers/server.js';
import type { FlushRequest } from '../src/transport.js';

function fresh(t: { after(fn: () => void): void }): void {
  close();
  resetForTests();
  t.after(() => close());
}

function dsnFor(server: TestServer): string {
  return server.url.replace('http://', 'http://adt_client_tag@');
}

/** A window-like page whose document has the given scripts. */
function pageWith(doc: Record<string, unknown>) {
  const appErrors: unknown[] = [];
  const win = Object.assign(new EventTarget(), {
    document: doc,
    fetch: ((...a: Parameters<typeof fetch>) => globalThis.fetch(...a)) as typeof fetch,
    console: { error: (...args: unknown[]) => { appErrors.push(args); } },
  });
  return win as typeof win & InitScope;
}

function script(src: string, data: Record<string, string> = {}) {
  return { src, dataset: { ...data } };
}

test('<script data-dsn> initializes itself: capture installed, release from data-release, key from the DSN', async (t) => {
  fresh(t);
  const server = await startTestServer();
  t.after(() => server.close());

  const tag = script('https://unpkg.com/devbench@0.5.0/dist/devbench.min.js', { dsn: dsnFor(server), release: 'abc123' });
  const win = pageWith({ currentScript: tag });
  const fetchBefore = win.fetch;

  const s = autoInit(win);
  assert.ok(s, 'no sensor from a tag with data-dsn');
  assert.equal(getSensor(), s);
  assert.notEqual(win.fetch, fetchBefore, 'capture was not installed');

  win.console.error('from the page');
  await s!.flush();
  s!.stop();

  assert.equal(server.received.length, 1);
  assert.equal(server.received[0]!.headers['x-adt-key'], 'adt_client_tag');
  const body = JSON.parse(server.received[0]!.body) as FlushRequest;
  assert.equal(body.release, 'abc123');
  assert.equal(body.counts[0]!.n, 1);
});

test('without data-dsn nothing happens', (t) => {
  fresh(t);
  const win = pageWith({ currentScript: script('/assets/devbench.js', { release: 'abc' }) });
  const fetchBefore = win.fetch;
  const errorBefore = win.console.error;

  assert.equal(autoInit(win), null);
  assert.equal(getSensor(), null);
  assert.equal(win.fetch, fetchBefore);
  assert.equal(win.console.error, errorBefore);
});

test('an empty data-dsn, no document, or a hostile document: nothing happens, nothing throws', (t) => {
  fresh(t);
  assert.equal(autoInit(pageWith({ currentScript: script('/devbench.js', { dsn: '  ' }) })), null);
  assert.equal(autoInit({} as InitScope), null);
  const hostile = pageWith({});
  Object.defineProperty(hostile.document, 'currentScript', { get() { throw new Error('nope'); } });
  assert.equal(autoInit(hostile), null);
  assert.equal(getSensor(), null);
});

test('with no currentScript, the last devbench script carrying data-dsn is used', async (t) => {
  fresh(t);
  const server = await startTestServer();
  t.after(() => server.close());

  const scripts = [
    script('/assets/vendor.js', { dsn: 'https://wrong@ingest.example.com' }), // not ours
    script('/assets/devbench.js', { dsn: dsnFor(server), release: 'from-fallback' }),
    script('/assets/app.js'),
  ];
  const win = pageWith({ currentScript: null, getElementsByTagName: (n: string) => (n === 'script' ? scripts : []) });

  const s = autoInit(win);
  assert.ok(s, 'the fallback lookup found nothing');
  win.console.error('x');
  await s!.flush();
  s!.stop();
  assert.equal((JSON.parse(server.received[0]!.body) as FlushRequest).release, 'from-fallback');
});

test('the bundle concatenated into an app script (currentScript has no data-dsn) does not auto-init', (t) => {
  fresh(t);
  const scripts = [script('/assets/devbench.js', { dsn: 'https://k@ingest.example.com' })];
  const win = pageWith({ currentScript: script('/assets/application-1a2b3c.js'), getElementsByTagName: () => scripts });
  assert.equal(autoInit(win), null);
});

test('an invalid data-dsn warns once and installs nothing', (t) => {
  fresh(t);
  const win = pageWith({ currentScript: script('/devbench.min.js', { dsn: 'devbench please' }) });
  const fetchBefore = win.fetch;
  const original = console.warn;
  const seen: unknown[] = [];
  console.warn = (...a: unknown[]) => { seen.push(a); };
  try {
    const s = autoInit(win);
    assert.equal(s?.enabled, false);
  } finally {
    console.warn = original;
  }
  assert.equal(seen.length, 1);
  assert.equal(win.fetch, fetchBefore);
});
