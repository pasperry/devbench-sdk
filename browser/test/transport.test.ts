import { test } from 'node:test';
import assert from 'node:assert/strict';
import { Transport, type FlushRequest } from '../src/transport.js';
import { startTestServer } from './helpers/server.js';

function counts(n: number) {
  return Array.from({ length: n }, (_, i) => ({ fp: `fp-${i}`.padEnd(64, '0'), n: i + 1, first: 100, last: 200, kind: 'error' }));
}

function transportFor(url: string) {
  return new Transport({ endpoint: `${url}/v1/flush`, tenant: 'acme', ingestKey: 'adt_client_test', release: 'abc123', sensorId: 's1' });
}

test('phase 1 posts counts and nothing else', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  await transportFor(server.url).flush(counts(3));

  assert.equal(server.received.length, 1);
  const body = JSON.parse(server.received[0]!.body) as FlushRequest;
  assert.equal(body.v, 1);
  assert.equal(body.tenant, 'acme');
  assert.equal(body.counts.length, 3);

  // No evidence, no messages, no user data in phase 1 — the entire point.
  const raw = server.received[0]!.body;
  assert.ok(!raw.includes('message'), 'phase 1 must not carry message text');
  assert.ok(!raw.includes('stack'), 'phase 1 must not carry stacks');
});

test('an empty flush makes no request at all', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  await transportFor(server.url).flush([]);
  assert.equal(server.received.length, 0, 'a quiet session must be silent on the network');
});

test('phase 2 uploads evidence only when asked, straight to the presigned URL', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  server.handle.set('/v1/flush', () => ({
    status: 200,
    body: JSON.stringify({
      need_evidence: [{ fp: 'fp-0'.padEnd(64, '0'), claim: 'c1', url: `${server.url}/bucket/obj`, expires: 4_102_444_800 }],
    }),
  }));

  const res = await transportFor(server.url).flush(counts(2));
  assert.equal(res.need_evidence?.length, 1);

  const ok = await transportFor(server.url).uploadEvidence(res.need_evidence![0]!, { v: 1, signals: [] });
  assert.equal(ok, true);

  const put = server.received.find((r) => r.method === 'PUT');
  assert.ok(put, 'evidence must be PUT directly at the presigned URL');
  assert.equal(put!.url, '/bucket/obj');
});

test('an expired claim is not uploaded', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const ok = await transportFor(server.url).uploadEvidence(
    { fp: 'x', claim: 'c', url: `${server.url}/bucket/obj`, expires: 1 },
    {},
  );
  assert.equal(ok, false);
  assert.equal(server.received.length, 0);
});

// Retrying a 4xx cannot help and doubles the load on a struggling backend.
test('4xx is not retried', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());
  server.handle.set('/v1/flush', () => ({ status: 400, body: '{}' }));

  await transportFor(server.url).flush(counts(1));
  assert.equal(server.received.length, 1);
});

test('5xx is retried once, then abandoned', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());
  server.handle.set('/v1/flush', () => ({ status: 503, body: '{}' }));

  await transportFor(server.url).flush(counts(1));
  assert.equal(server.received.length, 2, 'one retry, then give up — never queue unboundedly');
});

test('a dead endpoint fails silently and quickly', async () => {
  // Nothing is listening on this port.
  const t = new Transport({ endpoint: 'http://127.0.0.1:9/v1/flush', tenant: 'a', ingestKey: 'k', release: 'r', sensorId: 's' });
  assert.deepEqual(await t.flush(counts(1)), {}, 'must resolve, never reject');
});

// At production settings a full 64-count beacon is ~6.7KB, comfortably inside
// the 8KB budget — so this asserts the invariant that keeps us there, rather
// than pretending the splitter runs in normal operation.
test('a full beacon at production settings is inside the budget', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  await transportFor(server.url).flush(counts(64));

  assert.ok(
    server.received[0]!.body.length <= 8 * 1024,
    `full beacon was ${server.received[0]!.body.length} bytes, over the 8KB budget`,
  );
});

// The splitter is defense-in-depth for a future field being added to Count.
// Exercised with a lowered limit so it is real code rather than a dead branch.
test('an oversized beacon is split rather than sent', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const transport = new Transport({
    endpoint: `${server.url}/v1/flush`, tenant: 'acme', ingestKey: 'adt_client_test', release: 'abc123', sensorId: 's1',
    maxBeaconBytes: 1024,
  });

  await transport.flush(counts(64));

  const sent = JSON.parse(server.received[0]!.body) as FlushRequest;
  assert.ok(server.received[0]!.body.length <= 1024, 'beacon must respect the configured limit');
  assert.ok(sent.counts.length < 64, 'the batch was halved until it fit');
  assert.ok(sent.counts.length > 0, 'splitting must not starve the flush entirely');

  // And nothing is lost: every count arrives in some beacon, each within the
  // limit, and the remainder is not silently dropped.
  // Keyed by n, which counts() makes unique; its padded fps are not.
  const all = server.received.flatMap((r) => (JSON.parse(r.body) as FlushRequest).counts.map((c) => c.n));
  assert.equal(all.length, 64, `only ${all.length} of 64 counts were delivered`);
  assert.equal(new Set(all).size, 64, 'a count was sent twice');
  for (const r of server.received) assert.ok(r.body.length <= 1024, 'every beacon must respect the limit');
});

