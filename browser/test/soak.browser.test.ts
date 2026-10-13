/**
 * The browser soak (#228): the shipped script-tag bundle on a real page in
 * real Chrome, through a long session of fetch and XHR calls, thrown errors,
 * unhandled rejections, console.error and real clicks, with the heap and the
 * DOM measured over the DevTools Protocol. Driven over a bare DevTools socket
 * (helpers/cdp-chrome.ts), not playwright: playwright's Network domain keeps
 * every request in the renderer and grows the heap of a page with no SDK.
 *
 * soak.test.ts soaks the sensor's own data structures under Node. This soaks
 * what that cannot: the capture hooks (the fetch and XHR wrappers, the error,
 * rejection and console listeners, the vitals observers) and what they leave
 * on a page: objects kept alive, listeners piled up, nodes not let go.
 *
 * Every round the page makes, beside its ordinary traffic, each request the
 * hooks treat differently: 200, 500, a 200 carrying x-adt-handled, a request
 * that gets no response, an abort the app asked for, and XHRs both fresh and
 * reused (one XHR object sent again and again, as polling code does). The
 * responses carry ~1 KiB bodies, so a hook that holds on to requests holds on
 * to something measurable. The sensor flushes to a real ingest on another
 * origin, as in production: bounded because it drains is how it ships.
 *
 * Judged like soak.test.ts: the final sample against the median of the back
 * half. A heap-size step plateaus; a leak keeps climbing.
 *
 * Duration is scaled by ADT_BROWSER_SOAK_ROUNDS: short on every PR, long at
 * night (`make soak-browser`). Needs Google Chrome; CHROME_PATH points at
 * another Chrome/Chromium. Never skipped: no browser is a failure.
 */
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { openTab, type Tab } from './helpers/cdp-chrome.js';
import { deadOrigin, SOAK_KEY, startAppSite, startIngest } from './helpers/soak-site.js';

const ROUNDS = Number(process.env.ADT_BROWSER_SOAK_ROUNDS ?? 1200);
const SAMPLES = 20;
/** fetch + XHR pairs per round; each round also reuses one XHR three times. */
const PAIRS = 10;
/** Thrown errors, unhandled rejections and console.errors per round, each. */
const ERRORS = 5;

/**
 * The heap may sit this far above the back-half median at the end. V8 + Blink
 * as soak.test.ts allows: Blink's heap swings by a few hundred KB from one
 * sample to the next. V8's own heap is steady to ~20 KB once the JIT has
 * warmed, so it gets a tighter bound, which is what catches a hook keeping a
 * few hundred bytes per error inside the short run.
 */
const HEAP_SLACK = 1024 * 1024;
const V8_SLACK = 96 * 1024;
/** DOM nodes and JS event listeners: a round renders a 20-row list; a leak adds per round. */
const NODE_SLACK = 100;
const LISTENER_SLACK = 20;

/**
 * The app. `__soak()` is one round of a busy page; it resolves once every
 * request has settled and the errors have been dispatched. Nothing it creates
 * outlives the round except what the page means to keep: one reused XHR and
 * the rendered list.
 */
