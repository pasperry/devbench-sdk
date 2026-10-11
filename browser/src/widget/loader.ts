/**
 * The chat widget loader: the only chat code in the core bundle
 * (SERVER_SDK_SPEC.md, "Capability 6 — Chat widget" › Loading).
 *
 * No token → nothing at all. With one, after the page's `load` (and an idle
 * callback where there is one) it asks `GET <api>/v1/chat/widget` whether this
 * user gets a widget; only a `200 {"enabled": true}` naming a bundle with an
 * SRI hash inserts that bundle, which then mounts itself with what this hands
 * it. Every other outcome is one `console.debug` and no widget, with no retry
 * in this page. The bundle, not this file, holds the UI: the core bundle's
 * budget is unchanged for every tenant without a widget.
 *
 * Role and permissions are never read here: they exist only inside the signed
 * token. This file takes the token and nothing that claims to describe the user.
 */
import type { Sensor } from '../sensor.js';
import type { Bootstrap, WidgetGlobal, WidgetHandle } from './types.js';

export const DEFAULT_API = 'https://api.developerbench.com';
const TOKEN = /^dbw1\.([A-Za-z0-9_-]+)\.[A-Za-z0-9_-]+$/;
const INTEGRITY = /^sha(256|384|512)-[A-Za-z0-9+/=]+$/;
const LOOPBACK = /^http:\/\/(localhost|127\.0\.0\.1|\[::1\]|[a-z0-9.-]+\.localhost)(:\d+)?(\/|$)/i;
const CACHE_KEY = 'devbench.chat';
const CACHE_MS = 10 * 60 * 1000;
const TIMEOUT_MS = 5000;

/** The page the loader works on: `window` in a browser. */
export interface LoaderScope {
  document?: Document;
  fetch?: typeof fetch;
  sessionStorage?: Storage;
  requestIdleCallback?: (cb: () => void, opts?: { timeout: number }) => number;
  addEventListener?: (type: string, fn: () => void) => void;
  removeEventListener?: (type: string, fn: () => void) => void;
  AbortController?: typeof AbortController;
  DevbenchWidget?: WidgetGlobal;
}

