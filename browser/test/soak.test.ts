import { test } from 'node:test';
import assert from 'node:assert/strict';
import { Sensor } from '../src/sensor.js';
import { resetForTests } from '../src/safe.js';
import { BUDGET } from '../src/budget.js';

/**
 * The soak test (ARCHITECTURE_PROPOSAL.md 4.1, principle #2).
 *
 * "A diagnostics tool that slows down or leaks memory in the customer's app is
 * the worst possible failure." This is where that stops being a sentence in a
 * document. It drives the sensor through a long, realistic session and asserts
 * the footprint budget holds.
 *
 * It measures *our* data structures, which is where the leak risk actually
 * lives: ring buffers, the counter table, and the pending-write list. A browser
 * soak driving a real page covers the capture hooks and DOM overhead and runs
 * nightly — see scripts/soak-browser.mjs.
 *
 * Duration is scaled by ADT_SOAK_EVENTS so CI runs it short on every PR and
 * long on a schedule.
 */

const EVENTS = Number(process.env.ADT_SOAK_EVENTS ?? 200_000);

function heapUsed(): number {
  global.gc?.();
  return process.memoryUsage().heapUsed;
}

test(`sustains ${EVENTS.toLocaleString()} events without growing`, { timeout: 120_000 }, () => {
  resetForTests();

  const sensor = new Sensor({
    endpoint: 'http://127.0.0.1:9/v1/flush',
    tenant: 'acme', ingestKey: 'adt_client_test',
    release: 'soak',
    // No flush: this isolates in-memory growth from network behavior. A sensor
    // that only stays bounded because something drains it is not bounded.
    now: () => Date.now(),
  });

  const samples: number[] = [];
  const sampleEvery = Math.max(1, Math.floor(EVENTS / 20));

  // Timing is counted, not collected.
  //
  // An array of per-event timings would be megabytes of allocation inside the
  // very loop whose heap growth we are measuring, which would mask a real leak
  // with our own instrumentation. Two counters and a max are O(1).
  let overBudget = 0;
  let maxSliceMs = 0;

  for (let i = 0; i < EVENTS; i++) {
    // A realistic mix: mostly repeats of a handful of problems, a slow trickle
    // of genuinely novel ones, plus ordinary traffic.
    const novel = i % 500 === 0;

    sensor.beginIntent('click', novel ? `rare-button-${i}` : 'Save');

    const started = process.hrtime.bigint();

    if (i % 3 === 0) {
      sensor.record('error', novel ? `NovelError${i}` : 'TypeError',
        novel ? `an unseen failure ${i}` : "Cannot read property 'email' of undefined");
    } else if (i % 3 === 1) {
      sensor.recordRequest('PUT', `/api/customers/${i % 50}`, 200, {
        writtenValues: { email: `user${i}@example.com`, name: `Name ${i}` },
      });
    } else {
      sensor.recordRequest('GET', `/api/customers/${i % 50}`, 200, {
        body: { email: `user${i - 1}@example.com`, name: `Name ${i}` },
      });
    }

    const sliceMs = Number(process.hrtime.bigint() - started) / 1e6;
    if (sliceMs > BUDGET.maxTaskMs) overBudget++;
    if (sliceMs > maxSliceMs) maxSliceMs = sliceMs;

    if (i % sampleEvery === 0) samples.push(heapUsed());
  }

  const stats = sensor.stats();

  // 1. Every buffer is bounded by construction.
  assert.ok(stats.signals <= BUDGET.maxSignals, `signals ${stats.signals} > ${BUDGET.maxSignals}`);
  assert.ok(stats.intents <= BUDGET.maxIntents, `intents ${stats.intents} > ${BUDGET.maxIntents}`);
  assert.ok(stats.distinctFingerprints <= BUDGET.maxCountsPerFlush,
    `fingerprints ${stats.distinctFingerprints} > ${BUDGET.maxCountsPerFlush}`);
  assert.ok(stats.pendingWrites <= BUDGET.maxPendingVerifications,
    `pending writes ${stats.pendingWrites} > ${BUDGET.maxPendingVerifications}`);

  // 2. No *unbounded* heap growth.
  //
  //    The distinction matters, and comparing the means of two halves does not
  //    make it. V8 grows its heap in steps when the allocation rate rises, then
  //    plateaus — a legitimate one-time step reads as "3MB of growth" to a
  //    mean-of-halves comparison and fails, while a slow genuine leak that
  //    happens to straddle the midpoint can read as flat.
  //
  //    What separates them is the shape: a leak keeps climbing, a step settles.
  //    So compare the final sample against the median of the back half. A
  //    plateau puts the final sample at the median; a leak puts it well above.
  const back = samples.slice(Math.floor(samples.length / 2));
  assert.ok(back.length >= 4, 'not enough samples to judge growth');

  const sorted = [...back].sort((a, b) => a - b);
  const median = sorted[Math.floor(sorted.length / 2)]!;
  const final = back[back.length - 1]!;
  const growth = final - median;

  assert.ok(
    growth < 1024 * 1024,
    `heap is still climbing at the end of the run: final ${(final / 1024 / 1024).toFixed(1)}MB ` +
      `vs back-half median ${(median / 1024 / 1024).toFixed(1)}MB (+${(growth / 1024).toFixed(0)}KB). ` +
      `That is a leak, not a heap-size step. Samples: ${back.map((b) => (b / 1024 / 1024).toFixed(1)).join(', ')}MB`,
  );

  // 3. Events must not stall the main thread.
  //
  // Asserting on the *maximum* here was wrong, and flaked roughly one run in
  // twenty. Wall-clock around a single call also captures whatever GC pause,
  // scheduler preemption, or JIT deopt happens to land inside it — so over
  // enough events a max eventually measures the host, not this code. A flaky
  // budget check is worse than no budget check: it trains people to re-run CI.
  //
  // What the budget actually means is "the sensor does not stall the UI", which
  // is a property of the distribution. So: essentially every event is inside
  // budget, and nothing is catastrophically slow.
  const overBudgetRate = overBudget / EVENTS;
  assert.ok(
    overBudgetRate < 0.001,
    `${(overBudgetRate * 100).toFixed(3)}% of events exceeded the ${BUDGET.maxTaskMs}ms budget ` +
      `(${overBudget} of ${EVENTS}); at most 0.1% is tolerated as host noise`,
  );

  // A genuine pathology — a pathological regex, an unbounded loop — shows up
  // here rather than in the rate above.
  assert.ok(
    maxSliceMs < 50,
    `slowest single event took ${maxSliceMs.toFixed(2)}ms; that is not GC noise`,
  );
});

test('a full session drains to a bounded evidence bundle', () => {
  resetForTests();
  const sensor = new Sensor({ endpoint: 'http://127.0.0.1:9/v1/flush', tenant: 'acme', ingestKey: 'adt_client_test' });

  for (let i = 0; i < 50_000; i++) {
    sensor.beginIntent('click', `b${i}`);
    sensor.record('error', `T${i % 300}`, `failure ${i}`);
  }

  // Evidence is built only when asked, and is bounded by the buffers it reads.
  const bundle = JSON.stringify(sensor.evidenceBundle('fp'));
  assert.ok(
    bundle.length < 256 * 1024,
    `evidence bundle was ${(bundle.length / 1024).toFixed(0)}KB after a heavy session`,
  );
});
