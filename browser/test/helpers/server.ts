import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import type { AddressInfo } from 'node:net';

export interface Received {
  method: string;
  url: string;
  headers: Record<string, string | string[] | undefined>;
  body: string;
}

export type Handler = (req: Received, n: number) =>
  {
    status: number;
    body: string;
    headers?: Record<string, string>;
    /** Hold the response this long before answering: a real slow server. */
    delayMs?: number;
  } | undefined;

export interface TestServer {
  url: string;
  received: Received[];
  /** Per-path handler. Returning undefined falls through to 200 {}. */
  handle: Map<string, Handler>;
  close(): Promise<void>;
}

/**
 * A real HTTP server.
 *
 * The transport talks to an API route we own, so it is tested with real HTTP
 * against a real server (CLAUDE.md, "The Mock Permitted List"). Stubbing fetch
 * would test that our stub returns what we told it to.
 */
export async function startTestServer(): Promise<TestServer> {
  const received: Received[] = [];
  const handle = new Map<string, Handler>();
  const hits = new Map<string, number>();

  const server: Server = createServer((req: IncomingMessage, res: ServerResponse) => {
    const chunks: Buffer[] = [];
    req.on('data', (c: Buffer) => chunks.push(c));
    req.on('end', () => {
      const entry: Received = {
        method: req.method ?? '',
        url: req.url ?? '',
        headers: req.headers,
        body: Buffer.concat(chunks).toString('utf8'),
      };
      received.push(entry);

      const path = (req.url ?? '').split('?')[0] ?? '';
      const n = (hits.get(path) ?? 0) + 1;
      hits.set(path, n);

      const fn = handle.get(path);
      const out = fn ? fn(entry, n) : undefined;

      const answer = (): void => {
        res.writeHead(out?.status ?? 200, { 'content-type': 'application/json', ...out?.headers });
        res.end(out?.body ?? '{}');
      };
      if (out?.delayMs) setTimeout(answer, out.delayMs);
      else answer();
    });
  });

  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const { port } = server.address() as AddressInfo;

  return {
    url: `http://127.0.0.1:${port}`,
    received,
    handle,
    close: () => new Promise<void>((resolve) => server.close(() => resolve())),
  };
}