function page(dsn: string, dead: string): string {
  return `<!doctype html><html><head><meta charset="utf-8"><title>soak</title></head><body>
<button id="save">Save</button><ul id="list"></ul>
<script src="/devbench.min.js" data-dsn="${dsn}" data-release="soak"></script>
<script>
(function () {
  var PAIRS = ${PAIRS};
  var ERRORS = ${ERRORS};
  var DEAD = ${JSON.stringify(dead)};
  var list = document.getElementById('list');

  // A click re-renders the list: the DOM churn a real page has.
  document.getElementById('save').addEventListener('click', function () {
    var html = '';
    for (var r = 0; r < 20; r++) html += '<li>row ' + r + ' <b>' + Math.random() + '</b></li>';
    list.innerHTML = html;
  });

  function xhr(x, method, url) {
    return new Promise(function (resolve) {
      x.onloadend = function () { resolve(x.status); };
      x.open(method, url);
      x.send(method === 'GET' ? null : '{}');
    });
  }
  var reused = new XMLHttpRequest();

  function tick() { return new Promise(function (r) { setTimeout(r, 0); }); }

  var round = 0;
  window.__soak = async function () {
    var i = round++;
    var jobs = [];
    for (var k = 0; k < PAIRS; k++) {
      var id = (i * PAIRS + k) % 50;
      switch (k % 5) {
        case 0: jobs.push(fetch('/api/customers/' + id).then(function (r) { return r.text(); })); break;
        case 1: jobs.push(fetch('/api/fail/' + id, { method: 'PUT', body: '{}' }).then(function (r) { return r.text(); })); break;
        case 2: jobs.push(fetch('/api/handled/' + id).then(function (r) { return r.text(); })); break;
        case 3: jobs.push(fetch(DEAD + '/api/customers/' + id).catch(function () { return null; })); break;
        default: {
          var ac = new AbortController();
          jobs.push(fetch('/api/customers/' + id, { signal: ac.signal }).catch(function () { return null; }));
          ac.abort();
        }
      }
      jobs.push(xhr(new XMLHttpRequest(), k % 2 ? 'GET' : 'PUT', (k % 3 ? '/api/customers/' : '/api/fail/') + id));
    }
    await Promise.all(jobs);
    for (var j = 0; j < 3; j++) await xhr(reused, 'GET', '/api/customers/' + j);

    // Mostly the same few problems, now and then a novel one.
    // ERRORS of each kind per round, so a hook that keeps a few hundred bytes
    // per error climbs past the slack even in the short run.
    var novel = i % 50 === 0;
    for (var e = 0; e < ERRORS; e++) {
      setTimeout(function () {
        throw new TypeError(novel ? 'novel failure ' + i : "Cannot read properties of undefined (reading 'email')");
      }, 0);
      Promise.reject(new Error(novel ? 'novel rejection ' + i : 'save failed'));
      console.error('render failed for row', i % 7);
    }
    await tick();
    await tick();
  };
})();
</script>
</body></html>`;
}

interface Sample {
  heap: number;
  /** Of which V8's own heap (the rest is Blink's). */
  v8: number;
  nodes: number;
  listeners: number;
}

/**
 * Live memory after a full collection. The inspector keeps every console
 * message and uncaught exception (with the error object) up to its own cap;
 * that is DevTools' memory, not the app's or ours, so it is discarded first.
 * `embedderHeapUsedSize` is Blink's own heap (DOM objects, XHRs), where it is
 * reported separately from V8's.
 */
async function measure(tab: Tab): Promise<Sample> {
  await tab.send('Runtime.discardConsoleEntries');
  await tab.send('HeapProfiler.collectGarbage');
  await tab.send('HeapProfiler.collectGarbage');
  const heap = await tab.send<{ usedSize: number; embedderHeapUsedSize?: number }>('Runtime.getHeapUsage');
  const dom = await tab.send<{ nodes: number; jsEventListeners: number }>('Memory.getDOMCounters');
  return { heap: heap.usedSize + (heap.embedderHeapUsedSize ?? 0), v8: heap.usedSize, nodes: dom.nodes, listeners: dom.jsEventListeners };
}

/** final minus the median of the back half, and the back half itself. */
function climb(series: number[]): { growth: number; back: number[] } {
  const back = series.slice(Math.floor(series.length / 2));
  const sorted = [...back].sort((a, b) => a - b);
  const median = sorted[Math.floor(sorted.length / 2)]!;
  return { growth: back[back.length - 1]! - median, back };
}

const mb = (b: number): string => (b / 1024 / 1024).toFixed(2);

