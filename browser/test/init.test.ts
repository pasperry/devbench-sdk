import { test } from 'node:test';
import assert from 'node:assert/strict';
import { init, setUser, registerAngular, getSensor, close, type InitScope } from '../src/init.js';
import { Sensor } from '../src/sensor.js';
import { compute } from '../src/fingerprint.js';
import { resetForTests } from '../src/safe.js';
import { TRACE_HEADER } from '../src/trace.js';
import type { AngularStatic, AngularProvide } from '../src/adapters/angularjs.js';
import { startTestServer, type TestServer } from './helpers/server.js';
import { captureWarnings } from './helpers/console.js';
import { xhrClass, type TestXhr } from './helpers/xhr.js';
import type { FlushRequest } from '../src/transport.js';
import type { Count } from '../src/counters.js';

const settle = () => new Promise((r) => setTimeout(r, 20));

function dsnFor(server: TestServer, key = 'adt_client_init'): string {
  return `${server.url.replace('http://', `http://${key}@`)}`;
}

/**
 * A window-like page: a real EventTarget for error events, the platform's
 * fetch, a Node XHR stand-in that makes real requests, and a console whose
 * error() the app "owns".
 */
function page(): InitScope & EventTarget & { appErrors: unknown[][]; Xhr: { new (): TestXhr } } {
  const win = new EventTarget();
  const appErrors: unknown[][] = [];
  const Xhr = xhrClass();
  return Object.assign(win, {
    fetch: ((...a: Parameters<typeof fetch>) => globalThis.fetch(...a)) as typeof fetch,
    XMLHttpRequest: Xhr,
    console: { error: (...args: unknown[]) => { appErrors.push(args); } },
    appErrors,
    Xhr,
  });
}

/** Flushes the initialized sensor and returns the counts that reached ingest. */
async function flushed(server: TestServer): Promise<Count[]> {
  const before = server.received.length;
  await getSensor()!.flush();
  return server.received
    .slice(before)
    .filter((r) => r.url === '/v1/flush')
    .flatMap((r) => (JSON.parse(r.body) as FlushRequest).counts);
}

function fp(type: string, message: string): string {
  return compute({ kind: 'error', source: 'client', type, message }).fp;
}

function xhrRequest(Xhr: { new (): TestXhr }, method: string, url: string): Promise<TestXhr> {
  return new Promise((resolve) => {
    const xhr = new Xhr();
    xhr.open(method, url);
    xhr.addEventListener('loadend', () => resolve(xhr));
    xhr.send();
  });
}

function fresh(t: { after(fn: () => void): void }): void {
  close();
  resetForTests();
  t.after(() => close());
}

// ---------------------------------------------------------------- the one call

test('init installs every capture, and a second init installs none of them again', async (t) => {
  fresh(t);
  const server = await startTestServer();
  t.after(() => server.close());
  server.handle.set('/api/missing', () => ({ status: 404, body: '{}' }));
  server.handle.set('/api/broken', () => ({ status: 500, body: '{}' }));

  const win = page();
  const first = init({ dsn: dsnFor(server), autoFlush: false }, win);
  let second: Sensor | undefined;
  const warnings = await captureWarnings(() => {
    second = init({ dsn: dsnFor(server), autoFlush: false }, win);
  });
  assert.equal(second, first, 'a second init must return the first sensor');
  assert.equal(getSensor(), first);
  assert.equal(warnings.length, 1);

  // One of each; double installation would count each twice.
  await win.fetch!(`${server.url}/api/missing`);
  await xhrRequest(win.Xhr, 'GET', `${server.url}/api/broken`);
  win.console!.error!('boom from the app');
  win.dispatchEvent(Object.assign(new Event('error'), { error: new TypeError('uncaught thing'), message: 'uncaught thing' }));
  await settle();

  const counts = await flushed(server);
  const byFp = new Map(counts.map((c) => [c.fp, c.n]));
  assert.equal(byFp.get(fp('http_404', 'GET /api/missing failed with 404')), 1, JSON.stringify(counts));
  assert.equal(byFp.get(fp('http_500', 'GET /api/broken failed with 500')), 1, JSON.stringify(counts));
  assert.equal(byFp.get(fp('ConsoleError', 'boom from the app')), 1, JSON.stringify(counts));
  assert.equal(counts.find((c) => c.fp !== undefined && c.n !== 1), undefined, `something was counted twice: ${JSON.stringify(counts)}`);
  assert.equal(counts.length, 4, JSON.stringify(counts));
  assert.equal(win.appErrors.length, 1, "the app's own console.error must still run, once");
  assert.equal(server.received.find((r) => r.url === '/v1/flush')!.headers['x-adt-key'], 'adt_client_init');
});

