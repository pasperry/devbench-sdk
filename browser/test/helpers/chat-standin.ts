/**
 * A local stand-in for the chat service, built from docs/CHAT_API.md, because
 * the real one (#69) has not shipped. It is a real HTTP server on its own
 * origin, so the browser does real CORS preflights against it, and it checks
 * what the contract says the service checks: the public key, a dbw1 token
 * verified with HMAC-SHA256 (and expiry), no unknown body fields, the
 * per-user conversation ownership, 409 after a handoff, and the error shape.
 *
 * GAP: this is not the service. The real routes, their rate limits, the AI
 * and the handoff delivery are #69's, and must be rechecked against it.
 *
 * It also serves the built widget bundle (with ACAO: *, as a CDN does), and
 * answers /v1/flush so the sensor's own flushes land somewhere.
 */
import { createHash, createHmac, timingSafeEqual } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import type { AddressInfo } from 'node:net';

export const CLIENT_KEY = 'adt_client_chat';
export const SECRET = 'dbws_k7x2m4q9r3t6v8w1y5z0a2b4c6d8e0f1g3h5j7k9m2n4p6q8';
export const WIDGET_FILE = 'dist/devbench-widget.min.js';

export interface Seen {
  method: string;
  path: string;
  headers: Record<string, string | string[] | undefined>;
  body: string;
  /** When the request arrived (Date.now()). */
  at: number;
}

export interface Canned {
  status: number;
  body?: unknown;
  headers?: Record<string, string>;
  delayMs?: number;
}

interface Msg {
  id: number;
  role: string;
  text: string;
  links: Array<{ title: string; url: string }>;
  created_at: string;
}

interface Conv {
  id: number;
  owner: string;
  state: string;
  messages: Msg[];
  clientIds: Map<string, unknown>;
  handed_off_at: string | null;
}

export interface StandIn {
  url: string;
  seen: Seen[];
  /** Chat requests only (not the preflights, the flushes or the bundle). */
  chat(): Seen[];
  /** What GET /v1/chat/widget answers when the token is valid. */
  bootstrap: () => Canned;
  /** One-shot answers for the next request to a route ("POST /v1/chat/conversations/1/messages"). */
  next: Map<string, Canned>;
  /** From now on every chat call is 401 identity_expired (the token aged out). */
  expireAll: boolean;
  /** The assistant's reply to a user message. */
  reply: (text: string) => Partial<Msg>;
  conversations: Map<number, Conv>;
  bundleUrl: string;
  integrity: string;
  close(): Promise<void>;
}

const b64u = (b: Buffer): string => b.toString('base64url');

/** Signs per docs/server-sdk/chat-widget.md Capability 6 (checked against the shared vectors in the tests). */
export function signToken(
  user: { email?: string; account?: string; role?: string; chat?: string },
  issuedAt = Math.floor(Date.now() / 1000),
  secret = SECRET,
): string {
  const payload = b64u(Buffer.from([String(issuedAt), user.email ?? '', user.account ?? '', user.role ?? '', user.chat ?? ''].join('\n'), 'utf8'));
  const mac = b64u(createHmac('sha256', Buffer.from(secret, 'utf8')).update(`dbw1.${payload}`).digest());
  return `dbw1.${payload}.${mac}`;
}

type Verdict = { ok: true; owner: string } | { ok: false; code: 'identity_invalid' | 'identity_expired' };

function verify(token: string | undefined): Verdict {
  const parts = (token ?? '').split('.');
  if (parts.length !== 3 || parts[0] !== 'dbw1') return { ok: false, code: 'identity_invalid' };
  const want = b64u(createHmac('sha256', Buffer.from(SECRET, 'utf8')).update(`dbw1.${parts[1]}`).digest());
  const got = Buffer.from(parts[2] ?? '');
  if (got.length !== want.length || !timingSafeEqual(got, Buffer.from(want))) return { ok: false, code: 'identity_invalid' };
  const fields = Buffer.from(parts[1] ?? '', 'base64url').toString('utf8').split('\n');
  if (fields.length !== 5) return { ok: false, code: 'identity_invalid' };
  const age = Date.now() / 1000 - Number(fields[0]);
  if (age < -300) return { ok: false, code: 'identity_invalid' };
  if (age > 24 * 3600) return { ok: false, code: 'identity_expired' };
  return { ok: true, owner: `${(fields[1] ?? '').toLowerCase()}\n${fields[2]}` };
}

const ERRORS: Record<string, [number, string]> = {
  invalid_request: [400, 'The request is not valid.'],
  key_invalid: [401, 'This page is not set up for chat.'],
  identity_invalid: [401, 'Sign in again to chat.'],
  identity_expired: [401, 'Reload the page to keep chatting.'],
  unavailable: [403, 'Chat is not available.'],
  not_found: [404, 'Not found.'],
  conversation_closed: [409, 'This conversation is closed. Start a new one.'],
};