// Identity makes splitting routine at production settings: 64 counts each
// carrying a user no longer fit one 8KB beacon. They must all still arrive.
test('a full flush with identity on every count is split, not truncated', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const withUsers = counts(64).map((c) => ({
    ...c, users: [{ email: 'pat@shop.example.com', account: 'acct-1182' }], users_overflow: 0,
  }));
  await transportFor(server.url).flush(withUsers, { overflowed: 3, dropped: 2 });

  assert.ok(server.received.length > 1, 'expected the flush to need more than one beacon');
  const bodies = server.received.map((r) => JSON.parse(r.body) as FlushRequest);
  assert.equal(bodies.flatMap((b) => b.counts).length, 64);
  for (const r of server.received) assert.ok(r.body.length <= 8 * 1024, `beacon of ${r.body.length} bytes`);

  // Loss totals are reported once, not once per beacon.
  assert.equal(bodies.reduce((n, b) => n + (b.overflowed ?? 0), 0), 3);
  assert.equal(bodies.reduce((n, b) => n + (b.dropped ?? 0), 0), 2);
});

// Halving must terminate: a single count larger than the limit is sent alone
// rather than recursing forever.
test('splitting terminates on a single oversized count', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const transport = new Transport({
    endpoint: `${server.url}/v1/flush`, tenant: 'acme', ingestKey: 'adt_client_test', release: 'abc123', sensorId: 's1',
    maxBeaconBytes: 10,
  });

  await transport.flush(counts(4));
  assert.equal(server.received.length, 4, 'one beacon per count, not unbounded recursion');
});

// The endpoint is authenticated, so a flush without the key is a flush into a
// 401. Sending it in a header rather than the URL keeps it out of proxy logs and
// Referer headers.
test('the ingest key is sent as a header, never in the URL', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  await transportFor(server.url).flush(counts(1));

  const req = server.received[0]!;
  assert.equal(req.headers['x-adt-key'], 'adt_client_test');
  assert.ok(!req.url.includes('adt_client_test'), 'the key must not appear in the URL');
});

// A server-side http→https redirect does not protect the key: the browser
// sends the full request, key header and all, before it sees the 301. The only
// place this can be stopped is before the first request leaves.
test('refuses an endpoint that would send the key in plaintext', () => {
  assert.throws(
    () => new Transport({ endpoint: 'http://ingest.example.com/v1/flush', tenant: 'a', ingestKey: 'k', release: 'r', sensorId: 's' }),
    /plaintext/,
    'an http endpoint was accepted',
  );

  // The message must explain why a redirect is not a fix, or someone will
  // "solve" this by adding one.
  try {
    new Transport({ endpoint: 'http://ingest.example.com/v1/flush', tenant: 'a', ingestKey: 'k', release: 'r', sensorId: 's' });
  } catch (e) {
    assert.match((e as Error).message, /redirect/, 'the error does not explain why a redirect is insufficient');
  }
});

test('allows https and loopback', () => {
  assert.doesNotThrow(() => new Transport({ endpoint: 'https://ingest.example.com/v1/flush', tenant: 'a', ingestKey: 'k', release: 'r', sensorId: 's' }));
  for (const local of ['http://localhost:8080/v1/flush', 'http://127.0.0.1:8080/v1/flush']) {
    assert.doesNotThrow(() => new Transport({ endpoint: local, tenant: 'a', ingestKey: 'k', release: 'r', sensorId: 's' }), local);
  }
});

// The token is read per flush rather than once at construction: a single-page
// app outlives many tokens, and the backend refreshes the tag on each full page
// load.
test('sends the session token when the page carries one', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const transport = new Transport({
    endpoint: `${server.url}/v1/flush`, tenant: 'acme', ingestKey: 'adt_client_test',
    release: 'r', sensorId: 's', sessionToken: 'adts1.payload.signature',
  });
  await transport.flush(counts(1));

  assert.equal(server.received[0]!.headers['x-adt-session'], 'adts1.payload.signature');
});

test('omits the session header when the page carries no token', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const transport = new Transport({
    endpoint: `${server.url}/v1/flush`, tenant: 'acme', ingestKey: 'adt_client_test',
    release: 'r', sensorId: 's', sessionToken: null,
  });
  await transport.flush(counts(1));

  assert.equal(server.received[0]!.headers['x-adt-session'], undefined);
});