test('Devbench.setUser reaches the flushed counts, and init({ user }) sets it up front', async (t) => {
  fresh(t);
  const server = await startTestServer();
  t.after(() => server.close());

  const win = page();
  init({ dsn: dsnFor(server), autoFlush: false, user: { email: 'First@Example.com', account: 7 } }, win);
  win.console!.error!('before login change');
  let counts = await flushed(server);
  assert.deepEqual(counts[0]!.users, [{ email: 'first@example.com', account: '7' }]);

  setUser({ email: ' Pat@Example.COM ', account: 'acct-1182' });
  win.console!.error!('after login change');
  counts = await flushed(server);
  assert.deepEqual(counts[0]!.users, [{ email: 'pat@example.com', account: 'acct-1182' }]);

  setUser(null);
  win.console!.error!('after logout');
  counts = await flushed(server);
  assert.equal(counts[0]!.users, undefined);
});

test('init flushes on its own (auto-flush is on)', async (t) => {
  fresh(t);
  const server = await startTestServer();
  t.after(() => server.close());

  const win = page();
  init({ dsn: dsnFor(server), flushIntervalMs: 20 }, win);
  win.console!.error!('nobody calls flush');

  const until = Date.now() + 2000;
  while (server.received.length === 0 && Date.now() < until) await settle();
  assert.equal(server.received.length, 1, 'no flush arrived without a manual flush() call');
});

test('close() removes the hooks, and init can run again', async (t) => {
  fresh(t);
  const server = await startTestServer();
  t.after(() => server.close());

  const win = page();
  const fetchBefore = win.fetch;
  const errorBefore = win.console!.error;
  init({ dsn: dsnFor(server), autoFlush: false }, win);
  assert.notEqual(win.fetch, fetchBefore);

  close();
  assert.equal(getSensor(), null);
  assert.equal(win.fetch, fetchBefore);
  assert.equal(win.console!.error, errorBefore);

  const again = init({ dsn: dsnFor(server), autoFlush: false }, win);
  assert.equal(getSensor(), again);
});

// ---------------------------------------------------------------- disabled

test('an invalid DSN: no throw, one warning, an inert sensor, nothing installed', async (t) => {
  fresh(t);
  const win = page();
  const fetchBefore = win.fetch;
  const errorBefore = win.console!.error;

  let s: Sensor | undefined;
  const warnings = await captureWarnings(() => {
    s = init({ dsn: 'https://not-a-dsn' }, win);
  });

  assert.equal(s!.enabled, false);
  assert.equal(warnings.length, 1, warnings.join('\n'));
  assert.equal(getSensor(), null);
  assert.equal(win.fetch, fetchBefore, 'fetch was wrapped by a disabled install');
  assert.equal(win.console!.error, errorBefore);
  assert.doesNotThrow(() => setUser({ email: 'x@y.z' }));
  assert.doesNotThrow(() => s!.record('error', 'E', 'm'));
});

test('enabled: false installs nothing and says nothing; options that are not an object do not throw', async (t) => {
  fresh(t);
  const win = page();
  const fetchBefore = win.fetch;
  const warnings = await captureWarnings(() => {
    assert.equal(init({ dsn: 'https://k@ingest.example.com', enabled: false }, win).enabled, false);
  });
  assert.equal(warnings.length, 0);
  assert.equal(win.fetch, fetchBefore);

  await captureWarnings(() => {
    assert.equal(init(undefined as never, win).enabled, false);
  });
});

