/**
 * The browser soak's world (test/soak.browser.test.ts): an app origin that
 * serves a page with the shipped script-tag bundle and a small JSON API, and
 * an ingest on another origin that takes the sensor's flushes, so the
 * sensor's own requests go through a real CORS preflight like production.
 *
 * Both are real HTTP servers. Neither keeps what it received beyond a few
 * running totals: a soak whose own harness grows would hide nothing in the
 * page, but it would make the nightly slower the longer it runs.
 */
import { readFileSync } from 'node:fs';
import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import type { AddressInfo } from 'node:net';

export const SOAK_KEY = 'adt_client_soak';

/** A ~1 KiB customer record: a hook that retains requests retains these. */
function customer(id: string): string {
  return JSON.stringify({ id, name: `Customer ${id}`, email: `c${id}@example.com`, notes: 'x'.repeat(900) });
}

export interface AppSite {
  url: string;
  /** API requests the app made that reached the server. */
  apiRequests(): number;
  close(): Promise<void>;
}

/** The app: `/` is `html`, `/devbench.min.js` the built bundle, `/api/*` the API. */
export async function startAppSite(html: string): Promise<AppSite> {
  const bundle = readFileSync('dist/devbench.min.js');
  let api = 0;
  const server = createServer((req, res) => {
    drain(req, () => {
      const path = (req.url ?? '').split('?')[0] ?? '';
      if (path === '/') return send(res, 200, html, { 'content-type': 'text/html; charset=utf-8' });
      if (path === '/devbench.min.js') return send(res, 200, bundle, { 'content-type': 'text/javascript' });
      if (path.startsWith('/api/')) {
        api++;
        const id = path.split('/').pop() ?? '0';
        // A server error with a body, as a real 500 page has one.
        if (path.startsWith('/api/fail/')) return send(res, 500, JSON.stringify({ error: 'boom', detail: 'y'.repeat(900) }), json);
        // A 200 that says the server handled a failure the user was not told about.
        if (path.startsWith('/api/handled/')) return send(res, 200, customer(id), { ...json, 'x-adt-handled': '1' });
        return send(res, 200, customer(id), json);
      }
      send(res, 404, '', {});
    });
  });
  const url = await listen(server);
  return { url, apiRequests: () => api, close: () => shut(server) };
}

export interface FlushTotals {
  /** POST /v1/flush requests received. */
  flushes: number;
  /** Sum of every count's `n`. */
  occurrences: number;
  /** Signal classes seen (`kind`). */
  kinds: Set<string>;
  /** Flushes with a missing or wrong X-ADT-Key. */
  badKey: number;
}

export interface Ingest {
  url: string;
  totals: FlushTotals;
  close(): Promise<void>;
}

/** The ingest: answers the preflight and `POST /v1/flush` with `{}`. */
export async function startIngest(): Promise<Ingest> {
  const totals: FlushTotals = { flushes: 0, occurrences: 0, kinds: new Set(), badKey: 0 };
  const cors = {
    'access-control-allow-origin': '*',
    'access-control-allow-methods': 'POST, OPTIONS',
    'access-control-allow-headers': 'content-type, x-adt-key, x-adt-session',
    'access-control-max-age': '600',
  };
  const server = createServer((req, res) => {
    const body: Buffer[] = [];
    req.on('data', (c: Buffer) => body.push(c));
    req.on('end', () => {
      if (req.method === 'OPTIONS') return send(res, 204, '', cors);
      if (req.method !== 'POST' || req.url !== '/v1/flush') return send(res, 404, '', cors);
      totals.flushes++;
      if (req.headers['x-adt-key'] !== SOAK_KEY) totals.badKey++;
      const parsed = JSON.parse(Buffer.concat(body).toString('utf8')) as { counts?: Array<{ n: number; kind: string }> };
      for (const c of parsed.counts ?? []) {
        totals.occurrences += c.n;
        totals.kinds.add(c.kind);
      }
      send(res, 200, '{}', { ...cors, ...json });
    });
  });
  const url = await listen(server);
  return { url, totals, close: () => shut(server) };
}

/** An origin nobody listens on: requests to it fail without a response. */
export async function deadOrigin(): Promise<string> {
  const server = createServer();
  const url = await listen(server);
  await shut(server);
  return url;
}

const json = { 'content-type': 'application/json', 'cache-control': 'no-store' };

function drain(req: IncomingMessage, then: () => void): void {
  req.resume();
  req.on('end', then);
}

function send(res: ServerResponse, status: number, body: string | Buffer, headers: Record<string, string>): void {
  res.writeHead(status, headers);
  res.end(body);
}

async function listen(server: Server): Promise<string> {
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  return `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
}

function shut(server: Server): Promise<void> {
  return new Promise<void>((resolve) => {
    server.closeAllConnections();
    server.close(() => resolve());
  });
}