test(`a page with the SDK sustains ${ROUNDS.toLocaleString()} rounds of traffic and errors without growing`, { timeout: 120_000 + ROUNDS * 1_000 }, async (t) => {
  assert.ok(Number.isInteger(ROUNDS) && ROUNDS >= SAMPLES * 2, `ADT_BROWSER_SOAK_ROUNDS must be an integer >= ${SAMPLES * 2}`);

  const ingest = await startIngest();
  const dsn = ingest.url.replace('http://', `http://${SOAK_KEY}@`);
  const site = await startAppSite(page(dsn, await deadOrigin()));
  t.after(async () => {
    await site.close();
    await ingest.close();
  });
  const tab = await openTab(site.url + '/');
  t.after(async () => {
    // close() never rejects: a profile it could not remove is a warning (#317).
    const r = await tab.close();
    t.diagnostic(`chrome close: processes [${r.atClose.join(', ')}]; outlived the browser: [${r.stragglers.join(', ')}], waited ${r.waitedMs}ms, ` +
      `${r.killed} SIGKILLed, profile removed: ${r.removed}${r.problem ? ` (${r.problem})` : ''}`);
  });

  assert.equal(await tab.evaluate('!!window.Devbench && Devbench.getSensor() !== null'), true,
    'the script tag did not initialize the SDK');

  const samples: Sample[] = [];
  const every = Math.floor(ROUNDS / SAMPLES);
  const started = Date.now();
  for (let i = 0; i < ROUNDS; i++) {
    await tab.click('#save');
    // The same expression every time: each distinct one is a new script V8
    // compiles and keeps, which grows the heap by itself.
    await tab.evaluate('__soak()');
    if ((i + 1) % every === 0) samples.push(await measure(tab));
  }
  const elapsed = (Date.now() - started) / 1000;

  // Everything still counted goes out now, so the checks below see it all.
  const stats = await tab.evaluate<Record<string, number>>(
    '(async () => { const s = Devbench.getSensor(); await s.flush(); return s.stats(); })()');

  t.diagnostic(`${ROUNDS} rounds, ${site.apiRequests()} API requests reached the app, ${elapsed.toFixed(0)}s`);
  t.diagnostic(`heap samples: ${samples.map((s) => mb(s.heap)).join(', ')}MB`);
  t.diagnostic(`  of which V8: ${samples.map((s) => mb(s.v8)).join(', ')}MB`);
  t.diagnostic(`DOM nodes: ${samples.map((s) => s.nodes).join(', ')}`);
  t.diagnostic(`JS listeners: ${samples.map((s) => s.listeners).join(', ')}`);
  t.diagnostic(`ingest: ${ingest.totals.flushes} flushes, ${ingest.totals.occurrences} occurrences, kinds ${[...ingest.totals.kinds].sort().join(',')}`);

  // 0. The SDK was really capturing, through every hook, and really flushing.
  //    A soak of a page whose hooks never ran proves nothing.
  assert.ok(ingest.totals.flushes >= 1, 'the sensor never flushed');
  assert.equal(ingest.totals.badKey, 0, 'a flush without the DSN key');
  for (const kind of ['error', 'silent_failure']) {
    assert.ok(ingest.totals.kinds.has(kind), `no ${kind} counts reached ingest: a capture hook is not running`);
  }
  // Per round at least: 2 fetch 500s, 4 XHR 500s, 2 no-response fetches, 2
  // handled 200s, and ERRORS each of throws, rejections and console.errors.
  const floor = ROUNDS * (10 + 3 * ERRORS);
  assert.ok(ingest.totals.occurrences >= floor,
    `ingest counted ${ingest.totals.occurrences} occurrences; the page produced at least ${floor}`);

  // 1. The sensor's buffers stayed in budget on a real page too.
  assert.ok(stats.signals! <= 128 && stats.intents! <= 64 && stats.pendingWrites! <= 32,
    `sensor buffers over budget: ${JSON.stringify(stats)}`);

  // 2. No unbounded heap growth.
  const heap = climb(samples.map((s) => s.heap));
  assert.ok(heap.growth < HEAP_SLACK,
    `the page heap is still climbing at the end of the run: +${(heap.growth / 1024).toFixed(0)}KB over the back-half ` +
      `median. That is a leak, not a heap-size step. Back half: ${heap.back.map(mb).join(', ')}MB`);

  const v8 = climb(samples.map((s) => s.v8));
  assert.ok(v8.growth < V8_SLACK,
    `the page's V8 heap is still climbing at the end of the run: +${(v8.growth / 1024).toFixed(0)}KB over the back-half ` +
      `median. Back half: ${v8.back.map(mb).join(', ')}MB`);

  // 3. Nothing piles up in the DOM: nodes and listeners.
  const nodes = climb(samples.map((s) => s.nodes));
  assert.ok(nodes.growth < NODE_SLACK, `DOM nodes still climbing: +${nodes.growth}. Back half: ${nodes.back.join(', ')}`);
  const listeners = climb(samples.map((s) => s.listeners));
  assert.ok(listeners.growth < LISTENER_SLACK,
    `JS event listeners still climbing: +${listeners.growth}. Back half: ${listeners.back.join(', ')}`);
});
