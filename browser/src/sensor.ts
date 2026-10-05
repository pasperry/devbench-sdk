/**
 * The sensor: everything a browser session holds.
 *
 * Deterministic, always-on, and strictly budgeted. It keeps everything local,
 * counts repeats, and surfaces something only when a trigger fires
 * (ARCHITECTURE_PROPOSAL.md, "Operating model").
 */
import { BUDGET } from './budget.js';
import { RingBuffer } from './buffer.js';
import { CounterTable } from './counters.js';
import { compute, type Frame } from './fingerprint.js';
import { DEFAULT_REDACTION, fieldValue, newSessionSalt, redactText, redactUrl, type RedactionConfig } from './redact.js';
import { ReadbackTracker } from './detect/readback.js';
import { Transport } from './transport.js';
import { parseDsn, warn } from './dsn.js';
import { safe, safeVoid } from './safe.js';
import type { CapturedSignal, HttpRecord, Identity, Intent, SignalKind } from './types.js';
import { formatTrace, newId, TRACE_HEADER, type Trace } from './trace.js';

export interface SensorOptions {
  /**
   * `https://<ingest key>@host[:port]`, from the key Dev Bench minted for this
   * environment (SERVER_SDK_SPEC.md, "Dev Bench naming and configuration").
   * Replaces `endpoint` + `ingestKey`. An invalid DSN never throws: it logs
   * one console.warn and the sensor is inert (records and sends nothing).
   */
  dsn?: string;
  /** Pre-DSN installs: the flush endpoint. Ignored when `dsn` is set. */
  endpoint?: string;
  /** Optional: the ingest key alone identifies the tenant on the server. */
  tenant?: string;
  /** Pre-DSN installs: the public write-only ingest key. See TransportOptions.ingestKey. */
  ingestKey?: string;
  /** `false` makes the sensor inert (no capture, no requests), without a warning. */
  enabled?: boolean;
  release?: string;
  redaction?: RedactionConfig;
  fetchImpl?: typeof fetch;
  now?: () => number;
  sensorId?: string;
  /**
   * Flush on a timer and when the page is hidden. Default true.
   *
   * On by default because the alternative failed for real: an install shipped
   * without the integrator's own `setInterval` and sent nothing at all. Turn it
   * off only to drive `flush()` yourself.
   */
  autoFlush?: boolean;
  /** Timer period for `autoFlush`. Default `BUDGET.flushIntervalMs`. */
  flushIntervalMs?: number;
  /**
   * A fetch or XHR taking at least this long (start to response) is recorded
   * as a `slow_request` degradation. Default 3000; 0 disables.
   */
  slowRequestMs?: number;
  /**
   * A page whose Largest Contentful Paint is at least this is recorded as a
   * `slow_page_load` degradation (web.dev "poor"). Default 4000; 0 disables.
   */
  slowPageLoadMs?: number;
  /**
   * An interaction whose Event Timing duration (input to next paint) is at
   * least this is recorded as `slow_interaction` (web.dev INP "poor").
   * Default 500; 0 disables.
   */
  slowInteractionMs?: number;
}

/** Slowness thresholds in ms, resolved from the options. 0 means off. */
export interface SlowThresholds {
  request: number;
  pageLoad: number;
  interaction: number;
}

/** Counters the soak test and the SDK's own tests assert against. */
export interface SensorStats {
  signals: number;
  intents: number;
  distinctFingerprints: number;
  pendingWrites: number;
  droppedSignals: number;
  overflowedFingerprints: number;
}

export class Sensor {
  private readonly signals: RingBuffer<CapturedSignal>;
  private readonly intents: RingBuffer<Intent>;
  private readonly counters: CounterTable;
  private readonly readback: ReadbackTracker;
  /** Null when the sensor is inert (disabled, or no usable DSN). */
  private readonly transport: Transport | null;
  private readonly redaction: RedactionConfig;
  private readonly now: () => number;
  private readonly salt: string;
  /** The ingest endpoint and recent evidence URLs, as origin + path. Requests
   *  to these are the sensor's own and are never recorded (loop guard). */
  private readonly endpointKey: string;
  private readonly evidenceKeys: string[] = [];
  private networkCaptures = 0;
  private consoleMuted = 0;
  /** Read by the capture modules; fixed for the life of the sensor. */
  readonly slow: SlowThresholds;
  /**
   * False for an inert sensor: `enabled: false`, an invalid DSN, or no
   * destination at all. Every method still works and does nothing, so an app
   * never needs to check before calling one.
   */
  readonly enabled: boolean;

