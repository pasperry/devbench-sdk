import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { fieldValue, newSessionSalt, redactText, redactUrl, DEFAULT_REDACTION } from '../src/redact.js';
import { Sensor } from '../src/sensor.js';
import { resetForTests } from '../src/safe.js';
import { startTestServer } from './helpers/server.js';
import type { FlushRequest } from '../src/transport.js';

const salt = newSessionSalt();

/**
 * Regression test for a real defect found while writing the sensor tests.
 *
 * Redaction originally reduced a non-allow-listed value to `<len:N>`. But
 * "old@example.com" and "new@example.com" are both fifteen characters, so an
 * email that silently failed to update produced identical tokens before and
 * after — making the brief's own canonical silent failure invisible to the
 * detector built to catch it.
 *
 * Values must therefore be compared by salted hash, never by length.
 */
test('different values of equal length produce different tokens', () => {
  const a = fieldValue('email', 'old@example.com', DEFAULT_REDACTION, salt);
  const b = fieldValue('email', 'new@example.com', DEFAULT_REDACTION, salt);

  assert.equal('old@example.com'.length, 'new@example.com'.length, 'premise: same length');
  assert.notEqual(a, b, 'equal-length values must not collapse to the same token');
});

test('the same value produces the same token within a session', () => {
  assert.equal(
    fieldValue('email', 'a@example.com', DEFAULT_REDACTION, salt),
    fieldValue('email', 'a@example.com', DEFAULT_REDACTION, salt),
  );
});

test('the same value produces different tokens across sessions', () => {
  const s1 = newSessionSalt();
  const s2 = newSessionSalt();
  assert.notEqual(
    fieldValue('email', 'a@example.com', DEFAULT_REDACTION, s1),
    fieldValue('email', 'a@example.com', DEFAULT_REDACTION, s2),
    'tokens must not be correlatable across sessions or users',
  );
});

test('a token does not contain the value', () => {
  const token = fieldValue('email', 'secret@example.com', DEFAULT_REDACTION, salt);
  assert.ok(!token.includes('secret'));
  assert.ok(!token.includes('example.com'));
});

test('allow-listed fields are captured verbatim', () => {
  const cfg = { allowInputValues: ['status'] };
  assert.equal(fieldValue('status', 'active', cfg, salt), 'active');
  assert.ok(fieldValue('email', 'a@b.co', cfg, salt).startsWith('<h:'));
});

// Each shape must be redacted *and* labelled correctly. The greedy phone
// pattern will swallow SSNs and card numbers if it is ordered before them —
// still redacted, but mislabelled, which degrades the signal on the issue a
// human eventually reads.
test('redactText removes PII shapes with the right label', () => {
  const cases: Array<[string, string]> = [
    ['email ada@example.com taken', '<redacted:email>'],
    ['ssn 123-45-6789 on file', '<redacted:ssn>'],
    ['card 4111 1111 1111 1111 declined', '<redacted:card>'],
    ['call +1 (555) 010-9987 back', '<redacted:phone>'],
  ];

  for (const [input, label] of cases) {
    const out = redactText(input);
    assert.ok(out.includes(label), `${JSON.stringify(input)} -> ${JSON.stringify(out)}, expected ${label}`);
  }

  assert.equal(redactText('nothing sensitive'), 'nothing sensitive');
});

test('redactUrl keeps path shape and drops identifiers and query', () => {
  assert.equal(redactUrl('https://app.example.com/api/customers/9912?token=secret'), '/api/customers/<id>');
  assert.equal(redactUrl('/api/c/c8f2a1b4-1111-4222-8333-444455556666'), '/api/c/<uuid>');
  assert.equal(redactUrl('/api/health'), '/api/health');
});

test('a secret in a query string never reaches the wire', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());
  resetForTests();

  const s = new Sensor({ endpoint: `${server.url}/v1/flush`, tenant: 'acme', ingestKey: 'adt_client_test' });
  s.recordRequest('POST', '/api/login?password=hunter2&token=abc123', 500);
  await s.flush();

  const wire = server.received.map((r) => r.body).join('');
  assert.ok(!wire.includes('hunter2'), 'a query-string secret escaped');
  assert.ok(!wire.includes('abc123'), 'a query-string token escaped');
  const body = JSON.parse(server.received[0]!.body) as FlushRequest;
  assert.equal(body.counts.length, 1, 'the problem is still reported');
});

// The end-to-end version of the regression above: the detector must catch a
// same-length silent failure through the real sensor path.
test('a same-length silent failure is still detected end to end', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());
  resetForTests();

  const s = new Sensor({ endpoint: `${server.url}/v1/flush`, tenant: 'acme', ingestKey: 'adt_client_test' });
  s.recordRequest('PUT', '/api/customers/1', 200, { writtenValues: { email: 'new@example.com' } });
  s.recordRequest('GET', '/api/customers/1', 200, { body: { email: 'old@example.com' } });
  await s.flush();

  assert.equal(server.received.length, 1, 'the divergence must produce a flush');
  const body = JSON.parse(server.received[0]!.body) as FlushRequest;
  assert.equal(body.counts.length, 1);
});

// The contract between this SDK and the sidecar.
//
// Both redact free text, and a divergence means one side leaks what the other
// removes — a gap nobody notices until it is in a GitHub issue. The vectors
// live in testdata/redaction_vectors.json and the Go side checks the same
// file.
//
// Only the `shared` section applies here. The server-only patterns cover
// credentials that appear in server logs and never in a browser: this bundle
// has a size budget, and a page has no Authorization header to log.
test('the shared redaction vectors hold in the browser SDK', async () => {
  const raw = await readFile(
    process.env.DEVBENCH_TESTDATA
      ? `${process.env.DEVBENCH_TESTDATA}/redaction_vectors.json`
      : new URL('../../../testdata/redaction_vectors.json', import.meta.url),
    'utf8',
  );
  const vectors = JSON.parse(raw) as {
    shared: Array<{ name: string; in: string; out: string }>;
    must_survive: Array<{ name: string; in: string; out: string }>;
  };

  assert.ok(vectors.shared.length > 0, 'no shared vectors; this test proves nothing');

  for (const v of vectors.shared) {
    assert.equal(redactText(v.in), v.out, `${v.name}: ${v.in}`);
  }

  // A redactor that eats the diagnostic content is useless.
  for (const v of vectors.must_survive) {
    assert.equal(redactText(v.in), v.out, `${v.name}: destroyed diagnostic content`);
  }
});
