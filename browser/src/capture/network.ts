/**
 * Global HTTP capture: every `fetch` and `XMLHttpRequest` on the page.
 *
 * This is what Sentry's browser SDK catches and the AngularJS interceptor
 * alone did not: requests made outside `$http` (a vendor widget, a plain
 * `fetch`, jQuery) and requests that never got a response at all.
 *
 * Recorded:
 *   - a response with status >= 400, as `http_<status>`, with the same message
 *     the AngularJS path uses, so the two agree on fingerprints;
 *   - a request that failed without a response (offline, DNS, CORS, reset,
 *     timeout) as `network_error`. An abort the app asked for is not a failure;
 *   - a successful response carrying `x-adt-handled`, as the silent failure
 *     `handled_without_feedback` (see `Sensor.recordRequest`);
 *   - a request that took `sensor.slow.request` ms or more and recorded
 *     nothing else, as the degradation `slow_request`. Measured from the call
 *     to the response: the headers for fetch (the body is the app's to read,
 *     or not), `loadend` for XHR.
 *
 * One request is at most one problem. A request that failed is recorded as
 * that failure and never also as slow: the failure is the actionable fact, a
 * timeout's slowness is its symptom, and counting one bad request twice would
 * double its weight in triage. Likewise a handled-header silent failure is not
 * also counted as slow. Aborts are neither.
 *
 * Wrapped, never replaced: the original is always called with the same
 * `this` and the same arguments, and the app gets back exactly what the
 * original returned — the same promise object for fetch, so timing and
 * rejection behaviour are untouched. Observation runs beside it inside the
 * never-throw boundary.
 *
 * Requests to the sensor's own ingest endpoint and evidence URLs are never
 * recorded. Otherwise a failing ingest records a failure, flushes it, fails
 * again, and loops for the life of the page.
 */
import { safe, safeVoid } from './../safe.js';
import { HANDLED_HEADER, readHandled } from './../trace.js';
import type { Sensor } from './../sensor.js';

/**
 * The slice of `XMLHttpRequest.prototype` we wrap. Arguments are `any[]`
 * (documented exception): the real `open` is overloaded, and the wrapper must
 * forward whatever it was given untouched — including the argument *count*,
 * because `open(m, u)` and `open(m, u, undefined)` differ (the latter is sync).
 */
export interface XhrPrototype {
  open(...args: any[]): void;
  send(...args: any[]): void;
  abort(...args: any[]): void;
}

export interface NetworkCaptureTargets {
  fetch?: typeof fetch;
  XMLHttpRequest?: { prototype: XhrPrototype };
}

interface Req {
  method: string;
  url: string;
  /** clock() when the request started. */
  start: number;
}

/** Monotonic where available: a wall-clock adjustment mid-request must not
 *  make a fast request look slow. */
function clock(): number {
  const p = (globalThis as { performance?: { now?: () => number } }).performance;
  return p && typeof p.now === 'function' ? p.now() : Date.now();
}

interface XhrState extends Req {
  own: boolean;
  appAborted: boolean;
  listening: boolean;
}

/** Installs the fetch and XHR hooks on `target`. Returns their teardown. */
export function installNetworkCapture(sensor: Sensor, target: NetworkCaptureTargets): () => void {
  const teardown: Array<() => void> = [];
  safeVoid(() => {
    wrapFetch(sensor, target, teardown);
    wrapXhr(sensor, target, teardown);
  });
  return () => {
    for (const fn of teardown) safeVoid(fn);
  };
}

/** The outcome of one finished request, shared by fetch and XHR. */
function recordOutcome(sensor: Sensor, req: Req, status: number, headers: () => unknown): void {
  if (status >= 400) {
    sensor.recordRequest(req.method, req.url, status);
    return;
  }
  // 0 is an opaque response, not a verdict on success — but it did complete,
  // so its duration is still the user's wait.
  if (status >= 200) {
    const handled = readHandled(headers());
    if (handled > 0) {
      sensor.recordRequest(req.method, req.url, status, { handled });
      return;
    }
  }
  const limit = sensor.slow.request;
  if (limit > 0 && clock() - req.start >= limit) sensor.recordSlowRequest(req.method, req.url);
}