  private currentIntentId = '';
  private user: Identity | null = null;
  private userKey = '';
  private readonly sessionId: string;
  private stopAutoFlush: Array<() => void> = [];
  private hideHooks: Array<() => void> = [];

  constructor(opts: SensorOptions) {
    this.now = opts.now ?? (() => Date.now());
    this.redaction = opts.redaction ?? DEFAULT_REDACTION;
    this.salt = newSessionSalt();
    const conn = connectionFor(opts);
    this.endpointKey = conn ? bareUrl(conn.endpoint) : '';
    this.slow = {
      request: threshold(opts.slowRequestMs, 3000),
      pageLoad: threshold(opts.slowPageLoadMs, 4000),
      interaction: threshold(opts.slowInteractionMs, 500),
    };

    // One session id for the life of the page. Every action this user takes
    // shares it, which is what lets a scout see a whole session rather than
    // one disconnected request.
    this.sessionId = newId();

    this.signals = new RingBuffer<CapturedSignal>(BUDGET.maxSignals);
    this.intents = new RingBuffer<Intent>(BUDGET.maxIntents);
    this.counters = new CounterTable(BUDGET.maxCountsPerFlush);
    this.readback = new ReadbackTracker(BUDGET.maxPendingVerifications);

    // Pre-DSN options still throw from here on an insecure endpoint, exactly
    // as before; a DSN was already checked by connectionFor and cannot.
    this.transport = conn
      ? new Transport({
        endpoint: conn.endpoint,
        tenant: opts.tenant ?? '',
        ingestKey: conn.key,
        release: opts.release ?? 'unknown',
        sensorId: opts.sensorId ?? randomId(),
        fetchImpl: opts.fetchImpl,
        now: this.now,
      })
      : null;
    this.enabled = this.transport !== null;

    if (this.enabled && opts.autoFlush !== false) {
      const ms = opts.flushIntervalMs;
      this.startAutoFlush(typeof ms === 'number' && ms > 0 && isFinite(ms) ? ms : BUDGET.flushIntervalMs);
    }
  }

  /**
   * The flush loop: a timer, plus a flush when the page is hidden or unloaded,
   * which is the last moment a count recorded on this page can leave it.
   *
   * The hide-time flush works because everything up to the network call is
   * synchronous: the request (sent with `keepalive`, see Transport) has started
   * before the handler returns, so the browser lets it outlive the page.
   *
   * Safe outside a browser: with no `document` or window-level
   * `addEventListener`, only the timer is installed, and the timer is unref'd
   * where the runtime supports it so it never keeps a process (or a test
   * runner) alive on its own.
   */
  private startAutoFlush(intervalMs: number): void {
    safeVoid(() => {
      const flushNow = (): void => {
        this.flush().catch(() => undefined);
      };
      // The last flush a page gets: anything that only settles at hide (a
      // page's LCP) is recorded first, so it leaves with this flush.
      const flushOnHide = (): void => {
        for (const fn of this.hideHooks.slice()) safeVoid(fn);
        flushNow();
      };

      if (typeof setInterval === 'function') {
        const handle = setInterval(flushNow, intervalMs);
        (handle as unknown as { unref?: () => void }).unref?.();
        this.stopAutoFlush.push(() => clearInterval(handle));
      }

      const g = globalThis as unknown as {
        document?: Pick<Document, 'addEventListener' | 'removeEventListener' | 'visibilityState'>;
        addEventListener?: (type: string, fn: () => void) => void;
        removeEventListener?: (type: string, fn: () => void) => void;
      };

      const doc = g.document;
      if (doc && typeof doc.addEventListener === 'function') {
        const onVisibility = (): void => {
          if (doc.visibilityState === 'hidden') flushOnHide();
        };
        doc.addEventListener('visibilitychange', onVisibility);
        this.stopAutoFlush.push(() => doc.removeEventListener('visibilitychange', onVisibility));
      }

      if (typeof g.addEventListener === 'function') {
        g.addEventListener('pagehide', flushOnHide);
        this.stopAutoFlush.push(() => g.removeEventListener?.('pagehide', flushOnHide));
      }
    });
  }

  /** Stops the automatic flush loop and its page-lifecycle listeners. */
  stop(): void {
    const fns = this.stopAutoFlush;
    this.stopAutoFlush = [];
    for (const fn of fns) {
      try {
        fn();
      } catch {
        // Already gone is as good as removed.
      }
    }
  }

  /**
   * Runs `fn` when the page is hidden, just before the hide-time flush.
   * Returns a function that unregisters it.
   */
  onHide(fn: () => void): () => void {
    this.hideHooks.push(fn);
    return () => {
      const i = this.hideHooks.indexOf(fn);
      if (i >= 0) this.hideHooks.splice(i, 1);
    };
  }