test('setUser before init does nothing and does not throw', (t) => {
  fresh(t);
  assert.doesNotThrow(() => setUser({ email: 'early@example.com' }));
  assert.equal(registerAngular({ module: () => { throw new Error('no'); } } as never), false);
});

test('init with pre-DSN options still works (endpoint + tenant + ingestKey)', async (t) => {
  fresh(t);
  const server = await startTestServer();
  t.after(() => server.close());

  const win = page();
  init({ endpoint: `${server.url}/v1/flush`, tenant: 'acme', ingestKey: 'adt_client_legacy', autoFlush: false }, win);
  win.console!.error!('legacy');
  const counts = await flushed(server);
  assert.equal(counts.length, 1);
  assert.equal(server.received[0]!.headers['x-adt-key'], 'adt_client_legacy');
});

// ---------------------------------------------------------------- AngularJS

/**
 * AngularJS's module registry, as far as the adapter touches it: modules
 * record their config blocks, factories and decorators. `inject` plays the
 * injector for one module's config blocks, handing them real-shaped
 * `$provide` and `$httpProvider` objects.
 */
function fakeAngular() {
  interface Mod { deps?: string[]; configs: unknown[][]; factories: Map<string, unknown[]>; decorators: Map<string, unknown[]> }
  const modules = new Map<string, Mod>();
  let moduleCalls = 0;
  const angular: AngularStatic = {
    module(name: string, deps?: string[]) {
      moduleCalls++;
      let m = modules.get(name);
      if (!m || deps) {
        m = { deps, configs: [], factories: new Map(), decorators: new Map() };
        modules.set(name, m);
      }
      const mm = m;
      const api: any = {
        factory(n: string, fn: unknown[]) { mm.factories.set(n, fn); return api; },
        config(fn: unknown[]) { mm.configs.push(fn); return api; },
        decorator(n: string, fn: unknown[]) { mm.decorators.set(n, fn); return api; },
        run() { return api; },
      };
      return api;
    },
  };

  // One injector's providers.
  const httpProvider = { interceptors: [] as string[] };
  const provided = new Map<string, unknown[]>();
  const decorators: unknown[][] = [];
  const $provide: AngularProvide = {
    factory(n, fn) { provided.set(n, fn); return undefined; },
    decorator(n, fn) { if (n === '$exceptionHandler') decorators.push(fn); return undefined; },
  };
  const call = (arr: unknown[], ...args: unknown[]) => (arr[arr.length - 1] as (...a: unknown[]) => unknown)(...args);

  function inject(name: string): void {
    const m = modules.get(name);
    for (const cfg of m?.configs ?? []) {
      const deps = cfg.slice(0, -1) as string[];
      call(cfg, ...deps.map((d) => (d === '$provide' ? $provide : d === '$httpProvider' ? httpProvider : undefined)));
    }
    for (const [n, fn] of m?.factories ?? []) provided.set(n, fn);
    for (const [n, fn] of m?.decorators ?? []) if (n === '$exceptionHandler') decorators.push(fn);
  }

  /** The $exceptionHandler the injector would build: the app's, decorated in order. */
  function exceptionHandler(appHandler: (ex: Error) => void): (ex: Error) => void {
    return decorators.reduce<(ex: Error) => void>((h, d) => call(d, h) as (ex: Error) => void, appHandler);
  }

  function interceptor(): any {
    return call(provided.get('adtHttpInterceptor')!, { reject: (v: unknown) => Promise.reject(v) });
  }

  return { angular, modules, httpProvider, inject, exceptionHandler, interceptor, moduleCalls: () => moduleCalls };
}

