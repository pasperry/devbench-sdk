import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';

import { SDK_NAME, Sensor, Transport, VERSION } from '../src/index.js';
import { resetForTests } from '../src/safe.js';
import { startTestServer, type Received } from './helpers/server.js';

/**
 * Every flush request names the SDK that sent it:
 * "sdk": {"name": "browser", "version": VERSION}
 * (docs/server-sdk/wire-protocol.md, "the SDK's name and version"; #307).
 * Checked on what actually reaches a real local HTTP server.
 */

interface SdkVectors {
  names: Record<string, string>;
  name_pattern: string;
  version_pattern: string;
}

const here = dirname(fileURLToPath(import.meta.url));
// DEVBENCH_TESTDATA: the public SDK repo's CI, whose layout differs.
const vectorsPath = resolve(process.env.DEVBENCH_TESTDATA ?? resolve(here, '../../../testdata'), 'protocol_vectors.json');
const sdkVectors: SdkVectors = JSON.parse(readFileSync(vectorsPath, 'utf8')).sdk;
// The test build runs from sdk/client (npm test), as bundle.test.ts reads it.
const PKG_VERSION: string = JSON.parse(readFileSync('package.json', 'utf8')).version;

function assertSdk(r: Received): void {
  const body = JSON.parse(r.body) as Record<string, unknown>;
  assert.deepEqual(body.sdk, { name: 'browser', version: VERSION }, `sdk in ${r.body.slice(0, 300)}`);
  // After sensor_id and before counts, the wire-protocol example's order.
  const keys = Object.keys(body);
  const at = keys.indexOf('sdk');
  assert.deepEqual(keys.slice(at - 1, at + 2), ['sensor_id', 'sdk', 'counts'], `keys: ${keys.join(',')}`);
}

test('a flush from a real Sensor carries sdk, and nothing else changes', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  resetForTests();
  const s = new Sensor({
    endpoint: `${server.url}/v1/flush`, tenant: 'acme', ingestKey: 'adt_client_test',
    release: 'r1', sensorId: 's1', autoFlush: false,
  });
  s.record('error', 'TypeError', 'boom');
  await s.flush();

  const flushes = server.received.filter((r) => r.method === 'POST' && r.url === '/v1/flush');
  assert.equal(flushes.length, 1);
  assertSdk(flushes[0]!);
  // The keys the body had before #307, plus sdk.
  assert.deepEqual(Object.keys(JSON.parse(flushes[0]!.body)), ['v', 'tenant', 'source', 'release', 'sensor_id', 'sdk', 'counts']);
});

test('every beacon of a split flush carries sdk, each within the limit', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const transport = new Transport({
    endpoint: `${server.url}/v1/flush`, tenant: 'acme', ingestKey: 'adt_client_test', release: 'r1', sensorId: 's1',
  });
  const counts = Array.from({ length: 64 }, (_, i) => ({
    fp: `fp-${i}`.padEnd(64, '0'), n: i + 1, first: 100, last: 200, kind: 'error',
    users: [{ email: 'pat@shop.example.com', account: 'acct-1182' }], users_overflow: 0,
  }));
  await transport.flush(counts, { overflowed: 3, dropped: 2 });

  assert.ok(server.received.length > 1, 'expected the flush to need more than one beacon');
  for (const r of server.received) {
    assertSdk(r);
    assert.ok(r.body.length <= 8 * 1024, `beacon of ${r.body.length} bytes`);
  }
});

test('the name is the vectors\' for sdk/client and both values pass ingest\'s patterns', () => {
  const mine = Object.entries(sdkVectors.names).filter(([, dir]) => dir === 'sdk/client').map(([name]) => name);
  assert.deepEqual(mine, [SDK_NAME]);
  // Go-syntax patterns anchored with ^…$; JavaScript reads these the same
  // (no m flag, so $ is the end of the string).
  assert.match(SDK_NAME, new RegExp(sdkVectors.name_pattern));
  assert.match(VERSION, new RegExp(sdkVectors.version_pattern));
});

test('the version on the wire is the package\'s own', () => {
  assert.equal(VERSION, PKG_VERSION);
});