  /** Begins a semantic user action; subsequent signals and requests correlate to it. */
  beginIntent(action: string, target: string): string {
    return safe(() => {
      const id = newId();
      this.currentIntentId = id;
      this.intents.push({ id, action, target, at: this.now() });
      return id;
    }, '');
  }

  /**
   * The correlation id for the current action, at hop 0.
   *
   * Null before any intent has begun: a request that belongs to no user action
   * is not worth correlating, and inventing an intent for it would make the
   * server's index noisier rather than more useful.
   */
  currentTrace(): Trace | null {
    return safe(() => {
      if (!this.currentIntentId) return null;
      return { session: this.sessionId, intent: this.currentIntentId, hop: 0 };
    }, null);
  }

  /**
   * The header to stamp on an outbound request, or null.
   *
   *   const h = sensor.traceHeader();
   *   if (h) req.headers[h.name] = h.value;
   */
  traceHeader(): { name: string; value: string } | null {
    const trace = this.currentTrace();
    return trace ? { name: TRACE_HEADER, value: formatTrace(trace) } : null;
  }

  /** The session id, stable for the life of the page. */
  session(): string {
    return this.sessionId;
  }

  /**
   * Sets who is using the page, or clears it with `null`
   * (SERVER_SDK_SPEC.md, "Capability 5 — Identity").
   *
   *   sensor.setUser({ email: 'pat@example.com', account: 'acct-1182' });
   *
   * `email` is trimmed and lowercased, `account` is the app's own customer id
   * as a string; each is optional and truncated to 255 characters. Identity is
   * attached to counts in its own field and nowhere else: it never enters a
   * fingerprint, a message or an evidence bundle, and emails inside messages
   * are still redacted as before.
   */
  setUser(user: { email?: string | null; account?: string | number | null } | null | undefined): void {
    safeVoid(() => {
      this.user = null;
      this.userKey = '';
      if (!user) return;

      const id: Identity = {};
      if (typeof user.email === 'string') {
        const email = user.email.trim().toLowerCase().slice(0, MAX_IDENTITY_CHARS);
        if (email) id.email = email;
      }
      if (user.account !== null && user.account !== undefined) {
        const account = String(user.account).slice(0, MAX_IDENTITY_CHARS);
        if (account) id.account = account;
      }
      if (id.email === undefined && id.account === undefined) return;

      this.user = id;
      this.userKey = `${id.email ?? ''}\u0000${id.account ?? ''}`;
    });
  }

  /** Records a signal and counts its fingerprint. */
  record(kind: SignalKind, type: string, message: string, frames?: Frame[]): void {
    safeVoid(() => {
      if (!this.transport) return;
      const at = this.now();
      const clean = redactText(message);

      this.signals.push({ kind, type, message: clean, frames, intentId: this.currentIntentId, at });

      const { fp } = compute({ kind, source: 'client', type, message: clean, frames });
      // Identity goes to the counter only, after the fingerprint is computed
      // from the redacted message: it is never an input to either.
      this.counters.record(fp, kind, Math.floor(at / 1000), this.user, this.userKey);
    });
  }

  /**
   * Records an HTTP exchange.
   *
   * A mutation that reported success is remembered for readback; a read is
   * checked against anything pending. A divergence is recorded as a
   * `silent_failure` — the product's whole thesis, detected with no error
   * having been raised anywhere.
   */
  recordHttp(rec: HttpRecord, resourceKey: string, body?: Record<string, unknown>): void {
    safeVoid(() => {
      if (!this.transport) return;
      const isWrite = rec.method !== 'GET' && rec.method !== 'HEAD';

      if (isWrite) {
        this.readback.recordWrite(rec, resourceKey);
        return;
      }

      if (!body) return;

      const comparable: Record<string, unknown> = {};
      for (const [name, value] of Object.entries(body)) {
        comparable[name] = fieldValue(name, String(value ?? ''), this.redaction, this.salt);
      }

      for (const d of this.readback.checkRead(resourceKey, comparable)) {
        this.record(
          'silent_failure',
          'write_readback_divergence',
          `${rec.method} ${d.urlTemplate} reported success but ${d.field} did not change`,
        );
      }
    });
  }

