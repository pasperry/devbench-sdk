import { test } from 'node:test';
import assert from 'node:assert/strict';
import { ReadbackTracker } from '../src/detect/readback.js';
import type { HttpRecord } from '../src/types.js';

function write(fields: Record<string, string>, ok = true): HttpRecord {
  return {
    intentId: 'i1', method: 'PUT', urlTemplate: '/api/customers/<id>',
    status: ok ? 200 : 500, ok, at: 1000, writtenValues: fields,
  };
}

// The canonical case from the brief: the update silently fails inside a rescue
// block, the request returns 200, and nobody finds out until a colleague sees
// the old address. No exception is raised anywhere in this test.
test('detects a write that reported success but did not persist', () => {
  const t = new ReadbackTracker();
  t.recordWrite(write({ email: 'new@example.com' }), '/api/customers/9912');

  const found = t.checkRead('/api/customers/9912', { email: 'old@example.com', name: 'Ada' });

  assert.equal(found.length, 1);
  assert.equal(found[0]!.field, 'email');
  assert.equal(found[0]!.wrote, 'new@example.com');
  assert.equal(found[0]!.read, 'old@example.com');
});

test('a read that agrees is not a divergence', () => {
  const t = new ReadbackTracker();
  t.recordWrite(write({ email: 'new@example.com' }), '/api/customers/9912');

  assert.deepEqual(t.checkRead('/api/customers/9912', { email: 'new@example.com' }), []);
  assert.equal(t.pendingCount, 0, 'a confirmed write is consumed');
});

test('a failed write is not tracked — the user was already told', () => {
  const t = new ReadbackTracker();
  t.recordWrite(write({ email: 'x@example.com' }, false), '/api/customers/9912');
  assert.equal(t.pendingCount, 0);
});

test('reads of other resources leave the write pending', () => {
  const t = new ReadbackTracker();
  t.recordWrite(write({ email: 'new@example.com' }), '/api/customers/9912');

  assert.deepEqual(t.checkRead('/api/customers/7', { email: 'other@example.com' }), []);
  assert.equal(t.pendingCount, 1);
});

test('fields absent from the read are not divergences', () => {
  const t = new ReadbackTracker();
  t.recordWrite(write({ email: 'new@example.com', phone: '555' }), '/api/customers/9912');

  const found = t.checkRead('/api/customers/9912', { email: 'new@example.com' });
  assert.deepEqual(found, [], 'a partial response is not evidence of failure');
});

test('pending writes are bounded', () => {
  const t = new ReadbackTracker(4);
  for (let i = 0; i < 100; i++) t.recordWrite(write({ email: `e${i}` }), `/api/customers/${i}`);
  assert.equal(t.pendingCount, 4);
});

// These are what the next-session check settles, which is how a user hears
// "we noticed this wasn't working for you" days later (proposal 3.7 rung 1).
test('unverified writes are reported, not discarded', () => {
  const t = new ReadbackTracker();
  t.recordWrite(write({ email: 'new@example.com' }), '/api/customers/9912');
  assert.equal(t.unverified().length, 1);
});