export async function startChatStandIn(): Promise<StandIn> {
  const seen: Seen[] = [];
  const conversations = new Map<number, Conv>();
  let nextConv = 1;
  let nextMsg = 1;
  const bundle = readFileSync(WIDGET_FILE);
  const integrity = `sha384-${createHash('sha384').update(bundle).digest('base64')}`;
  const now = (): string => new Date().toISOString().replace(/\.\d+Z$/, 'Z');

  const state: Omit<StandIn, 'url' | 'bundleUrl' | 'close' | 'chat'> & { url: string } = {
    url: '',
    seen,
    integrity,
    conversations,
    expireAll: false,
    next: new Map(),
    reply: (text) => ({ text: `You said: ${text}` }),
    bootstrap: () => ({
      status: 200,
      body: {
        enabled: true, name: 'Help', color: '#1d76db', position: 'bottom-right',
        capabilities: ['handoff', 'howto', 'report', 'status'],
        bundle: { url: `${state.url}/bundle/devbench-widget.min.js`, integrity },
        resume: null,
      },
    }),
  };

  const convJson = (c: Conv): Record<string, unknown> => ({ id: c.id, state: c.state, created_at: now(), updated_at: now(), handed_off_at: c.handed_off_at });
  const msg = (role: string, m: Partial<Msg>): Msg => ({ id: nextMsg++, role, text: m.text ?? '', links: m.links ?? [], created_at: now() });

  const server = createServer((req: IncomingMessage, res: ServerResponse) => {
    const chunks: Buffer[] = [];
    req.on('data', (c: Buffer) => chunks.push(c));
    req.on('end', () => {
      const path = (req.url ?? '').split('?')[0] ?? '';
      const entry: Seen = { method: req.method ?? '', path, headers: req.headers, body: Buffer.concat(chunks).toString('utf8'), at: Date.now() };
      seen.push(entry);

      const cors: Record<string, string> = req.headers.origin ? { 'Access-Control-Allow-Origin': '*', Vary: 'Origin' } : {};
      const send = (c: Canned): void => {
        const go = (): void => {
          if (res.destroyed) return;
          res.writeHead(c.status, { 'content-type': 'application/json', 'cache-control': 'no-store', ...cors, ...c.headers });
          res.end(c.body === undefined ? '' : JSON.stringify(c.body));
        };
        if (c.delayMs) setTimeout(go, c.delayMs);
        else go();
      };
      const fail = (code: string, headers?: Record<string, string>): void => {
        const [status, error] = ERRORS[code] ?? [500, 'error'];
        send({ status, body: { error, code }, headers });
      };

      if (path.startsWith('/bundle/')) {
        const body = path.endsWith('/tampered.js') ? Buffer.concat([bundle, Buffer.from('\n;window.TAMPERED=1;')]) : bundle;
        res.writeHead(200, { 'content-type': 'text/javascript', 'Access-Control-Allow-Origin': '*' });
        res.end(body);
        return;
      }
      if (req.method === 'OPTIONS' && path === '/v1/flush') {
        res.writeHead(204, { ...cors, 'Access-Control-Allow-Methods': 'POST, OPTIONS', 'Access-Control-Allow-Headers': 'content-type, x-adt-key' });
        res.end();
        return;
      }
      if (path === '/v1/flush') return send({ status: 200, body: {} });
      if (!path.startsWith('/v1/chat/')) return send({ status: 404, body: {} });

      if (req.method === 'OPTIONS') {
        res.writeHead(204, {
          ...cors,
          'Access-Control-Allow-Methods': 'GET, POST, OPTIONS',
          'Access-Control-Allow-Headers': 'content-type, x-adt-key, x-adt-identity',
          'Access-Control-Max-Age': '86400',
        });
        res.end();
        return;
      }

      const canned = state.next.get(`${req.method} ${path}`);
      if (canned) {
        state.next.delete(`${req.method} ${path}`);
        return send(canned);
      }

      // Order of checks (CHAT_API.md "Errors"): key → identity → the route.
      if (req.headers['x-adt-key'] !== CLIENT_KEY) return fail('key_invalid');
      if (state.expireAll) return fail('identity_expired');
      const v = verify(req.headers['x-adt-identity'] as string | undefined);
      if (!v.ok) return fail(v.code);

      if (req.method === 'GET' && path === '/v1/chat/widget') return send(state.bootstrap());

      let body: Record<string, unknown> = {};
      if (req.method === 'POST' && entry.body) {
        try {
          body = JSON.parse(entry.body) as Record<string, unknown>;
        } catch {
          return fail('invalid_request');
        }
        for (const k of Object.keys(body)) if (!['text', 'client_id', 'context'].includes(k)) return fail('invalid_request');
      }

      const own = (id: string): Conv | null => {
        const c = conversations.get(Number(id));
        return c && c.owner === v.owner ? c : null;
      };
      const post = (c: Conv): unknown => {
        const text = typeof body.text === 'string' ? body.text.trim() : '';
        if (text.length < 1 || text.length > 4000) return null;
        const cid = typeof body.client_id === 'string' ? body.client_id : '';
        if (cid && c.clientIds.has(cid)) return c.clientIds.get(cid);
        const out = [msg('user', { text }), msg('assistant', state.reply(text))];
        c.messages.push(...out);
        const answer = { conversation: convJson(c), messages: out };
        if (cid) c.clientIds.set(cid, answer);
        return answer;
      };

      let m: RegExpExecArray | null;
      if (path === '/v1/chat/conversations' && req.method === 'POST') {
        const c: Conv = { id: nextConv++, owner: v.owner, state: 'open', messages: [], clientIds: new Map(), handed_off_at: null };
        const answer = post(c);
        if (!answer) return fail('invalid_request');
        conversations.set(c.id, c);
        return send({ status: 201, body: answer });
      }
      if ((m = /^\/v1\/chat\/conversations\/(\d+)$/.exec(path)) && req.method === 'GET') {
        const c = own(m[1]!);
        if (!c) return fail('not_found');
        return send({ status: 200, body: { conversation: convJson(c), messages: c.messages } });
      }
      if ((m = /^\/v1\/chat\/conversations\/(\d+)\/messages$/.exec(path)) && req.method === 'POST') {
        const c = own(m[1]!);
        if (!c) return fail('not_found');
        if (c.state !== 'open') return fail('conversation_closed');
        const answer = post(c);
        return answer ? send({ status: 200, body: answer }) : fail('invalid_request');
      }
      if ((m = /^\/v1\/chat\/conversations\/(\d+)\/handoff$/.exec(path)) && req.method === 'POST') {
        const c = own(m[1]!);
        if (!c) return fail('not_found');
        if (c.state === 'handed_off') return send({ status: 200, body: { conversation: convJson(c), messages: [] } });
        c.state = 'handed_off';
        c.handed_off_at = now();
        const notice = msg('notice', { text: "Thanks. Acme Shop's support team will follow up by email." });
        c.messages.push(notice);
        return send({ status: 200, body: { conversation: convJson(c), messages: [notice] } });
      }
      return fail('not_found');
    });
  });

  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const { port } = server.address() as AddressInfo;
  state.url = `http://127.0.0.1:${port}`;

  return Object.assign(state, {
    bundleUrl: `${state.url}/bundle/devbench-widget.min.js`,
    chat: () => seen.filter((s) => s.path.startsWith('/v1/chat/') && s.method !== 'OPTIONS'),
    close: () => new Promise<void>((resolve) => {
      server.closeAllConnections();
      server.close(() => resolve());
    }),
  });
}

