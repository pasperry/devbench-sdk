/**
 * The two-phase reporting protocol (ARCHITECTURE_PROPOSAL.md 2).
 *
 * Phase 1: flush fingerprint counts. Always. Tiny, and it means the server sees
 * every distinct problem, so a new one can never be silently lost.
 *
 * Phase 2: upload an evidence bundle, but only for the fingerprints the server
 * asks for, and directly to object storage via a presigned URL — evidence bytes
 * never pass through ADT's API servers.
 */
import { BUDGET } from './budget.js';
import type { Count } from './counters.js';
import { readSessionToken, SESSION_HEADER } from './trace.js';

/**
 * Phase 1 carries counts and signal class only — no message text, no templates,
 * no stacks.
 *
 * The tradeoff, stated because it is a real one: a fingerprint whose sensor is
 * asked for evidence but never delivers it (tab closed, crash) has no
 * human-readable form on the server until another sensor is asked. Sending the
 * message template on every flush would fix that, at the cost of putting
 * content on every request from every browser — which is the property that
 * keeps this endpoint cheap and keeps user data off it. Counts keep accruing
 * either way, so nothing is lost but detail, and only temporarily.
 */
export interface FlushRequest {
  v: 1;
  tenant: string;
  source: 'client';
  release: string;
  sensor_id: string;
  counts: Count[];
  /** Counters refused because the table was full; reported so loss is visible. */
  overflowed?: number;
  /** Buffered signals dropped because a ring buffer was full. */
  dropped?: number;
}

export interface EvidenceRequest {
  fp: string;
  claim: string;
  url: string;
  expires: number;
}

export interface FlushResponse {
  need_evidence?: EvidenceRequest[];
}

export interface TransportOptions {
  endpoint: string;
  tenant: string;
  /**
   * The tenant's public, write-only ingest key.
   *
   * Public by construction — it ships in a browser bundle, so it cannot be a
   * real secret. It identifies a tenant and grants no read access, so a leaked
   * key means junk counts, not disclosure. Sent as a header rather than in the
   * URL so it never reaches proxy logs or a Referer.
   */
  ingestKey: string;
  release: string;
  sensorId: string;
  /** Injected so tests exercise the real code path against a real server rather
   *  than asserting against a stub of ourselves. */
  fetchImpl?: typeof fetch;
  now?: () => number;
  /**
   * Overrides the session token. Normally left unset — the token is read from
   * the meta tag the customer's backend injected, and the browser never mints
   * one of its own.
   */
  sessionToken?: string | null;
  /** Overridable so the split path can be exercised for real. At production
   *  settings the count cap keeps a beacon around 6.7KB, well under the 8KB
   *  budget, so the splitter is defense-in-depth against a future field being
   *  added to Count — which is exactly the kind of guard that rots untested. */
  maxBeaconBytes?: number;
}

/**
 * Rejects an endpoint that would send the ingest key over plaintext.
 *
 * Relying on the server's http→https redirect is not enough: the browser sends
 * the full request, key header and all, *before* it sees the 301. By the time
 * the redirect arrives the key has already crossed the network in the clear.
 * The only place this can be stopped is before the first request leaves.
 *
 * Loopback is exempt so local development works.
 */
export function assertSecureEndpoint(endpoint: string): void {
  let url: URL;
  try {
    url = new URL(endpoint);
  } catch {
    throw new Error(`ADT: endpoint is not a valid URL: ${endpoint}`);
  }

  if (url.protocol === 'https:') return;

  const host = url.hostname;
  const loopback = host === 'localhost' || host === '127.0.0.1' || host === '::1' || host.endsWith('.localhost');
  if (url.protocol === 'http:' && loopback) return;

  throw new Error(
    `ADT: refusing to send to ${url.protocol}//${host} — the ingest key would cross the network ` +
      `in plaintext. A server-side redirect does not help: the browser sends the key before it ` +
      `sees the redirect. Use https.`,
  );
}

export class Transport {
  private readonly fetchImpl: typeof fetch;