  /** Convenience: templates the URL before recording. */
  recordRequest(method: string, url: string, status: number, opts: {
    writtenValues?: Record<string, string>;
    body?: Record<string, unknown>;
    /**
     * handled is the `x-adt-handled` count from the response, if the server
     * SDK is installed and the header was readable.
     *
     * This is detector 2. The browser already knows whether the application
     * told the user anything; it cannot know whether the server swallowed an
     * exception getting to that answer. The server SDK supplies exactly that
     * one number, and the two facts meet here — in the one place that holds
     * both at the same moment.
     */
    handled?: number;
    /**
     * The outcome (an HTTP error, or the handled-header silent failure) is
     * recorded by the global fetch/XHR capture instead; only readback is done
     * here. Set by the AngularJS adapter, whose $http runs on XHR, so one
     * failed request is one count rather than two.
     */
    outcomeRecordedElsewhere?: boolean;
  } = {}): void {
    safeVoid(() => {
      // Written values pass through the allow-list before they are held at all.
      // A field that is not allow-listed is kept as shape (<len:12>, <empty>),
      // which is all the detector needs: a divergence is a change in shape too,
      // and we never hold the value in the first place (proposal 3.5).
      let written: Record<string, string> | undefined;
      if (opts.writtenValues) {
        written = {};
        for (const [name, value] of Object.entries(opts.writtenValues)) {
          written[name] = fieldValue(name, value, this.redaction, this.salt);
        }
      }

      const rec: HttpRecord = {
        intentId: this.currentIntentId,
        method: method.toUpperCase(),
        urlTemplate: redactUrl(url),
        status,
        ok: status >= 200 && status < 400,
        at: this.now(),
        writtenValues: written,
      };

      const outcome = !opts.outcomeRecordedElsewhere;

      if (outcome && !rec.ok && status >= 400) {
        this.record('error', `http_${status}`, `${rec.method} ${rec.urlTemplate} failed with ${status}`);
      }

      // The canonical silent failure: the server rescued something and
      // answered success anyway.
      //
      // Only on a response the application treated as successful. A handled
      // failure on a 4xx or 5xx is a server that caught a problem and then
      // correctly told the user — which is the system working, not a bug, and
      // reporting it would spend the customer's trust on noise.
      if (outcome && rec.ok && (opts.handled ?? 0) > 0) {
        rec.handled = opts.handled;
        this.record(
          'silent_failure',
          'handled_without_feedback',
          `${rec.method} ${rec.urlTemplate} returned ${status} after the server handled a failure`,
        );
      }

      this.recordHttp(rec, resourceKeyOf(url), opts.body);
    });
  }

  /** Records a request that never got a response: offline, DNS, CORS, reset. */
  recordNetworkFailure(method: string, url: string): void {
    safeVoid(() => {
      this.record('error', 'network_error', `${method.toUpperCase()} ${redactUrl(url)} failed with network error`);
    });
  }

  /**
   * Records a request that succeeded, but slowly.
   *
   * The message carries the method and URL template and nothing else — no
   * duration. Every occurrence of a slow endpoint must be one fingerprint, so
   * that "GET /api/deals/<id> was slow" is counted (how often, how many users)
   * rather than split by how slow it happened to be each time. The fingerprint
   * template would turn a number into <dur> anyway; not relying on that keeps
   * the message stable by construction.
   */
  recordSlowRequest(method: string, url: string): void {
    safeVoid(() => {
      this.record('degradation', 'slow_request', `${method.toUpperCase()} ${redactUrl(url)} was slow`);
    });
  }

  /**
   * True for a request the sensor itself makes: the ingest endpoint or an
   * evidence upload URL. Global capture must never record these — a failing
   * ingest would otherwise record a failure, flush it, fail, and record again.
   */
  isOwnRequest(url: string): boolean {
    return safe(() => {
      const key = bareUrl(url);
      return key === this.endpointKey || this.evidenceKeys.indexOf(key) >= 0;
    }, true);
  }

  /**
   * Runs `fn` with console.error capture muted, rethrowing whatever it throws.
   *
   * For the AngularJS $exceptionHandler decorator, which records the exception
   * itself and then calls the app's handler, whose default logs the same
   * exception through `$log.error` -> `console.error`. Without this one
   * AngularJS exception would be two counts with two fingerprints.
   */
  muteConsole(fn: () => void): void {
    this.consoleMuted++;
    try {
      fn();
    } finally {
      this.consoleMuted--;
    }
  }

  /** True while `muteConsole` is running; read by console.error capture. */
  isConsoleMuted(): boolean {
    return this.consoleMuted > 0;
  }

  /** Whether global fetch/XHR capture is installed for this sensor. */
  capturesNetwork(): boolean {
    return this.networkCaptures > 0;
  }

  /** Called by installNetworkCapture on install (true) and teardown (false). */
  setNetworkCapture(on: boolean): void {
    this.networkCaptures = Math.max(0, this.networkCaptures + (on ? 1 : -1));
  }

