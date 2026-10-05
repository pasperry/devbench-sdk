import { test } from 'node:test';
import assert from 'node:assert/strict';
import { CounterTable } from '../src/counters.js';

// Principle #1: a repeat of a known problem costs a counter increment, not an
// upload. Ten thousand occurrences must be one entry.
test('collapses repeats into one counted entry', () => {
  const c = new CounterTable(10);
  for (let i = 0; i < 10_000; i++) c.record('fp-a', 'error', 1000 + i);

  const { counts } = c.drain();
  assert.equal(counts.length, 1);
  assert.equal(counts[0]!.n, 10_000);
  assert.equal(counts[0]!.first, 1000);
  assert.equal(counts[0]!.last, 10_999);
});

test('tracks distinct fingerprints separately', () => {
  const c = new CounterTable(10);
  c.record('a', 'error', 1); c.record('b', 'silent_failure', 2); c.record('a', 'error', 3);

  const { counts } = c.drain();
  assert.equal(counts.length, 2);
  assert.equal(counts.find((x) => x.fp === 'a')!.n, 2);
  assert.equal(counts.find((x) => x.fp === 'b')!.n, 1);
});

// A flood of novel signatures (a bad deploy) must not grow this map without
// bound. New ones overflow; existing ones keep counting accurately.
test('bounds distinct fingerprints and reports overflow', () => {
  const c = new CounterTable(4);
  for (let i = 0; i < 100; i++) c.record(`fp-${i}`, 'error', 1);
  c.record('fp-0', 'error', 2);

  assert.equal(c.size, 4);
  const { counts, overflowed } = c.drain();
  assert.equal(counts.length, 4);
  assert.equal(overflowed, 96);
  assert.equal(counts.find((x) => x.fp === 'fp-0')!.n, 2, 'admitted fingerprints keep counting');
});

test('drain resets', () => {
  const c = new CounterTable(4);
  c.record('a', 'error', 1);
  c.drain();
  assert.equal(c.size, 0);
  assert.deepEqual(c.drain().counts, []);
});

// Regression: the kind must survive the flush. It was originally dropped, so
// every signal - including a silent failure, the product's whole thesis - landed
// on the server labelled "error".
test('carries the signal class through to the flush', () => {
  const c = new CounterTable(10);
  c.record('a', 'silent_failure', 1);
  c.record('b', 'degradation', 2);

  const { counts } = c.drain();
  assert.equal(counts.find((x) => x.fp === 'a')!.kind, 'silent_failure');
  assert.equal(counts.find((x) => x.fp === 'b')!.kind, 'degradation');
});
