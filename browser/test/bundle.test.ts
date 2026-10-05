/**
 * The shipped script-tag bundles themselves (dist/, built by `npm test` before
 * the tests run), evaluated as a page evaluates a classic <script>: in a fresh
 * global with a document whose currentScript is the bundle's own tag.
 *
 * This is the only place the global names (`Devbench`, the `ADT` alias) and the
 * auto-init side effect can be tested, because both exist only in the built
 * IIFE, not in the TypeScript sources.
 */
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createContext, runInContext } from 'node:vm';
import { startTestServer, type TestServer } from './helpers/server.js';
import type { FlushRequest } from '../src/transport.js';

const BUNDLES = ['dist/devbench.min.js', 'dist/devbench.js', 'dist/adt.iife.js'];

interface Api {
  VERSION: string;
  Sensor: new (opts: Record<string, unknown>) => { record(k: string, t: string, m: string): void; flush(): Promise<void>; stop(): void };
  init(opts: Record<string, unknown>): unknown;
  getSensor(): { flush(): Promise<void>; stop(): void } | null;
  setUser(u: unknown): void;
  close(): void;
}

/** A fresh browser-like global: the platform APIs the bundle feature-detects. */
function page(currentScript: unknown) {
  const appErrors: unknown[] = [];
  const g = createContext({
    document: { currentScript, getElementsByTagName: () => [] },
    console: { error: (...a: unknown[]) => { appErrors.push(a); }, warn: () => undefined },
    fetch: globalThis.fetch.bind(globalThis),
    URL, TextEncoder, crypto: globalThis.crypto,
    setTimeout, clearTimeout, setInterval, clearInterval,
  }) as Record<string, unknown> & { Devbench?: Api; ADT?: Api; console: { error(...a: unknown[]): void } };
  return { g, appErrors };
}

function load(file: string, currentScript: unknown) {
  const p = page(currentScript);
  runInContext(readFileSync(file, 'utf8'), p.g, { filename: file });
  return p;
}

function dsnFor(server: TestServer): string {
  return server.url.replace('http://', 'http://adt_client_bundle@');
}

for (const file of BUNDLES) {
  test(`${file}: global Devbench, with ADT the very same object`, () => {
    const { g } = load(file, null);
    assert.equal(typeof g.Devbench, 'object');
    assert.equal(g.ADT, g.Devbench, 'ADT must be an alias of Devbench, not a copy');
    assert.equal(g.Devbench!.VERSION, '0.5.0');
    for (const name of ['init', 'setUser', 'registerAngular', 'getSensor', 'close', 'Sensor', 'installErrorCapture', 'registerAngularModule']) {
      assert.equal(typeof (g.Devbench as unknown as Record<string, unknown>)[name], 'function', `Devbench.${name}`);
    }
  });

  test(`${file}: <script data-dsn> is the whole install`, async (t) => {
    const server = await startTestServer();
    t.after(() => server.close());

    const { g, appErrors } = load(file, { src: `https://cdn.example/${file}`, dataset: { dsn: dsnFor(server), release: 'tag-rel' } });
    const sensor = g.Devbench!.getSensor();
    assert.ok(sensor, 'the bundle did not initialize itself from data-dsn');
    t.after(() => g.Devbench!.close());

    g.Devbench!.setUser({ email: 'pat@example.com' });
    g.console.error('from the page');
    assert.equal(appErrors.length, 1, "the page's console.error still runs");
    await sensor!.flush();

    assert.equal(server.received.length, 1);
    assert.equal(server.received[0]!.headers['x-adt-key'], 'adt_client_bundle');
    const body = JSON.parse(server.received[0]!.body) as FlushRequest;
    assert.equal(body.release, 'tag-rel');
    assert.deepEqual(body.counts[0]!.users, [{ email: 'pat@example.com' }]);
  });

  test(`${file}: without data-dsn nothing is installed`, () => {
    const { g } = load(file, { src: `https://cdn.example/${file}`, dataset: {} });
    const errorBefore = g.console.error;
    assert.equal(g.Devbench!.getSensor(), null);
    assert.equal(g.console.error, errorBefore);
  });

  test(`${file}: an existing ADT.Sensor install (endpoint + tenant + ingestKey) still works`, async (t) => {
    const server = await startTestServer();
    t.after(() => server.close());

    const { g } = load(file, null);
    const s = new g.ADT!.Sensor({
      endpoint: `${server.url}/v1/flush`, tenant: 'acme', ingestKey: 'adt_client_old', release: 'r1', autoFlush: false,
    });
    s.record('error', 'TypeError', 'boom');
    await s.flush();
    assert.equal(server.received[0]!.headers['x-adt-key'], 'adt_client_old');
  });
}