  constructor(private readonly opts: TransportOptions) {
    assertSecureEndpoint(opts.endpoint);
    this.fetchImpl = opts.fetchImpl ?? ((...a: Parameters<typeof fetch>) => fetch(...a));
  }

  /**
   * Sends phase-1 counts and returns any evidence the server wants.
   *
   * Never throws and never retries more than `BUDGET.maxFlushRetries`. A
   * diagnostics tool that queues unboundedly when its own backend is down is a
   * memory leak with extra steps.
   */
  async flush(counts: Count[], extra: { overflowed?: number; dropped?: number } = {}): Promise<FlushResponse> {
    if (counts.length === 0) return {};

    const limit = this.opts.maxBeaconBytes ?? BUDGET.maxBeaconBytes;
    const beacons: string[] = [];
    this.split(counts.slice(0, BUDGET.maxCountsPerFlush), extra, limit, beacons);

    // Every beacon is started before the first await, so a flush begun in a
    // pagehide handler has all of its requests in flight (keepalive) before
    // the handler returns.
    const responses = await Promise.all(beacons.map((payload) => this.send(payload)));

    const need = ([] as EvidenceRequest[]).concat(...responses.map((r) => r.need_evidence ?? []));
    return need.length ? { need_evidence: need } : {};
  }

  /**
   * Serializes `counts` into beacons no larger than `limit`, halving until each
   * fits. Every count is sent: identity on counts makes a split an ordinary
   * event rather than a theoretical one, and dropping the remainder would lose
   * fingerprints outright. A single count larger than the limit is sent alone
   * rather than recursing forever. Overflow and drop totals ride on the first
   * beacon only, so they are not double counted.
   */
  private split(counts: Count[], extra: { overflowed?: number; dropped?: number }, limit: number, out: string[]): void {
    const body: FlushRequest = {
      v: 1,
      tenant: this.opts.tenant,
      source: 'client',
      release: this.opts.release,
      sensor_id: this.opts.sensorId,
      counts,
    };
    if (out.length === 0 && extra.overflowed) body.overflowed = extra.overflowed;
    if (out.length === 0 && extra.dropped) body.dropped = extra.dropped;

    const payload = JSON.stringify(body);
    if (payload.length > limit && counts.length > 1) {
      const half = Math.floor(counts.length / 2);
      this.split(counts.slice(0, half), extra, limit, out);
      this.split(counts.slice(half), extra, limit, out);
      return;
    }
    out.push(payload);
  }

  private async send(payload: string): Promise<FlushResponse> {
    for (let attempt = 0; attempt <= BUDGET.maxFlushRetries; attempt++) {
      try {
        const headers: Record<string, string> = {
          'content-type': 'application/json',
          'x-adt-key': this.opts.ingestKey,
        };

        // Read per flush rather than once at construction: a single-page app
        // outlives many tokens, and the backend refreshes the tag on each full
        // page load.
        const session = this.opts.sessionToken !== undefined
          ? this.opts.sessionToken
          : readSessionToken();
        if (session) headers[SESSION_HEADER] = session;

        const res = await this.fetchImpl(this.opts.endpoint, {
          method: 'POST',
          headers,
          body: payload,
          keepalive: true,
          credentials: 'omit',
        });

        if (!res.ok) {
          // 4xx is our bug or a rejected tenant; retrying will not help.
          if (res.status < 500) return {};
          continue;
        }

        return (await res.json()) as FlushResponse;
      } catch {
        // Network failure. Fall through to the retry, then give up silently.
      }
    }

    return {};
  }

  /**
   * Phase 2. PUTs the bundle straight at the presigned URL.
   *
   * Returns whether it succeeded; a failure is not retried, because the server
   * will ask a different sensor for the same fingerprint next time.
   */
  async uploadEvidence(req: EvidenceRequest, bundle: unknown): Promise<boolean> {
    const now = (this.opts.now ?? Date.now)();
    if (req.expires * 1000 <= now) return false;

    try {
      const res = await this.fetchImpl(req.url, {
        method: 'PUT',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify(bundle),
        credentials: 'omit',
      });
      return res.ok;
    } catch {
      return false;
    }
  }
}
