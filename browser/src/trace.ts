/**
 * The correlation id that ties a user action together across every service.
 *
 * Wire form (docs/SERVER_SDK_SPEC.md): `v1/<session>/<intent>/<hop>`
 *
 * The browser originates it at hop 0 and each service increments. Without it,
 * client evidence cannot be joined to server logs and both detectors in §3.2
 * degrade to guesswork — silently, which is why this is risk #9.
 *
 * Deliberately not W3C `traceparent`: that carries no session or intent, and
 * intent is the thing intent-versus-outcome analysis is about. Where a
 * traceparent already exists it is recorded alongside, never relied on.
 */

const ID_ALPHABET = 'abcdefghijklmnopqrstuvwxyz0123456789';
const ID_LENGTH = 12;

// One scratch buffer, reused. An id is generated per user action, and a
// fresh Uint8Array each time measurably raises the allocation rate inside the
// host app for no benefit — the bytes are consumed immediately and never
// retained. Safe because JavaScript is single-threaded here: nothing can
// interleave between fill and read.
const idScratch = new Uint8Array(ID_LENGTH);

/** Generates an opaque id matching the spec's [A-Za-z0-9_-]{1,64}. */
export function newId(): string {
  // crypto.getRandomValues where available; Math.random is an acceptable
  // fallback because this is a correlation id, not a secret — guessing one
  // reveals nothing and grants nothing.
  const cryptoObj = typeof globalThis !== 'undefined' ? (globalThis as { crypto?: Crypto }).crypto : undefined;

  let out = '';
  if (cryptoObj?.getRandomValues) {
    cryptoObj.getRandomValues(idScratch);
    for (let i = 0; i < ID_LENGTH; i++) out += ID_ALPHABET[idScratch[i]! % ID_ALPHABET.length];
    return out;
  }

  for (let i = 0; i < ID_LENGTH; i++) {
    out += ID_ALPHABET[Math.floor(Math.random() * ID_ALPHABET.length)];
  }
  return out;
}

/** The header name. */
export const TRACE_HEADER = 'x-adt-trace';

export interface Trace {
  session: string;
  intent: string;
  hop: number;
}

/** Renders the wire form. */
export function formatTrace(t: Trace): string {
  return `v1/${t.session}/${t.intent}/${t.hop}`;
}

/** Parses a wire value, returning null rather than throwing. */
export function parseTrace(value: string): Trace | null {
  const m = /^v1\/([A-Za-z0-9_-]{1,64})\/([A-Za-z0-9_-]{1,64})\/(\d{1,3})$/.exec(value.trim());
  if (!m) return null;

  const hop = Number(m[3]);
  if (!Number.isInteger(hop) || hop < 0 || hop > 99) return null;

  return { session: m[1]!, intent: m[2]!, hop };
}

/** The header carrying a server-minted session token. */
export const SESSION_HEADER = 'x-adt-session';

/** The meta tag the server SDK injects. */
export const SESSION_META = 'adt-session';

/**
 * Reads the session token the customer's backend put in the page.
 *
 * The ingest key is public — it ships in this bundle. A session token proves
 * the page was served by the customer's real application, which is what
 * separates a genuine sensor from a bot posting to the endpoint directly. The
 * browser never mints one; it only carries what the server put there.
 *
 * Absent is normal and not an error: a tenant that has not enabled session
 * tokens works exactly as before.
 */
export function readSessionToken(doc?: Document): string | null {
  try {
    const d = doc ?? (typeof document !== 'undefined' ? document : undefined);
    if (!d) return null;

    const tag = d.querySelector(`meta[name="${SESSION_META}"]`);
    const value = tag?.getAttribute('content')?.trim();
    return value ? value : null;
  } catch {
    return null;
  }
}

/**
 * The header the server SDK sets when it handled a failure while producing a
 * response.
 *
 * Detector 2's whole wire surface: one count, no detail. The class name,
 * message and rescue site travel to the sidecar over a unix socket on the
 * customer's own host, never to the browser — this header is readable by any
 * script on the page.
 */
export const HANDLED_HEADER = 'x-adt-handled';

/**
 * Reads the handled count from a response, returning 0 when absent.
 *
 * Returns 0 rather than throwing for every reason it can fail, because all of
 * them are ordinary: the server SDK may not be installed, the header may not
 * be exposed cross-origin, and some HTTP clients hand back a headers object
 * that does not implement `get`.
 *
 * **Cross-origin:** a browser cannot read a custom response header unless the
 * server lists it in `Access-Control-Expose-Headers`. An Angular app on a
 * different origin from its Rails API — a common setup — reads nothing without
 * that. The server SDK adds it automatically; a hand-rolled integration must.
 */
export function readHandled(headers: unknown): number {
  try {
    if (!headers) return 0;

    let raw: string | null = null;

    if (typeof (headers as Headers).get === 'function') {
      raw = (headers as Headers).get(HANDLED_HEADER);
    } else if (typeof headers === 'object') {
      // A plain object, as AngularJS's $http and several test doubles hand
      // back. Header names are case-insensitive, so this cannot assume.
      for (const [name, value] of Object.entries(headers as Record<string, unknown>)) {
        if (name.toLowerCase() === HANDLED_HEADER) {
          raw = String(value);
          break;
        }
      }
    }

    if (raw === null || raw === '') return 0;

    const n = Number(raw);
    // A non-numeric or negative value is a header we did not set, or one a
    // proxy mangled. Treating it as zero is right: inventing a silent failure
    // from a malformed header would spend trust on noise.
    return Number.isFinite(n) && n > 0 ? Math.floor(n) : 0;
  } catch {
    return 0;
  }
}