function wrapFetch(sensor: Sensor, target: NetworkCaptureTargets, teardown: Array<() => void>): void {
  const original = target.fetch;
  if (typeof original !== 'function') return;

  let active = true;
  const wrapped = function (this: unknown, ...args: Parameters<typeof fetch>): Promise<Response> {
    const req = active ? safe(() => describeFetch(sensor, args[0], args[1]), null) : null;

    // Outside the boundary on purpose: if the original throws, the app must
    // see exactly that throw.
    const result = original.apply(this, args);

    if (req) {
      safeVoid(() => {
        if (!result || typeof result.then !== 'function') return;
        // A side branch: both handlers are supplied, so this branch never
        // produces an unhandled rejection of its own, and the app keeps the
        // original promise — rejecting, or not, exactly as before.
        result.then(
          (res) => safeVoid(() => {
            if (active) recordOutcome(sensor, req, res.status, () => res.headers);
          }),
          (err: unknown) => safeVoid(() => {
            if (active && !isAbort(err, req.signal)) sensor.recordNetworkFailure(req.method, req.url);
          }),
        );
      });
    }
    return result;
  };

  target.fetch = wrapped as typeof fetch;
  teardown.push(() => {
    active = false;
    if (target.fetch === wrapped) target.fetch = original;
  });
}

function describeFetch(
  sensor: Sensor,
  input: RequestInfo | URL,
  init: RequestInit | undefined,
): (Req & { signal?: AbortSignal | null }) | null {
  const like = input as { href?: string; url?: string; method?: string; signal?: AbortSignal };
  const url = typeof input === 'string' ? input : like.href ?? like.url ?? String(input);
  if (sensor.isOwnRequest(url)) return null;
  return {
    method: String(init?.method ?? like.method ?? 'GET').toUpperCase(),
    url,
    start: clock(),
    signal: init?.signal ?? like.signal,
  };
}

/** An abort the app asked for is the app's decision, not a failure. */
function isAbort(err: unknown, signal: AbortSignal | null | undefined): boolean {
  if (signal && signal.aborted) return true;
  return !!err && (err as { name?: string }).name === 'AbortError';
}

function wrapXhr(sensor: Sensor, target: NetworkCaptureTargets, teardown: Array<() => void>): void {
  const proto = target.XMLHttpRequest?.prototype;
  if (!proto || typeof proto.open !== 'function' || typeof proto.send !== 'function') return;

  // Prototype, not constructor: covers every XHR, including ones created from
  // a reference to XMLHttpRequest taken before we were installed.
  const { open, send, abort } = proto;
  const states = new WeakMap<object, XhrState>();
  let active = true;

  const onEnd = function (this: XMLHttpRequest): void {
    safeVoid(() => {
      const st = states.get(this);
      if (!active || !st || st.own || st.appAborted) return;
      const status = this.status;
      if (status === 0) {
        // error, timeout, or an abort the app did not ask for
        sensor.recordNetworkFailure(st.method, st.url);
        return;
      }
      // getAllResponseHeaders rather than getResponseHeader: asking for a
      // header CORS does not expose makes the browser log a console error on
      // every request, and that noise would be ours.
      recordOutcome(sensor, st, status, () => headerFromAll(this.getAllResponseHeaders()));
    });
  };

  const wrappedOpen = function (this: XMLHttpRequest, ...args: unknown[]): void {
    safeVoid(() => {
      if (!active) return;
      const url = String(args[1]);
      const prev = states.get(this);
      states.set(this, {
        method: String(args[0] ?? 'GET').toUpperCase(),
        url,
        start: 0,
        own: sensor.isOwnRequest(url),
        appAborted: false,
        listening: prev ? prev.listening : false,
      });
    });
    return open.apply(this, args);
  };

  const wrappedSend = function (this: XMLHttpRequest, ...args: unknown[]): void {
    safeVoid(() => {
      const st = states.get(this);
      if (!active || !st || st.own) return;
      st.start = clock();
      if (st.listening) return;
      st.listening = true;
      this.addEventListener('loadend', onEnd);
    });
    return send.apply(this, args);
  };

  const wrappedAbort = function (this: XMLHttpRequest, ...args: unknown[]): void {
    safeVoid(() => {
      const st = states.get(this);
      if (st) st.appAborted = true;
    });
    return abort.apply(this, args);
  };

  proto.open = wrappedOpen;
  proto.send = wrappedSend;
  proto.abort = wrappedAbort;
  sensor.setNetworkCapture(true);

  teardown.push(() => {
    active = false;
    sensor.setNetworkCapture(false);
    if (proto.open === wrappedOpen) proto.open = open;
    if (proto.send === wrappedSend) proto.send = send;
    if (proto.abort === wrappedAbort) proto.abort = abort;
  });
}

/** Picks `x-adt-handled` out of a raw `getAllResponseHeaders()` block. */
function headerFromAll(all: string | null): Record<string, string> | null {
  if (!all) return null;
  const m = /(?:^|\n)x-adt-handled:[ \t]*([^\r\n]*)/i.exec(all);
  return m ? { [HANDLED_HEADER]: m[1] ?? '' } : null;
}
