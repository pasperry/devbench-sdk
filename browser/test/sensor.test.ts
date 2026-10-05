import { test } from 'node:test';
import assert from 'node:assert/strict';
import { Sensor } from '../src/sensor.js';
import { compute } from '../src/fingerprint.js';
import { resetForTests, isDisabled } from '../src/safe.js';
import { startTestServer } from './helpers/server.js';
import type { FlushRequest } from '../src/transport.js';

function sensorFor(url: string, extra: Partial<ConstructorParameters<typeof Sensor>[0]> = {}) {
  resetForTests();
  return new Sensor({ endpoint: `${url}/v1/flush`, tenant: 'acme', ingestKey: 'adt_client_test', release: 'r1', sensorId: 's1', ...extra });
}

test('repeated identical errors flush as one count, not many requests', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const s = sensorFor(server.url);
  for (let i = 0; i < 500; i++) {
    s.record('error', 'TypeError', "Cannot read property 'email' of undefined");
  }

  await s.flush();

  assert.equal(server.received.length, 1, '500 occurrences, one request');
  const body = JSON.parse(server.received[0]!.body) as FlushRequest;
  assert.equal(body.counts.length, 1);
  assert.equal(body.counts[0]!.n, 500);
});

// The product's thesis, end to end: a 200 response, no exception anywhere, and
// a problem is still found.
test('detects a silent failure with no error raised', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const s = sensorFor(server.url);
  s.beginIntent('submit', 'Save customer');

  // The app reports success.
  s.recordRequest('PUT', 'https://app.example.com/api/customers/9912', 200, {
    writtenValues: { email: 'new@example.com' },
  });

  // A later read shows the old value.
  s.recordRequest('GET', 'https://app.example.com/api/customers/9912', 200, {
    body: { email: 'old@example.com' },
  });

  await s.flush();

  const body = JSON.parse(server.received[0]!.body) as FlushRequest;
  const expected = compute({
    kind: 'silent_failure', source: 'client', type: 'write_readback_divergence',
    message: 'GET /api/customers/<id> reported success but email did not change',
  });
  assert.ok(
    body.counts.some((c) => c.fp === expected.fp),
    `no silent-failure fingerprint in ${JSON.stringify(body.counts)}`,
  );
});

test('a write confirmed by a later read raises nothing', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const s = sensorFor(server.url);
  s.recordRequest('PUT', '/api/customers/9912', 200, { writtenValues: { email: 'new@example.com' } });
  s.recordRequest('GET', '/api/customers/9912', 200, { body: { email: 'new@example.com' } });

  await s.flush();
  assert.equal(server.received.length, 0, 'a working app must be silent');
});

// Risk #2: PII must never leave the browser. Allow-list, not deny-list.
test('field values are not captured unless allow-listed', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const s = sensorFor(server.url);
  s.recordRequest('PUT', '/api/customers/1', 200, { writtenValues: { email: 'secret@example.com' } });
  s.recordRequest('GET', '/api/customers/1', 200, { body: { email: 'different@example.com' } });
  await s.flush();

  const bundle = JSON.stringify(s.evidenceBundle('any'));
  const wire = server.received.map((r) => r.body).join('');
  for (const haystack of [bundle, wire]) {
    assert.ok(!haystack.includes('secret@example.com'), 'a non-allow-listed value escaped');
    assert.ok(!haystack.includes('different@example.com'), 'a read value escaped');
  }
});

test('PII in a message is redacted before it is ever buffered', () => {
  const s = sensorFor('http://127.0.0.1:9');
  s.record('error', 'ValidationError', 'email ada@example.com has already been taken');

  const bundle = JSON.stringify(s.evidenceBundle('any'));
  assert.ok(!bundle.includes('ada@example.com'));
  assert.ok(bundle.includes('<redacted:email>'));
});

test('evidence is built only on request, and carries the session context', () => {
  const s = sensorFor('http://127.0.0.1:9');
  s.beginIntent('click', 'Save');
  s.record('error', 'TypeError', 'boom');

  const bundle = s.evidenceBundle('fp1') as Record<string, unknown>;
  assert.equal(bundle.fp, 'fp1');
  assert.equal((bundle.intents as unknown[]).length, 1);
  assert.equal((bundle.signals as unknown[]).length, 1);
});

test('memory stays bounded under sustained load', () => {
  const s = sensorFor('http://127.0.0.1:9');
  for (let i = 0; i < 50_000; i++) {
    s.beginIntent('click', `button-${i}`);
    s.record('error', `Type${i % 200}`, `failure number ${i}`);
  }

  const stats = s.stats();
  assert.ok(stats.signals <= 128, `signals buffer grew to ${stats.signals}`);
  assert.ok(stats.intents <= 64, `intents buffer grew to ${stats.intents}`);
  assert.ok(stats.distinctFingerprints <= 64, `counter table grew to ${stats.distinctFingerprints}`);
  assert.ok(stats.droppedSignals > 0, 'drops must be counted, not silent');
});

