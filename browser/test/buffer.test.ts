import { test } from 'node:test';
import assert from 'node:assert/strict';
import { RingBuffer } from '../src/buffer.js';

test('holds entries in order until capacity', () => {
  const b = new RingBuffer<number>(3);
  b.push(1); b.push(2);
  assert.deepEqual(b.toArray(), [1, 2]);
  assert.equal(b.length, 2);
  assert.equal(b.droppedCount, 0);
});

// The memory ceiling is the whole point: a buffer that grows under load is a
// leak in someone else's application (principle #2).
test('never exceeds capacity, dropping oldest', () => {
  const b = new RingBuffer<number>(3);
  for (let i = 0; i < 1000; i++) b.push(i);

  assert.equal(b.length, 3);
  assert.deepEqual(b.toArray(), [997, 998, 999]);
  assert.equal(b.droppedCount, 997, 'dropped entries must be counted, not lost silently');
});

test('clear releases entries', () => {
  const b = new RingBuffer<string>(2);
  b.push('a'); b.push('b');
  b.clear();
  assert.equal(b.length, 0);
  assert.deepEqual(b.toArray(), []);
});

test('rejects a nonsensical capacity', () => {
  assert.throws(() => new RingBuffer<number>(0));
  assert.throws(() => new RingBuffer<number>(-1));
  assert.throws(() => new RingBuffer<number>(1.5));
});