/** `https://…` or loopback `http://…`, without a trailing slash; else ''. */
export function apiBase(raw: unknown): string {
  if (raw === undefined || raw === null || raw === '') return DEFAULT_API;
  if (typeof raw !== 'string') return '';
  const v = raw.trim().replace(/\/+$/, '');
  return /^https:\/\/[^/?#\s@]+$/i.test(v) || (LOOPBACK.test(v + '/') && /^http:\/\/[^/?#\s@]+$/i.test(v)) ? v : '';
}

function secureUrl(v: unknown): v is string {
  return typeof v === 'string' && (/^https:\/\/[^\s]+$/i.test(v) || LOOPBACK.test(v));
}

/**
 * The cache key: the public key plus the payload's fields after `issued_at`
 * (email, account, role, chat). The signer stamps `issued_at` at every render,
 * so the whole payload would never repeat across page views; the rest is
 * exactly "this user, with this role and permission".
 */
function identityKey(key: string, payload: string): string {
  try {
    const bin = atob(payload.replace(/-/g, '+').replace(/_/g, '/'));
    const nl = bin.indexOf('\n');
    return key + '\n' + (nl >= 0 ? bin.slice(nl + 1) : bin);
  } catch {
    return key + '\n' + payload;
  }
}

function debug(message: string): void {
  try {
    const c = (globalThis as { console?: { debug?: (...a: unknown[]) => void } }).console;
    if (c && typeof c.debug === 'function') c.debug(`Devbench: chat widget not loaded (${message}).`);
  } catch {
    // A console that throws is not ours to fix.
  }
}

export class WidgetLoader {
  private gen = 0;
  private handle: WidgetHandle | null = null;
  private readonly api: string;

  constructor(
    private readonly scope: LoaderScope,
    private readonly sensor: Sensor,
    private readonly key: string,
    api: unknown,
    private readonly nonce: string,
  ) {
    this.api = apiBase(api);
    if (this.api) sensor.addOwnPrefix(`${this.api}/v1/chat/`);
  }

  /** A token starts (or replaces) the widget; null or '' removes it. */
  set(token: unknown): void {
    try {
      const gen = ++this.gen;
      this.unmount();
      if (token === null || token === undefined || token === '') return;
      const m = typeof token === 'string' ? TOKEN.exec(token.trim()) : null;
      if (!m) return debug('the widget identity is not a dbw1 token');
      if (!this.api) return debug('the api option is not an https URL');
      if (!this.key) return debug('no public key');
      const t = (token as string).trim();
      const cacheKey = identityKey(this.key, m[1] ?? '');
      this.afterLoad(() => {
        if (gen === this.gen) void this.run(gen, t, cacheKey);
      });
    } catch {
      // Never the page's problem.
    }
  }

  close(): void {
    this.gen++;
    this.unmount();
  }

  private unmount(): void {
    const h = this.handle;
    this.handle = null;
    if (h) {
      try {
        h.destroy();
      } catch {
        // Already gone.
      }
    }
  }

  /** After the page's `load`, then in idle time where the browser has it. */
  private afterLoad(fn: () => void): void {
    const s = this.scope;
    const idle = (): void => {
      if (typeof s.requestIdleCallback === 'function') s.requestIdleCallback(fn, { timeout: TIMEOUT_MS });
      else setTimeout(fn, 0);
    };
    if (!s.document || s.document.readyState === 'complete' || typeof s.addEventListener !== 'function') return idle();
    const onLoad = (): void => {
      s.removeEventListener?.('load', onLoad);
      idle();
    };
    s.addEventListener('load', onLoad);
  }

  private async run(gen: number, token: string, cacheKey: string): Promise<void> {
    try {
      let body = this.cached(cacheKey);
      if (body === undefined) {
        const answer = await this.fetchBootstrap(token);
        if (typeof answer === 'string') return debug(answer);
        body = answer;
        this.store(cacheKey, body);
      }
      if (gen !== this.gen) return;
      const b = body as Partial<Bootstrap> | null;
      if (!b || b.enabled !== true) return debug('the service answered enabled: false');
      const bundle = b.bundle;
      if (!bundle || !secureUrl(bundle.url) || typeof bundle.integrity !== 'string' || !INTEGRITY.test(bundle.integrity)) {
        return debug('the bootstrap named no https bundle with an integrity hash');
      }
      this.insert(gen, { ...(b as Bootstrap), bundle }, token);
    } catch {
      debug('internal error');
    }
  }

  private async fetchBootstrap(token: string): Promise<unknown | string> {
    const f = this.scope.fetch;
    if (typeof f !== 'function') return 'no fetch';
    const AC = this.scope.AbortController;
    const ctl = typeof AC === 'function' ? new AC() : null;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const timeout = new Promise<string>((resolve) => {
      timer = setTimeout(() => {
        ctl?.abort();
        resolve('timed out');
      }, TIMEOUT_MS);
    });
    const request = (async (): Promise<unknown | string> => {
      const res = await f.call(this.scope, `${this.api}/v1/chat/widget`, {
        method: 'GET',
        credentials: 'omit',
        cache: 'no-store',
        headers: { 'X-ADT-Key': this.key, 'X-ADT-Identity': token },
        signal: ctl ? ctl.signal : undefined,
      });
      if (res.status !== 200) return `HTTP ${res.status}`;
      return (await res.json()) as unknown;
    })().catch(() => 'network error');
    try {
      return await Promise.race([request, timeout]);
    } finally {
      clearTimeout(timer);
    }
  }

  private cached(cacheKey: string): unknown | undefined {
    try {
      const raw = this.scope.sessionStorage?.getItem(CACHE_KEY);
      if (!raw) return undefined;
      const c = JSON.parse(raw) as { k?: string; at?: number; body?: unknown };
      const age = Date.now() - (c.at ?? 0);
      return c.k === cacheKey && age >= 0 && age < CACHE_MS ? c.body : undefined;
    } catch {
      return undefined;
    }
  }

  /** One slot: a tab has one signed-in user at a time. */
  private store(cacheKey: string, body: unknown): void {
    try {
      this.scope.sessionStorage?.setItem(CACHE_KEY, JSON.stringify({ k: cacheKey, at: Date.now(), body }));
    } catch {
      // Full or blocked storage: the next page asks again.
    }
  }

  private insert(gen: number, bootstrap: Bootstrap, token: string): void {
    const mount = (): void => {
      if (gen !== this.gen) return;
      const w = this.scope.DevbenchWidget;
      if (!w || typeof w.mount !== 'function') return debug('the bundle did not define DevbenchWidget');
      try {
        this.handle = w.mount({ bootstrap, key: this.key, token, api: this.api, session: this.sensor.session(), nonce: this.nonce || undefined });
      } catch {
        debug('the widget failed to start');
      }
    };
    // Already loaded in this page (a token was replaced): mount again.
    if (this.scope.DevbenchWidget) return mount();
    const doc = this.scope.document;
    if (!doc) return;
    const el = doc.createElement('script');
    el.src = bootstrap.bundle.url;
    el.integrity = bootstrap.bundle.integrity;
    el.crossOrigin = 'anonymous';
    el.async = true;
    if (this.nonce) el.nonce = this.nonce;
    el.addEventListener('load', mount);
    el.addEventListener('error', () => debug('the bundle failed to load or its integrity did not match'));
    (doc.head || doc.documentElement).appendChild(el);
  }
}