/** Serves the test pages (and the built core bundle) from a second origin. */
export interface PageServer {
  url: string;
  pages: Map<string, { html: string; headers?: Record<string, string> }>;
  /** When /slow.gif (held 800 ms, so the page's load event waits) was answered. */
  slowDoneAt: number;
  close(): Promise<void>;
}

const GIF = Buffer.from('R0lGODlhAQABAAAAACw=', 'base64');

export async function startPageServer(): Promise<PageServer> {
  const pages = new Map<string, { html: string; headers?: Record<string, string> }>();
  const out = { url: '', pages, slowDoneAt: 0, close: async () => undefined } as PageServer;
  const server = createServer((req, res) => {
    const path = (req.url ?? '').split('?')[0] ?? '';
    if (path === '/slow.gif') {
      setTimeout(() => {
        out.slowDoneAt = Date.now();
        res.writeHead(200, { 'content-type': 'image/gif', 'cache-control': 'no-store' });
        res.end(GIF);
      }, 800);
      return;
    }
    if (path === '/devbench.min.js') {
      res.writeHead(200, { 'content-type': 'text/javascript' });
      res.end(readFileSync('dist/devbench.min.js'));
      return;
    }
    const p = pages.get(path);
    if (!p) {
      res.writeHead(404);
      res.end();
      return;
    }
    // A first-party cookie: the chat calls must never carry it (credentials: 'omit').
    res.writeHead(200, { 'content-type': 'text/html; charset=utf-8', 'set-cookie': 'app_session=s3cret; Path=/', ...p.headers });
    res.end(p.html);
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const { port } = server.address() as AddressInfo;
  out.url = `http://127.0.0.1:${port}`;
  out.close = () => new Promise<void>((resolve) => {
    server.closeAllConnections();
    server.close(() => resolve());
  });
  return out;
}
