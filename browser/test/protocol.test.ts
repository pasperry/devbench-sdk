import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';

import { PROTOCOL_VERSION, Sensor, type FlushRequest } from '../src/index.js';
import { resetForTests } from '../src/safe.js';
import { startTestServer } from './helpers/server.js';

interface ProtocolVectors {
  current: number;
  supported: number[];
  refused: number[];
}

const here = dirname(fileURLToPath(import.meta.url));
// DEVBENCH_TESTDATA: the public SDK repo's CI, whose layout differs.
const vectorsPath = resolve(process.env.DEVBENCH_TESTDATA ?? resolve(here, '../../../testdata'), 'protocol_vectors.json');
const vectors: ProtocolVectors = JSON.parse(readFileSync(vectorsPath, 'utf8'));

/**
 * The protocol version (docs/SERVER_SDK_SPEC.md, "Protocol version"): the
 * entry point declares the one every SDK and ingest agree on, so a release
 * speaking a version ingest refuses cannot pass.
 */
test('the declared protocol version is the vectors\' current, supported and not refused', () => {
  assert.equal(typeof vectors.current, 'number', 'protocol_vectors.json has no current');
  assert.equal(PROTOCOL_VERSION, vectors.current);
  assert.ok(vectors.supported.includes(PROTOCOL_VERSION), `${PROTOCOL_VERSION} is not in supported ${JSON.stringify(vectors.supported)}`);
  assert.ok(!vectors.refused.includes(PROTOCOL_VERSION), `${PROTOCOL_VERSION} is in refused ${JSON.stringify(vectors.refused)}`);
});

/**
 * Every payload the browser SDK sends carries the declared version as `v`:
 * the phase-1 flush body and the evidence bundle, as they reach a real HTTP
 * server from a real Sensor.
 */
test('the flush body and the evidence bundle both send PROTOCOL_VERSION as v', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  // Ask for evidence on whatever fingerprint the flush reports.
  server.handle.set('/v1/flush', (req) => {
    const fp = (JSON.parse(req.body) as FlushRequest).counts[0]?.fp;
    if (!fp) return undefined;
    return {
      status: 200,
      body: JSON.stringify({ need_evidence: [{ fp, claim: 'c1', url: `${server.url}/bucket/obj`, expires: 4_102_444_800 }] }),
    };
  });

  resetForTests();
  const s = new Sensor({
    endpoint: `${server.url}/v1/flush`, tenant: 'acme', ingestKey: 'adt_client_test',
    release: 'r1', sensorId: 's1', autoFlush: false,
  });
  s.record('error', 'TypeError', 'boom');
  await s.flush();

  const flush = server.received.find((r) => r.method === 'POST' && r.url === '/v1/flush');
  assert.ok(flush, 'no flush reached ingest');
  const body = JSON.parse(flush!.body) as Record<string, unknown>;
  assert.equal(body.v, PROTOCOL_VERSION, 'flush body v');
  assert.equal((body.counts as unknown[]).length, 1);

  const put = server.received.find((r) => r.method === 'PUT' && r.url === '/bucket/obj');
  assert.ok(put, 'evidence was asked for and never uploaded');
  const bundle = JSON.parse(put!.body) as Record<string, unknown>;
  assert.equal(bundle.v, PROTOCOL_VERSION, 'evidence bundle v');
  assert.equal(bundle.fp, (body.counts as { fp: string }[])[0]!.fp);
});
