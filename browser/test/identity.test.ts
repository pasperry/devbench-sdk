import { test } from 'node:test';
import assert from 'node:assert/strict';
import { Sensor } from '../src/sensor.js';
import { compute } from '../src/fingerprint.js';
import { resetForTests } from '../src/safe.js';
import { startTestServer, type TestServer } from './helpers/server.js';
import type { FlushRequest } from '../src/transport.js';

function sensorFor(url: string): Sensor {
  resetForTests();
  return new Sensor({ endpoint: `${url}/v1/flush`, tenant: 'acme', ingestKey: 'adt_client_test', release: 'r1', sensorId: 's1', autoFlush: false });
}

function bodies(server: TestServer): FlushRequest[] {
  return server.received.map((r) => JSON.parse(r.body) as FlushRequest);
}

test('counts carry the normalized identity held when they were recorded', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const s = sensorFor(server.url);
  s.setUser({ email: '  Pat@Example.COM ', account: 1182 });
  s.record('error', 'TypeError', 'boom');
  s.record('error', 'TypeError', 'boom');
  await s.flush();

  const c = bodies(server)[0]!.counts[0]!;
  assert.equal(c.n, 2);
  assert.deepEqual(c.users, [{ email: 'pat@example.com', account: '1182' }], 'one identity, listed once');
  assert.equal(c.users_overflow, 0);
});

test('each field is truncated to 255 characters, not rejected', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const s = sensorFor(server.url);
  s.setUser({ email: `${'a'.repeat(300)}@x.com`, account: 'b'.repeat(300) });
  s.record('error', 'TypeError', 'boom');
  await s.flush();

  const u = bodies(server)[0]!.counts[0]!.users![0]!;
  assert.equal(u.email!.length, 255);
  assert.equal(u.account!.length, 255);
});

test('with no identity set, users and users_overflow are omitted', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const s = sensorFor(server.url);
  s.record('error', 'TypeError', 'boom');
  s.setUser({ email: '   ', account: '' }); // nothing usable is no identity
  s.record('error', 'TypeError', 'other');
  await s.flush();

  assert.ok(!server.received[0]!.body.includes('users'), server.received[0]!.body);
});

test('setUser(null) clears the identity', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const s = sensorFor(server.url);
  s.setUser({ email: 'pat@example.com' });
  s.record('error', 'TypeError', 'boom');
  s.setUser(null);
  s.record('error', 'TypeError', 'boom');
  s.record('error', 'RangeError', 'after logout');
  await s.flush();

  const counts = bodies(server)[0]!.counts;
  const boom = counts.find((c) => c.n === 2)!;
  assert.deepEqual(boom.users, [{ email: 'pat@example.com' }], 'only the identity actually held is listed');
  const after = counts.find((c) => c.n === 1)!;
  assert.equal(after.users, undefined, 'a cleared identity still attached itself');
});

test('users are the distinct identities since the last flush, and reset with it', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const s = sensorFor(server.url);
  s.setUser({ email: 'a@x.com', account: '1' });
  s.record('error', 'TypeError', 'boom');
  s.setUser({ email: 'b@x.com', account: '2' });
  s.record('error', 'TypeError', 'boom');
  s.setUser({ email: 'a@x.com', account: '1' });
  s.record('error', 'TypeError', 'boom');
  await s.flush();

  assert.deepEqual(bodies(server)[0]!.counts[0]!.users, [
    { email: 'a@x.com', account: '1' },
    { email: 'b@x.com', account: '2' },
  ]);

  s.setUser({ email: 'b@x.com', account: '2' });
  s.record('error', 'TypeError', 'boom');
  await s.flush();
  assert.deepEqual(bodies(server)[1]!.counts[0]!.users, [{ email: 'b@x.com', account: '2' }], 'a window must not inherit the last one\'s users');
});

test('at most 20 users per count; the rest are counted in users_overflow', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const s = sensorFor(server.url);
  for (let i = 0; i < 25; i++) {
    s.setUser({ email: `u${i}@x.com` });
    s.record('error', 'TypeError', 'boom');
  }
  // The last user repeating is one user, not three more.
  s.record('error', 'TypeError', 'boom');
  s.record('error', 'TypeError', 'boom');
  await s.flush();

  const c = bodies(server)[0]!.counts[0]!;
  assert.equal(c.users!.length, 20);
  assert.equal(c.users_overflow, 5);
});

// Identity is its own field. If it ever reached the fingerprint, one problem
// would split into one issue per user; if it reached a message or evidence, it
// would bypass the redaction everything else goes through.
test('identity never enters a fingerprint, a message or evidence', async (t) => {
  const server = await startTestServer();
  t.after(() => server.close());

  const s = sensorFor(server.url);
  s.setUser({ email: 'pat@example.com', account: 'acct-1182' });
  s.record('error', 'ValidationError', 'email pat@example.com has already been taken');
  await s.flush();

  const expected = compute({
    kind: 'error', source: 'client', type: 'ValidationError',
    message: 'email <redacted:email> has already been taken',
  });
  const c = bodies(server)[0]!.counts[0]!;
  assert.equal(c.fp, expected.fp, 'the fingerprint depends on who hit it');

  const bundle = JSON.stringify(s.evidenceBundle(c.fp));
  assert.ok(!bundle.includes('pat@example.com'), 'the email reached evidence');
  assert.ok(!bundle.includes('acct-1182'), 'the account reached evidence');
  assert.ok(bundle.includes('<redacted:email>'), 'the email in the message must still be redacted');

  // On the wire, the address appears exactly once: in the users field.
  const raw = server.received[0]!.body;
  assert.equal(raw.split('pat@example.com').length - 1, 1, raw);
});
