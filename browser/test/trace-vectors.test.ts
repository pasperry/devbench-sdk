import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';

import { parseTrace, formatTrace } from '../src/trace.js';

interface ValidVector {
  raw: string; session: string; intent: string; hop: number; key: string; next_hop: string;
}
interface VectorFile { valid: ValidVector[]; invalid: string[] }

const here = dirname(fileURLToPath(import.meta.url));
const vectors: VectorFile = JSON.parse(
  readFileSync(resolve(process.env.DEVBENCH_TESTDATA ?? resolve(here, '../../../testdata'), 'trace_vectors.json'), 'utf8'),
);

/**
 * The trace contract, shared with the Go and Ruby SDKs.
 *
 * If the browser writes a value Rails renders differently, or Go rejects what
 * Ruby forwards, the request still succeeds and nothing logs — the evidence
 * just cannot be joined, and both detectors quietly degrade. That is risk #9,
 * and this is the part of it a test can hold.
 */
test('trace vectors agree with the Go and Ruby implementations', () => {
  assert.ok(vectors.valid.length > 0, 'vector file is empty');

  for (const v of vectors.valid) {
    const got = parseTrace(v.raw);
    assert.ok(got, `rejected a vector every implementation must accept: ${v.raw}`);
    assert.equal(got!.session, v.session, v.raw);
    assert.equal(got!.intent, v.intent, v.raw);
    assert.equal(got!.hop, v.hop, v.raw);
    assert.equal(formatTrace(got!), v.raw, 'render must round-trip');
    assert.equal(`${got!.session}/${got!.intent}`, v.key, v.raw);
  }

  for (const bad of vectors.invalid) {
    assert.equal(parseTrace(bad), null, `accepted a vector every implementation must reject: ${JSON.stringify(bad)}`);
  }
});