  /**
   * Phase 1. Sends counts and handles any evidence the server asks for.
   *
   * Never throws: a flush failure is invisible to the host app by design.
   */
  async flush(): Promise<void> {
    const transport = this.transport;
    if (!transport || this.counters.size === 0) return;

    try {
      const { counts, overflowed } = this.counters.drain();
      const dropped = this.signals.droppedCount;

      const res = await transport.flush(counts, { overflowed, dropped });

      for (const req of res.need_evidence ?? []) {
        // Remembered before the upload starts, so global capture sees it as
        // ours. Bounded: only the most recent few are kept.
        this.evidenceKeys.push(bareUrl(req.url));
        if (this.evidenceKeys.length > MAX_EVIDENCE_KEYS) this.evidenceKeys.shift();
        await transport.uploadEvidence(req, this.evidenceBundle(req.fp));
      }
    } catch {
      // Dropped, by design: see the failure behaviour in the wire protocol.
    }
  }

  /**
   * Phase 2 payload: the buffered context for one fingerprint.
   *
   * Built only when the server asks, which is why this work is not on the hot
   * path and why a known problem costs nothing but a counter increment.
   */
  evidenceBundle(fp: string): Record<string, unknown> {
    return {
      v: 1,
      fp,
      // The session id, so the server can rebuild a trace key.
      //
      // The sidecar indexes the customer's server logs by `<session>/<intent>`
      // (Trace.TraceKey), and every record in this bundle already carries its
      // intentId — but not the session, which is held here and nowhere else in
      // the payload. Without this one field the two halves of a problem cannot
      // be joined: we would have the browser's account of what happened and
      // the server's, and no way to tell which lines belong to which failure.
      session: this.sessionId,
      intents: this.intents.toArray(),
      signals: this.signals.toArray(),
      unverified_writes: this.readback.unverified().map((w) => ({
        intentId: w.intentId,
        urlTemplate: w.urlTemplate,
        fields: Object.keys(w.fields),
        at: w.at,
      })),
    };
  }

  stats(): SensorStats {
    return {
      signals: this.signals.length,
      intents: this.intents.length,
      distinctFingerprints: this.counters.size,
      pendingWrites: this.readback.pendingCount,
      droppedSignals: this.signals.droppedCount,
      overflowedFingerprints: this.counters.overflowCount,
    };
  }

  /** Releases buffered references. Called on unload. */
  clear(): void {
    safeVoid(() => {
      this.signals.clear();
      this.intents.clear();
    });
  }
}

/** Stable key for "the same resource", independent of query string. */
function resourceKeyOf(url: string): string {
  const scheme = url.indexOf('://');
  let path = url;
  if (scheme >= 0) {
    const rest = url.slice(scheme + 3);
    const slash = rest.indexOf('/');
    path = slash >= 0 ? rest.slice(slash) : '/';
  }
  const q = path.search(/[?#]/);
  return q >= 0 ? path.slice(0, q) : path;
}

const MAX_IDENTITY_CHARS = 255;

/**
 * Where to send, or null for an inert sensor. A DSN wins over the pre-DSN
 * `endpoint` + `ingestKey`, which keep working for existing installs.
 */
function connectionFor(opts: SensorOptions): { endpoint: string; key: string } | null {
  if (opts.enabled === false) return null;
  if (opts.dsn !== undefined) {
    const d = parseDsn(opts.dsn);
    if (typeof d === 'string') {
      warn(`invalid DSN (${d}); reporting is disabled.`);
      return null;
    }
    return { endpoint: d.endpoint, key: d.key };
  }
  if (opts.endpoint === undefined) {
    warn('no dsn given; reporting is disabled.');
    return null;
  }
  return { endpoint: opts.endpoint, key: opts.ingestKey ?? '' };
}

/** A threshold option: a finite number >= 0 (0 disables), else the default. */
function threshold(v: number | undefined, def: number): number {
  return typeof v === 'number' && v >= 0 && isFinite(v) ? v : def;
}
const MAX_EVIDENCE_KEYS = 16;

/** Origin + path of a URL, resolved against the page; the query is dropped
 *  (presigned URLs differ only there). */
function bareUrl(raw: string): string {
  try {
    const base = (globalThis as { location?: { href?: string } }).location?.href;
    const u = base ? new URL(raw, base) : new URL(raw);
    return u.origin + u.pathname;
  } catch {
    const q = raw.search(/[?#]/);
    return q >= 0 ? raw.slice(0, q) : raw;
  }
}

function randomId(): string {
  return `s${Math.random().toString(36).slice(2, 10)}${Date.now().toString(36)}`;
}