// Principle #2. A bug in ADT must never become a bug in the customer's app.
test('an internal failure disables the sensor instead of throwing', () => {
  resetForTests();
  const s = new Sensor({
    endpoint: 'http://127.0.0.1:9/v1/flush', tenant: 'acme', ingestKey: 'adt_client_test',
    now: () => { throw new Error('clock exploded'); },
  });

  assert.doesNotThrow(() => s.record('error', 'X', 'y'));
  assert.equal(isDisabled(), true);
  assert.doesNotThrow(() => s.record('error', 'X', 'y'), 'still silent once disabled');
  resetForTests();
});

test('a flush against a dead endpoint never rejects', async () => {
  const s = sensorFor('http://127.0.0.1:9');
  s.record('error', 'TypeError', 'boom');
  await assert.doesNotReject(() => s.flush());
});

test('a 4xx client error is recorded as a problem', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const s = sensorFor(server.url);
  s.recordRequest('POST', '/api/invoices', 422);
  await s.flush();

  const body = JSON.parse(server.received[0]!.body) as FlushRequest;
  assert.equal(body.counts.length, 1);
});

// Joining the two halves of a problem depends on one field.
//
// The sidecar indexes the customer's server logs by `<session>/<intent>`. Every
// record in the bundle already carries its intentId, but the session id lives
// only on the sensor. Without it in the payload we hold the browser's account
// of a failure and the server's, with no way to say which lines belong to it.
test('the evidence bundle carries the session id used in the trace header', () => {
  const s = sensorFor('http://127.0.0.1:9');
  s.beginIntent('click', 'Save invoice');
  s.record('error', 'TypeError', 'boom');

  const bundle = s.evidenceBundle('somefp') as { session?: string };
  assert.equal(typeof bundle.session, 'string');
  assert.equal(bundle.session, s.session(), 'must match the id used to build trace headers');

  const header = s.traceHeader();
  assert.ok(header, 'expected a trace header');
  assert.ok(
    header!.value.startsWith(`v1/${bundle.session}/`),
    `trace header ${header!.value} should begin with the bundle's session`,
  );
});

// The trace key the sidecar looks up is session/intent, so an HTTP record must
// be resolvable to one. If a record's intentId is not in the bundle's intents,
// the join has nothing to anchor to.
test('every http record in a bundle resolves to an intent in the same bundle', () => {
  const s = sensorFor('http://127.0.0.1:9');
  s.beginIntent('click', 'Save invoice');
  s.recordRequest('POST', '/api/invoices', 422);

  const bundle = s.evidenceBundle('somefp') as {
    session: string;
    intents: Array<{ id: string }>;
    signals: Array<{ intentId?: string }>;
  };

  const known = new Set(bundle.intents.map((i) => i.id));
  for (const sig of bundle.signals) {
    if (sig.intentId === undefined) continue;
    assert.ok(known.has(sig.intentId), `signal references unknown intent ${sig.intentId}`);
  }
});

// The brief's canonical silent failure, detected in one place.
//
// The user changed their email, the server rescued a uniqueness violation and
// answered 200, and the UI said it worked. The browser is the only place that
// holds both facts at the same moment: what the application told the user,
// and — via one header — that something went wrong producing it.
test('a 200 that swallowed a server-side failure is a silent failure', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const s = sensorFor(server.url);
  s.beginIntent('submit', 'Save profile');
  s.recordRequest('PUT', '/api/customers/9912', 200, { handled: 1 });
  await s.flush();

  const body = JSON.parse(server.received[0]!.body) as FlushRequest;
  const kinds = body.counts.map((c) => c.kind);
  assert.ok(
    kinds.includes('silent_failure'),
    `expected a silent_failure count, got ${JSON.stringify(kinds)}`,
  );
});

// False positives are what spend a team's trust, and trust is spent once.
test('a handled failure the user was told about is not a silent failure', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const s = sensorFor(server.url);
  s.beginIntent('submit', 'Save profile');

  // The server caught the problem and correctly returned an error. This is
  // the system working.
  s.recordRequest('PUT', '/api/customers/9912', 422, { handled: 1 });
  await s.flush();

  const body = JSON.parse(server.received[0]!.body) as FlushRequest;
  const kinds = body.counts.map((c) => c.kind);
  assert.ok(
    !kinds.includes('silent_failure'),
    `a 422 after a handled failure must not be reported as silent: ${JSON.stringify(kinds)}`,
  );
  assert.ok(kinds.includes('error'), 'the 4xx itself should still be recorded');
});

test('an ordinary success reports nothing', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const s = sensorFor(server.url);
  s.beginIntent('submit', 'Save profile');
  s.recordRequest('PUT', '/api/customers/9912', 200);
  await s.flush();

  assert.equal(server.received.length, 0, 'a clean request produced traffic');
});

// Without the server SDK installed there is no header, and the detector must
// stay quiet rather than guess.
test('no handled header means no verdict', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const s = sensorFor(server.url);
  s.beginIntent('submit', 'Save profile');
  s.recordRequest('PUT', '/api/customers/9912', 200, { handled: 0 });
  await s.flush();

  assert.equal(server.received.length, 0);
});
