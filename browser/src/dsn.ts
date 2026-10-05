/**
 * The DSN: one string per environment that carries the ingest base and key
 * (SERVER_SDK_SPEC.md, "Dev Bench naming and configuration").
 *
 *   https://<ingest key>@adt-ingest.onrender.com
 *
 * `scheme://host[:port]` is the ingest base and the flush endpoint is
 * `<base>/v1/flush`; the userinfo is the key, sent as `X-ADT-Key`. A path or
 * query, if present, is ignored. A browser DSN is public by design: the key
 * is write-only.
 *
 * Parsed with a regular expression rather than `URL`, so a bad DSN is an
 * answer ("this is why") rather than an exception, and so parsing does not
 * depend on a platform API.
 */

export interface Dsn {
  /** The ingest key from the userinfo part. */
  key: string;
  /** `scheme://host[:port]`, lowercased. */
  base: string;
  /** `<base>/v1/flush`. */
  endpoint: string;
}

const SHAPE = /^(https?):\/\/([^@/?#\s]+)@([^/?#@\s]+)(?:[/?#]\S*)?$/i;
const HOST = /^(\[[0-9a-f:.]+\]|[a-z0-9]([a-z0-9.-]*[a-z0-9])?)(?::(\d{1,5}))?$/;

/** Parses a DSN. Returns the parts, or a sentence saying what is wrong. */
export function parseDsn(raw: unknown): Dsn | string {
  if (typeof raw !== 'string' || raw.trim() === '') return 'the DSN is empty';

  const m = SHAPE.exec(raw.trim());
  if (!m) return 'expected https://<key>@host[:port]';

  // A server DSN is https://<public>:<secret>@host (DECISIONS #161). Its
  // secret must never reach a page: refuse it outright rather than use the
  // public half, so the mistake is seen and the secret gets rotated.
  const userinfo = m[2] ?? '';
  if (userinfo.includes(':')) {
    return 'this is a server DSN: it contains a secret key and must not be used in a browser. ' +
      'Use the public form https://<public key>@host (the Rails helper renders it), and rotate this DSN if it was ever shipped to a page';
  }

  let key: string;
  try {
    key = decodeURIComponent(userinfo);
  } catch {
    return 'the key is not valid percent-encoding';
  }
  if (key === '') return 'the key is empty';

  const scheme = (m[1] ?? '').toLowerCase();
  const host = (m[3] ?? '').toLowerCase();
  const h = HOST.exec(host);
  if (!h) return `"${host}" is not a host[:port]`;
  if (h[3] !== undefined && (Number(h[3]) < 1 || Number(h[3]) > 65535)) return `port ${h[3]} is out of range`;

  // Same rule as Transport.assertSecureEndpoint, checked here so a bad DSN
  // disables the sensor instead of throwing from its constructor.
  const hostname = h[1] ?? '';
  const loopback = hostname === 'localhost' || hostname === '127.0.0.1' || hostname === '[::1]' || /\.localhost$/.test(hostname);
  if (scheme === 'http' && !loopback) {
    return 'http would send the key in plaintext; use https (http is allowed only for localhost)';
  }

  const base = `${scheme}://${host}`;
  return { key, base, endpoint: `${base}/v1/flush` };
}

/** One console.warn, feature-detected: the only way the SDK talks to a developer. */
export function warn(message: string): void {
  try {
    const c = (globalThis as { console?: { warn?: (...a: unknown[]) => void } }).console;
    if (c && typeof c.warn === 'function') c.warn(`Devbench: ${message}`);
  } catch {
    // A console that throws is not ours to fix.
  }
}
