/**
 * A minimal XMLHttpRequest for Node, which has none.
 *
 * It stands in for the browser platform, not for the code under test: it makes
 * real HTTP requests (via the platform fetch, captured at import so a test that
 * patches a fetch cannot reach it) and reports real statuses and headers, with
 * the event order a browser uses — load/error/abort, then loadend.
 *
 * Each test gets its own subclass from `xhrClass()`, so prototype patching in
 * one test cannot leak into another.
 */
const platformFetch = globalThis.fetch.bind(globalThis);

export interface TestXhr extends EventTarget {
  status: number;
  readyState: number;
  responseText: string;
  onload: (() => void) | null;
  onerror: (() => void) | null;
  /** How many arguments the most recent open() received. */
  openArgCount: number;
  open(method: string, url: string, async?: boolean): void;
  send(body?: unknown): void;
  abort(): void;
  getAllResponseHeaders(): string;
  getResponseHeader(name: string): string | null;
}

export function xhrClass(): { new (): TestXhr; prototype: TestXhr } {
  class Xhr extends EventTarget {
    status = 0;
    readyState = 0;
    responseText = '';
    onload: (() => void) | null = null;
    onerror: (() => void) | null = null;
    openArgCount = 0;
    private method = 'GET';
    private url = '';
    private headers = '';
    private controller: AbortController | null = null;
    private done = false;

    open(method: string, url: string): void {
      // eslint-disable-next-line prefer-rest-params
      this.openArgCount = arguments.length;
      this.method = method;
      this.url = url;
      this.readyState = 1;
      this.status = 0;
      this.done = false;
    }

    send(body?: unknown): void {
      this.controller = new AbortController();
      platformFetch(this.url, { method: this.method, body: body as string | undefined, signal: this.controller.signal })
        .then(async (res) => {
          const text = await res.text();
          if (this.done) return;
          this.status = res.status;
          this.responseText = text;
          const lines: string[] = [];
          res.headers.forEach((v, k) => lines.push(`${k}: ${v}`));
          this.headers = lines.join('\r\n');
          this.finish('load');
        })
        .catch(() => {
          if (this.done) return;
          this.status = 0;
          this.finish('error');
        });
    }

    abort(): void {
      if (this.done || !this.controller) return;
      this.controller.abort();
      this.status = 0;
      this.finish('abort');
    }

    getAllResponseHeaders(): string {
      return this.headers;
    }

    getResponseHeader(name: string): string | null {
      const m = new RegExp(`(?:^|\\n)${name}:\\s*([^\\r\\n]*)`, 'i').exec(this.headers);
      return m ? m[1]! : null;
    }

    private finish(kind: 'load' | 'error' | 'abort'): void {
      this.done = true;
      this.readyState = 4;
      if (kind === 'load') this.onload?.();
      if (kind === 'error') this.onerror?.();
      this.dispatchEvent(new Event(kind));
      this.dispatchEvent(new Event('loadend'));
    }
  }
  return Xhr as unknown as { new (): TestXhr; prototype: TestXhr };
}
