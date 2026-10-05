import { test } from 'node:test';
import assert from 'node:assert/strict';
import { parseDsn, type Dsn } from '../src/dsn.js';
import { Sensor } from '../src/sensor.js';
import { resetForTests } from '../src/safe.js';
import { startTestServer } from './helpers/server.js';
import { captureWarnings } from './helpers/console.js';
import type { FlushRequest } from '../src/transport.js';

function ok(raw: string): Dsn {
  const d = parseDsn(raw);
  assert.equal(typeof d, 'object', `${raw}: ${String(d)}`);
  return d as Dsn;
}

test('a DSN gives the key and <base>/v1/flush', () => {
  assert.deepEqual(ok('https://adt_client_abc123@adt-ingest.onrender.com'), {
    key: 'adt_client_abc123',
    base: 'https://adt-ingest.onrender.com',
    endpoint: 'https://adt-ingest.onrender.com/v1/flush',
  });
});

test('a DSN keeps its port; a path or query is ignored; the host is lowercased', () => {
  assert.equal(ok('https://k@Ingest.Example.com:8443').endpoint, 'https://ingest.example.com:8443/v1/flush');
  assert.equal(ok('https://k@ingest.example.com/some/path?x=1').endpoint, 'https://ingest.example.com/v1/flush');
  assert.equal(ok('  https://k@ingest.example.com  ').key, 'k');
  assert.equal(ok('https://a%2Bb@ingest.example.com').key, 'a+b');
});

test('http is accepted only for loopback, so the key never crosses the network in plaintext', () => {
  assert.equal(ok('http://k@127.0.0.1:8080').endpoint, 'http://127.0.0.1:8080/v1/flush');
  assert.equal(ok('http://k@localhost:3000').endpoint, 'http://localhost:3000/v1/flush');
  assert.equal(ok('http://k@[::1]:3000').endpoint, 'http://[::1]:3000/v1/flush');
  assert.match(parseDsn('http://k@ingest.example.com') as string, /https/);
});

test('invalid DSNs are answers, not exceptions', () => {
  for (const bad of [
    '', '   ', 'not a dsn', 'ingest.example.com', 'https://ingest.example.com', 'https://@ingest.example.com',
    'ftp://k@ingest.example.com', 'https://k@', 'https://k@host:99999', 'https://k@ho st', 'https://k@-bad-.com',
    'https://%E0%A4%A@ingest.example.com',
  ]) {
    const d = parseDsn(bad);
    assert.equal(typeof d, 'string', `${JSON.stringify(bad)} should be rejected, got ${JSON.stringify(d)}`);
  }
  for (const bad of [undefined, null, 42, {}]) {
    assert.equal(typeof parseDsn(bad), 'string');
  }
});

test('a sensor built from a DSN flushes to <base>/v1/flush with the key from the DSN', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());
  resetForTests();

  const host = server.url.replace('http://', '');
  const s = new Sensor({ dsn: `http://adt_client_dsn@${host}`, release: 'r1', autoFlush: false });
  assert.equal(s.enabled, true);
  s.record('error', 'TypeError', 'boom');
  await s.flush();

  assert.equal(server.received.length, 1);
  const req = server.received[0]!;
  assert.equal(req.url, '/v1/flush');
  assert.equal(req.headers['x-adt-key'], 'adt_client_dsn');
  const body = JSON.parse(req.body) as FlushRequest;
  assert.equal(body.release, 'r1');
  assert.equal(body.counts[0]!.n, 1);
});

test('a DSN wins over endpoint + ingestKey', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());
  resetForTests();

  const host = server.url.replace('http://', '');
  const s = new Sensor({
    dsn: `http://from_dsn@${host}`, endpoint: 'https://elsewhere.invalid/v1/flush', ingestKey: 'from_option', autoFlush: false,
  });
  s.record('error', 'TypeError', 'boom');
  await s.flush();
  assert.equal(server.received[0]?.headers['x-adt-key'], 'from_dsn');
});

test('an invalid DSN: no throw, one warning, an inert sensor that sends nothing', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());
  resetForTests();

  // Any request the sensor tried to make would land on the test server.
  const toServer = ((_u: string, init?: RequestInit) => fetch(`${server.url}/v1/flush`, init)) as typeof fetch;
  let s: Sensor | undefined;
  const warnings = await captureWarnings(async () => {
    s = new Sensor({ dsn: 'http://key@ingest.example.com', fetchImpl: toServer });
    s.record('error', 'TypeError', 'boom');
    s.setUser({ email: 'pat@example.com' });
    s.recordRequest('GET', '/api/x', 500);
    await s.flush();
  });

  assert.equal(s!.enabled, false);
  assert.equal(warnings.length, 1, warnings.join('\n'));
  assert.match(warnings[0]!, /^Devbench: invalid DSN/);
  assert.equal(s!.stats().distinctFingerprints, 0, 'an inert sensor recorded a signal');
  assert.equal(server.received.length, 0, 'an inert sensor sent a request');
});

test('no destination at all is the same: a warning, not a throw', async () => {
  resetForTests();
  let s: Sensor | undefined;
  const warnings = await captureWarnings(() => {
    s = new Sensor({});
  });
  assert.equal(s!.enabled, false);
  assert.equal(warnings.length, 1);
});

test('enabled: false is inert and silent', async () => {
  resetForTests();
  let s: Sensor | undefined;
  const warnings = await captureWarnings(() => {
    s = new Sensor({ dsn: 'https://k@ingest.example.com', enabled: false });
  });
  assert.equal(s!.enabled, false);
  assert.equal(warnings.length, 0);
});

test('existing installs: endpoint + tenant + ingestKey still flush exactly as before', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());
  resetForTests();

  const s = new Sensor({
    endpoint: `${server.url}/v1/flush`, tenant: 'acme', ingestKey: 'adt_client_legacy', release: 'r1', autoFlush: false,
  });
  assert.equal(s.enabled, true);
  s.record('error', 'TypeError', 'boom');
  await s.flush();

  const req = server.received[0]!;
  assert.equal(req.headers['x-adt-key'], 'adt_client_legacy');
  assert.equal((JSON.parse(req.body) as FlushRequest).tenant, 'acme');
});
