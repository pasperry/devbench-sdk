/**
 * The widget's HTTP calls to the chat API (docs/CHAT_API.md). Every request
 * carries the public key and the signed identity and never a cookie; an error
 * becomes a `ChatError` holding the contract's `code`, which is what the UI
 * acts on.
 */

export class ChatError extends Error {
  constructor(
    readonly status: number,
    /** The contract's `code`; `network` when nothing came back, `` when unknown. */
    readonly code: string,
    message: string,
    /** Seconds from `Retry-After`, when the response let the page read it. */
    readonly retryAfter: number | null,
  ) {
    super(message);
  }
}

const GET_TIMEOUT_MS = 15_000;
/** The reply is produced in the request: "allow up to 60 s". */
const POST_TIMEOUT_MS = 75_000;

export class ChatClient {
  private readonly inflight: AbortController[] = [];

  constructor(
    private readonly api: string,
    private readonly key: string,
    private readonly token: string,
    private readonly fetchImpl: typeof fetch,
  ) {}

  async request<T>(method: 'GET' | 'POST', path: string, body?: unknown): Promise<T> {
    const ctl = typeof AbortController === 'function' ? new AbortController() : null;
    if (ctl) this.inflight.push(ctl);
    const timer = setTimeout(() => ctl?.abort(), method === 'GET' ? GET_TIMEOUT_MS : POST_TIMEOUT_MS);
    let res: Response;
    try {
      const headers: Record<string, string> = { 'X-ADT-Key': this.key, 'X-ADT-Identity': this.token };
      if (body !== undefined) headers['Content-Type'] = 'application/json';
      res = await this.fetchImpl(`${this.api}${path}`, {
        method,
        credentials: 'omit',
        cache: 'no-store',
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: ctl ? ctl.signal : undefined,
      });
    } catch {
      throw new ChatError(0, 'network', '', null);
    } finally {
      clearTimeout(timer);
      if (ctl) this.inflight.splice(this.inflight.indexOf(ctl), 1);
    }

    let parsed: unknown = null;
    try {
      parsed = await res.json();
    } catch {
      parsed = null;
    }
    if (res.ok) {
      if (parsed === null || typeof parsed !== 'object') throw new ChatError(res.status, '', '', null);
      return parsed as T;
    }
    const e = (parsed && typeof parsed === 'object' ? parsed : {}) as { code?: unknown; error?: unknown };
    const ra = Number(res.headers.get('Retry-After'));
    throw new ChatError(
      res.status,
      typeof e.code === 'string' ? e.code : '',
      typeof e.error === 'string' ? e.error : '',
      res.headers.get('Retry-After') !== null && isFinite(ra) && ra >= 0 ? ra : null,
    );
  }

  /** Cancels every request in flight (the widget is going away). */
  abortAll(): void {
    for (const c of this.inflight.splice(0)) c.abort();
  }
}