test('with window.angular present, init hooks AngularJS with no dependency for the app to add', async (t) => {
  fresh(t);
  const server = await startTestServer();
  t.after(() => server.close());

  const ng = fakeAngular();
  const win = Object.assign(page(), { angular: ng.angular });
  init({ dsn: dsnFor(server), autoFlush: false }, win);

  assert.ok(ng.modules.has('adt'), 'the adt module was not registered (apps that list it would break)');
  assert.equal(ng.modules.get('ng')?.configs.length, 1, 'the core ng module was not hooked');

  // The injector loads ng — every app's injector does — and nothing else of ours.
  ng.inject('ng');
  assert.deepEqual(ng.httpProvider.interceptors, ['adtHttpInterceptor']);

  const appSaw: Error[] = [];
  // AngularJS's default handler logs through $log.error -> console.error.
  const handler = ng.exceptionHandler((ex) => {
    appSaw.push(ex);
    win.console!.error!(ex);
  });
  handler(new TypeError('digest blew up'));
  assert.equal(appSaw.length, 1, "the app's $exceptionHandler must still run");
  assert.equal(win.appErrors.length, 1, "the app's console.error must still run");

  const interceptor = ng.interceptor();
  const sensor = getSensor()!;
  sensor.beginIntent('submit', 'Save');
  const cfg = interceptor.request({ method: 'GET', url: '/api/x' });
  assert.ok(cfg.headers?.[TRACE_HEADER], 'no trace header stamped');

  const counts = await flushed(server);
  assert.equal(counts.length, 1);
  assert.equal(counts[0]!.fp, fp('TypeError', 'digest blew up'));
  assert.equal(counts[0]!.n, 1);
});

test('an app that also lists the adt module gets one interceptor and one exception hook, not two', async (t) => {
  fresh(t);
  const server = await startTestServer();
  t.after(() => server.close());

  const ng = fakeAngular();
  init({ dsn: dsnFor(server), autoFlush: false }, Object.assign(page(), { angular: ng.angular }));

  ng.inject('ng');
  ng.inject('adt'); // angular.module('app', ['adt'])
  assert.deepEqual(ng.httpProvider.interceptors, ['adtHttpInterceptor']);

  ng.exceptionHandler(() => undefined)(new TypeError('once'));
  const counts = await flushed(server);
  assert.equal(counts[0]!.n, 1, 'one exception was counted twice');
});

test('AngularJS loaded after the script is hooked at DOMContentLoaded; registerAngular is idempotent', async (t) => {
  fresh(t);
  const server = await startTestServer();
  t.after(() => server.close());

  const doc = Object.assign(new EventTarget(), { readyState: 'loading', visibilityState: 'visible' });
  const win = Object.assign(page(), { document: doc as unknown as Document });
  init({ dsn: dsnFor(server), autoFlush: false }, win);

  const ng = fakeAngular();
  (win as InitScope).angular = ng.angular; // angular.js loads
  assert.equal(ng.modules.size, 0);
  doc.dispatchEvent(new Event('DOMContentLoaded'));
  assert.ok(ng.modules.has('adt') && ng.modules.has('ng'), 'not hooked at DOMContentLoaded');

  const calls = ng.moduleCalls();
  assert.equal(registerAngular(ng.angular), true);
  assert.equal(ng.moduleCalls(), calls, 'registerAngular hooked the same angular twice');
});

test('registerAngular hooks an angular that arrives later still', async (t) => {
  fresh(t);
  const server = await startTestServer();
  t.after(() => server.close());

  init({ dsn: dsnFor(server), autoFlush: false }, page());
  const ng = fakeAngular();
  assert.equal(registerAngular(ng.angular), true);
  ng.inject('ng');
  ng.exceptionHandler(() => undefined)(new RangeError('late'));
  const counts = await flushed(server);
  assert.equal(counts[0]!.fp, fp('RangeError', 'late'));
});

test('console capture is muted only inside the AngularJS handler, even when the handler throws', async (t) => {
  fresh(t);
  const server = await startTestServer();
  t.after(() => server.close());

  const ng = fakeAngular();
  const win = Object.assign(page(), { angular: ng.angular });
  init({ dsn: dsnFor(server), autoFlush: false }, win);
  ng.inject('ng');

  const handler = ng.exceptionHandler((ex) => {
    win.console!.error!(ex);
    throw new Error('app handler rethrows');
  });
  assert.throws(() => handler(new TypeError('inside')), /app handler rethrows/);
  win.console!.error!('outside, afterwards');

  const counts = await flushed(server);
  const fps = counts.map((c) => c.fp).sort();
  assert.deepEqual(fps, [fp('TypeError', 'inside'), fp('ConsoleError', 'outside, afterwards')].sort(), JSON.stringify(counts));
});
