/**
 * The chat widget in a real browser (Chrome, via playwright-core): the
 * loader in the shipped core bundle (dist/devbench.min.js) and the shipped
 * widget bundle (dist/devbench-widget.min.js), on a page served from one
 * origin, talking to a chat API on another, so CORS, SRI, CSP and
 * credentials are the browser's real behaviour, not a model of it.
 *
 * GAP: the chat API here is a local stand-in built from docs/CHAT_API.md
 * (test/helpers/chat-standin.ts) because the real service is #69. The
 * loader/widget contract is proved; the service's own behaviour is not.
 *
 * Needs Google Chrome (the GitHub ubuntu runner has it). CHROME_PATH points
 * at another Chrome/Chromium binary. Never skipped: no browser is a failure.
 */
import { after, before, test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { chromium, type Browser, type BrowserContext, type Page } from 'playwright-core';
import { CLIENT_KEY, signToken, startChatStandIn, startPageServer, type PageServer, type StandIn } from './helpers/chat-standin.js';

let browser: Browser;

// DEVBENCH_TESTDATA: the public SDK repo's CI, whose layout differs.
const here = dirname(fileURLToPath(import.meta.url));
const VECTORS = resolve(process.env.DEVBENCH_TESTDATA ?? resolve(here, '../../../testdata'), 'widget_identity_vectors.json');

before(async () => {
  const path = process.env.CHROME_PATH;
  browser = await chromium.launch(path ? { executablePath: path } : { channel: 'chrome' });
});
after(async () => {
  await browser?.close();
});

interface Env {
  api: StandIn;
  site: PageServer;
  ctx: BrowserContext;
  debugs: string[];
}

async function env(t: { after(fn: () => Promise<void> | void): void }): Promise<Env> {
  const api = await startChatStandIn();
  const site = await startPageServer();
  const ctx = await browser.newContext();
  const e: Env = { api, site, ctx, debugs: [] };
  t.after(async () => {
    await ctx.close();
    await api.close();
    await site.close();
  });
  return e;
}

function dsn(e: Env): string {
  return e.api.url.replace('http://', `http://${CLIENT_KEY}@`);
}

const esc = (s: string): string => s.replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/</g, '&lt;');

/**
 * A page with the core bundle's tag (`tag` attributes) and, optionally, an
 * inline script after it. `slow` holds the load event ~800 ms.
 */
async function open(e: Env, o: { tag?: Record<string, string>; inline?: string; headers?: Record<string, string>; path?: string; slow?: boolean } = {}): Promise<Page> {
  const path = o.path ?? '/';
  const attrs = Object.entries(o.tag ?? {}).map(([k, v]) => ` ${k}="${esc(v)}"`).join('');
  e.site.pages.set(path, {
    headers: o.headers,
    html: `<!doctype html><html><head><meta charset="utf-8"><title>app</title></head><body><h1>The app</h1>
${o.slow ? '<img src="/slow.gif" alt="">' : ''}
<script src="/devbench.min.js"${attrs}></script>
${o.inline ? `<script nonce="n0nce">${o.inline}</script>` : ''}
</body></html>`,
  });
  const page = await e.ctx.newPage();
  page.on('console', (m) => {
    if (m.type() === 'debug' && m.text().startsWith('Devbench:')) e.debugs.push(m.text());
  });
  await page.goto(e.site.url + path);
  return page;
}

/** Load, then an idle callback, then a margin for anything the loader started. */
async function settle(page: Page, ms = 800): Promise<void> {
  await page.waitForLoadState('load');
  await page.evaluate(() => new Promise<void>((r) => requestIdleCallback(() => r())));
  await page.waitForTimeout(ms);
}

async function waitDebug(e: Env, n = 1): Promise<void> {
  for (let i = 0; i < 160 && e.debugs.length < n; i++) await new Promise((r) => setTimeout(r, 50));
  assert.ok(e.debugs.length >= n, `expected ${n} console.debug from the loader, saw ${JSON.stringify(e.debugs)}`);
}

const launcher = (page: Page) => page.locator('devbench-chat button.launcher');
const widgetScripts = (page: Page) => page.evaluate(() => Array.from(document.querySelectorAll('script')).filter((s) => s.src.indexOf('/bundle/') >= 0).length);
const bootstraps = (e: Env) => e.api.chat().filter((s) => s.path === '/v1/chat/widget');

function token(user: { email?: string; account?: string; role?: string; chat?: string } = { email: 'pat@dealer.com', account: 'dealer-1182' }, issuedAt?: number): string {
  return signToken(user, issuedAt);
}

async function send(page: Page, text: string): Promise<void> {
  await page.locator('devbench-chat textarea').fill(text);
  await page.locator('devbench-chat form button').click();
}

// ---------------------------------------------------------------- stand-in

test('the stand-in signs dbw1 tokens exactly as the shared vectors do', () => {
  const v = (JSON.parse(readFileSync(VECTORS, 'utf8')) as {
    sign: Array<{ secret: string; email: string; account: string; role: string; chat: string[] | null; issued_at: number; token: string }>;
  }).sign[0]!;
  assert.equal(signToken({ email: v.email, account: v.account, role: v.role, chat: (v.chat ?? []).join(',') }, v.issued_at, v.secret), v.token);
});

// ---------------------------------------------------------------- loader

test('no token: the SDK makes no chat request at all', async (t) => {
  const e = await env(t);
  const page = await open(e, { tag: { 'data-dsn': dsn(e), 'data-api': e.api.url } });
  await settle(page, 1200);
  assert.deepEqual(e.api.chat(), []);
  assert.equal(await widgetScripts(page), 0);
  assert.deepEqual(e.debugs, [], 'no token is not worth a console line');
});

test('a token on the tag: after load, one bootstrap with the key and token, no cookie; SRI, crossorigin and nonce on the inserted script; the widget mounts', async (t) => {
  const e = await env(t);
  const tok = token();
  const page = await open(e, { slow: true, tag: { 'data-dsn': dsn(e), 'data-api': e.api.url, 'data-widget-identity': tok, nonce: 'n0nce' } });
  await launcher(page).waitFor();

  const b = bootstraps(e);
  assert.equal(b.length, 1);
  assert.equal(b[0]!.method, 'GET');
  assert.equal(b[0]!.headers['x-adt-key'], CLIENT_KEY);
  assert.equal(b[0]!.headers['x-adt-identity'], tok);
  assert.equal(b[0]!.headers.cookie, undefined, 'credentials must be omitted');
  assert.equal(b[0]!.headers.origin, e.site.url);
  assert.ok(e.site.slowDoneAt > 0 && b[0]!.at >= e.site.slowDoneAt, 'the bootstrap must wait for the page load event');

  const s = await page.evaluate(() => {
    const el = Array.from(document.querySelectorAll('script')).find((x) => x.src.indexOf('/bundle/') >= 0)!;
    return { integrity: el.integrity, crossOrigin: el.crossOrigin, nonce: el.nonce, src: el.src };
  });
  assert.equal(s.integrity, e.api.integrity);
  assert.equal(s.crossOrigin, 'anonymous');
  assert.equal(s.nonce, 'n0nce');
  assert.equal(s.src, e.api.bundleUrl);
  assert.deepEqual(e.debugs, []);
});

test('under a strict CSP (script-src nonce only, style-src none) the nonce lets the bundle in and the widget is styled', async (t) => {
  const e = await env(t);
  const page = await open(e, {
    headers: { 'content-security-policy': `default-src 'none'; script-src 'nonce-n0nce'; connect-src ${e.api.url}; style-src 'none'; img-src 'self'` },
    tag: { 'data-dsn': dsn(e), 'data-api': e.api.url, 'data-widget-identity': token(), nonce: 'n0nce' },
  });
  await launcher(page).waitFor();
  assert.equal(await launcher(page).evaluate((b) => getComputedStyle(b).position), 'fixed');
});

test('enabled false, 401, 403, 503, bad JSON, a bundle without SRI or over plain http, a dead API: no script, one console.debug each', async (t) => {
  const e = await env(t);
  const cases: Array<[string, () => { status: number; body?: unknown }]> = [
    ['enabled false', () => ({ status: 200, body: { enabled: false } })],
    ['401', () => ({ status: 401, body: { error: 'x', code: 'identity_invalid' } })],
    ['403', () => ({ status: 403, body: { error: 'x', code: 'origin_not_allowed' } })],
    ['503', () => ({ status: 503, body: { error: 'down' } })],
    ['not JSON', () => ({ status: 200, body: undefined })],
    ['no integrity', () => ({ status: 200, body: { enabled: true, capabilities: ['report'], bundle: { url: e.api.bundleUrl, integrity: '' } } })],
    ['plain http bundle', () => ({ status: 200, body: { enabled: true, capabilities: ['report'], bundle: { url: 'http://cdn.example.com/w.js', integrity: e.api.integrity } } })],
    ['enabled "true" as a string', () => ({ status: 200, body: { enabled: 'true', capabilities: ['report'], bundle: { url: e.api.bundleUrl, integrity: e.api.integrity } } })],
  ];
  for (const [name, answer] of cases) {
    e.api.bootstrap = answer;
    e.debugs.length = 0;
    const page = await open(e, { path: `/${encodeURIComponent(name)}`, tag: { 'data-dsn': dsn(e), 'data-api': e.api.url, 'data-widget-identity': token() } });
    await waitDebug(e);
    await settle(page, 300);
    assert.equal(await widgetScripts(page), 0, name);
    assert.equal(await page.locator('devbench-chat').count(), 0, name);
    assert.equal(e.debugs.length, 1, `${name}: ${JSON.stringify(e.debugs)}`);
    await page.close();
  }

  e.debugs.length = 0;
  const before = e.api.chat().length;
  const dead = await open(e, { path: '/dead', tag: { 'data-dsn': dsn(e), 'data-api': 'http://127.0.0.1:9', 'data-widget-identity': token() } });
  await waitDebug(e);
  assert.equal(await widgetScripts(dead), 0);
  assert.equal(e.debugs.length, 1);
  assert.equal(e.api.chat().length, before);
});

test('a bootstrap slower than 5 s: no widget, no retry, one console.debug', async (t) => {
  const e = await env(t);
  const ok = e.api.bootstrap;
  e.api.bootstrap = () => ({ ...ok(), delayMs: 6500 });
  const page = await open(e, { tag: { 'data-dsn': dsn(e), 'data-api': e.api.url, 'data-widget-identity': token() } });
  await waitDebug(e);
  assert.match(e.debugs[0]!, /timed out/);
  await page.waitForTimeout(2500); // the slow answer has now arrived, and must change nothing
  assert.equal(await widgetScripts(page), 0);
  assert.equal(bootstraps(e).length, 1, 'no retry');
  assert.equal(e.debugs.length, 1);
});

test('a bundle whose bytes do not match its integrity never runs', async (t) => {
  const e = await env(t);
  const ok = e.api.bootstrap;
  e.api.bootstrap = () => {
    const b = ok();
    (b.body as { bundle: { url: string } }).bundle.url = `${e.api.url}/bundle/tampered.js`;
    return b;
  };
  const page = await open(e, { tag: { 'data-dsn': dsn(e), 'data-api': e.api.url, 'data-widget-identity': token() } });
  await waitDebug(e);
  assert.equal(await widgetScripts(page), 1, 'the script is inserted, with its integrity');
  assert.equal(await page.evaluate(() => (window as unknown as { TAMPERED?: number }).TAMPERED), undefined);
  assert.equal(await page.locator('devbench-chat').count(), 0);
});

test('enabled: false in init, with a token: no chat request', async (t) => {
  const e = await env(t);
  const page = await open(e, { inline: `Devbench.init({ dsn: ${JSON.stringify(dsn(e))}, api: ${JSON.stringify(e.api.url)}, enabled: false, widgetIdentity: ${JSON.stringify(token())} });` });
  await settle(page, 1200);
  assert.deepEqual(e.api.chat(), []);
});

test('a role or permission passed as a JS option or data-widget-role never reaches a request; only the signed token does', async (t) => {
  const e = await env(t);
  const tok = token({ email: 'pat@dealer.com', account: 'dealer-1182', role: 'viewer' });
  const page = await open(e, {
    tag: { 'data-widget-role': 'zzrole-tag' },
    inline: `Devbench.init({ dsn: ${JSON.stringify(dsn(e))}, api: ${JSON.stringify(e.api.url)}, widgetIdentity: ${JSON.stringify(tok)},
      role: 'zzrole-init', chat: ['zzchat-init'], permissions: ['zzperm-init'] });`,
  });
  await launcher(page).click();
  await send(page, 'hello');
  await page.locator('devbench-chat .msg.assistant').waitFor();

  const all = e.api.seen;
  assert.ok(all.some((s) => s.path === '/v1/chat/conversations' && s.method === 'POST'));
  for (const s of all) {
    const blob = JSON.stringify([s.path, s.headers, s.body]);
    assert.doesNotMatch(blob, /zz(role|chat|perm)/, `${s.method} ${s.path} carried an unsigned role or permission`);
    if (s.path.startsWith('/v1/chat/') && s.method !== 'OPTIONS') assert.equal(s.headers['x-adt-identity'], tok);
  }
});

test('the bootstrap is cached for the user for 10 minutes across page views, though each render re-signs the token', async (t) => {
  const e = await env(t);
  const tag = (tok: string) => ({ 'data-dsn': dsn(e), 'data-api': e.api.url, 'data-widget-identity': tok });
  const now = Math.floor(Date.now() / 1000);

  const page = await open(e, { tag: tag(token(undefined, now - 30)) });
  await launcher(page).waitFor();
  assert.equal(bootstraps(e).length, 1);

  // Same user, a later render: a new issued_at, so a new payload. Cache hit.
  e.site.pages.set('/', { html: e.site.pages.get('/')!.html.replace(/data-widget-identity="[^"]+"/, `data-widget-identity="${token(undefined, now - 5)}"`) });
  await page.reload();
  await launcher(page).waitFor();
  assert.equal(bootstraps(e).length, 1, 'a re-signed token for the same user must hit the cache');

  // The same user with another role: another answer, so another request.
  e.site.pages.set('/', { html: e.site.pages.get('/')!.html.replace(/data-widget-identity="[^"]+"/, `data-widget-identity="${token({ email: 'pat@dealer.com', account: 'dealer-1182', role: 'admin' })}"`) });
  await page.reload();
  await launcher(page).waitFor();
  assert.equal(bootstraps(e).length, 2);

  // Older than 10 minutes: asked again.
  await page.evaluate(() => {
    const c = JSON.parse(sessionStorage.getItem('devbench.chat')!) as { at: number };
    c.at -= 10 * 60 * 1000 + 1000;
    sessionStorage.setItem('devbench.chat', JSON.stringify(c));
  });
  await page.reload();
  await launcher(page).waitFor();
  assert.equal(bootstraps(e).length, 3);
});

test('setWidgetIdentity: before init it is kept; null removes the widget; a new token replaces it', async (t) => {
  const e = await env(t);
  const a = token({ email: 'pat@dealer.com', account: 'dealer-1182', role: 'a' });
  const b = token({ email: 'pat@dealer.com', account: 'dealer-1182', role: 'b' });
  const page = await open(e, {
    inline: `Devbench.setWidgetIdentity(${JSON.stringify(a)}); Devbench.init({ dsn: ${JSON.stringify(dsn(e))}, api: ${JSON.stringify(e.api.url)} });`,
  });
  await launcher(page).waitFor();
  assert.equal(bootstraps(e)[0]!.headers['x-adt-identity'], a);

  await page.evaluate(() => (window as unknown as { Devbench: { setWidgetIdentity(t: string | null): void } }).Devbench.setWidgetIdentity(null));
  assert.equal(await page.locator('devbench-chat').count(), 0, 'null must remove the widget');

  await page.evaluate((tok) => (window as unknown as { Devbench: { setWidgetIdentity(t: string | null): void } }).Devbench.setWidgetIdentity(tok), b);
  await launcher(page).waitFor();
  assert.equal(bootstraps(e).length, 2);
  assert.equal(bootstraps(e)[1]!.headers['x-adt-identity'], b);
  assert.equal(await page.locator('devbench-chat').count(), 1);
  assert.equal(await widgetScripts(page), 1, 'the loaded bundle is reused, not inserted twice');
});

test('without a token set, setWidgetIdentity(null) and a malformed token make no request', async (t) => {
  const e = await env(t);
  const page = await open(e, { inline: `Devbench.init({ dsn: ${JSON.stringify(dsn(e))}, api: ${JSON.stringify(e.api.url)} }); Devbench.setWidgetIdentity(null); Devbench.setWidgetIdentity('not-a-token');` });
  await settle(page, 1000);
  assert.deepEqual(e.api.chat(), []);
  assert.equal(e.debugs.length, 1, 'a malformed token is one console.debug');
});

// ---------------------------------------------------------------- widget

async function mounted(e: Env, caps: string[], resume: unknown = null): Promise<Page> {
  const ok = e.api.bootstrap;
  e.api.bootstrap = () => {
    const b = ok();
    Object.assign(b.body as object, { capabilities: caps, resume });
    return b;
  };
  const page = await open(e, { tag: { 'data-dsn': dsn(e), 'data-api': e.api.url, 'data-widget-identity': token() } });
  await launcher(page).click();
  return page;
}

test('messages are plain text with line breaks kept; only https links are clickable; the body carries client_id and context', async (t) => {
  const e = await env(t);
  e.api.reply = (text) => ({
    text: `Echo: ${text}`,
    links: [{ title: 'Gift cards', url: 'https://help.acme.com/gift-cards' }, { title: 'evil', url: 'javascript:alert(1)' }],
  });
  const page = await mounted(e, ['howto', 'report']);

  const labels = await page.locator('devbench-chat .suggestions button').allTextContents();
  assert.deepEqual(labels, ['How do I…?', 'Report a problem']);

  await send(page, '<img src=x onerror="window.PWNED=1"><b>bold</b>\nsecond line');
  await page.locator('devbench-chat .msg.assistant').waitFor();

  const shown = await page.locator('devbench-chat .msg.assistant').evaluate((m) => ({
    text: m.querySelector('p')!.textContent,
    elements: m.querySelectorAll('p *').length,
    whiteSpace: getComputedStyle(m).whiteSpace,
    links: Array.from(m.querySelectorAll('a')).map((a) => ({ href: a.href, text: a.textContent, rel: a.rel, target: a.target })),
  }));
  assert.equal(shown.text, 'Echo: <img src=x onerror="window.PWNED=1"><b>bold</b>\nsecond line');
  assert.equal(shown.elements, 0, 'text must never become markup');
  assert.equal(shown.whiteSpace, 'pre-wrap');
  assert.deepEqual(shown.links, [{ href: 'https://help.acme.com/gift-cards', text: 'Gift cards', rel: 'noopener noreferrer', target: '_blank' }]);
  assert.equal(await page.evaluate(() => (window as unknown as { PWNED?: number }).PWNED), undefined);

  const post = e.api.chat().find((s) => s.method === 'POST')!;
  const body = JSON.parse(post.body) as { text: string; client_id: string; context: { page_url: string; session: string } };
  assert.equal(body.text, '<img src=x onerror="window.PWNED=1"><b>bold</b>\nsecond line');
  assert.match(body.client_id, /^[0-9a-f-]{36}$/);
  assert.equal(body.context.page_url, `${e.site.url}/`);
  assert.match(body.context.session, /^[A-Za-z0-9_-]{1,128}$/);
  assert.equal(post.headers['content-type'], 'application/json');
  assert.equal(post.headers.cookie, undefined);

  assert.equal(await page.locator('devbench-chat .suggestions').isHidden(), true, 'suggestions go once the conversation starts');
  assert.equal(await page.locator('devbench-chat button.handoff').isHidden(), true, 'no handoff button without the handoff capability');
});

test('handoff: shown only with the capability and a conversation; then the conversation is closed and a new one starts fresh', async (t) => {
  const e = await env(t);
  const page = await mounted(e, ['handoff', 'report']);
  assert.equal(await page.locator('devbench-chat button.handoff').isHidden(), true, 'nothing to hand off yet');
  await send(page, 'checkout fails');
  await page.locator('devbench-chat .msg.assistant').waitFor();
  await page.locator('devbench-chat button.handoff').click();
  await page.locator('devbench-chat .msg.notice').waitFor();
  assert.match(await page.locator('devbench-chat .msg.notice').textContent() ?? '', /will follow up/);
  assert.equal(e.api.conversations.get(1)!.state, 'handed_off');
  assert.equal(await page.locator('devbench-chat textarea').isDisabled(), true);

  await page.locator('devbench-chat button.new').click();
  await send(page, 'another thing');
  await page.locator('devbench-chat .msg.assistant').waitFor();
  const posts = e.api.chat().filter((s) => s.method === 'POST').map((s) => s.path);
  assert.deepEqual(posts, ['/v1/chat/conversations', '/v1/chat/conversations/1/handoff', '/v1/chat/conversations']);
});

test('409 conversation_closed: says so and offers a new conversation', async (t) => {
  const e = await env(t);
  const page = await mounted(e, ['report']);
  await send(page, 'one');
  await page.locator('devbench-chat .msg.assistant').waitFor();
  e.api.next.set('POST /v1/chat/conversations/1/messages', { status: 409, body: { error: 'This conversation is closed. Start a new one.', code: 'conversation_closed' } });
  await send(page, 'two');
  await page.locator('devbench-chat button.new').waitFor();
  assert.equal(await page.locator('devbench-chat .status').textContent(), 'This conversation is closed. Start a new one.');
  assert.equal(await page.locator('devbench-chat textarea').inputValue(), 'two', 'the unsent text is kept');
});

test('429 rate_limited: sending pauses for Retry-After when the response exposes it, else 30 s', async (t) => {
  const e = await env(t);
  const page = await mounted(e, ['report']);
  e.api.next.set('POST /v1/chat/conversations', { status: 429, body: { error: 'Slow down.', code: 'rate_limited' }, headers: { 'Retry-After': '7' } });
  await send(page, 'hi');
  await page.locator('devbench-chat .status:text("30 seconds")').waitFor();
  assert.equal(await page.locator('devbench-chat form button').isDisabled(), true);

  // Readable only when the API exposes the header (CORS): see the PR.
  const page2 = await mounted(e, ['report']);
  e.api.next.set('POST /v1/chat/conversations', { status: 429, body: { error: 'Slow down.', code: 'rate_limited' }, headers: { 'Retry-After': '7', 'Access-Control-Expose-Headers': 'Retry-After' } });
  await send(page2, 'hi');
  await page2.locator('devbench-chat .status:text("7 seconds")').waitFor();
});

test('502 handoff_failed: the sentence is shown and the handoff can be tried again', async (t) => {
  const e = await env(t);
  const page = await mounted(e, ['handoff', 'report']);
  await send(page, 'help');
  await page.locator('devbench-chat .msg.assistant').waitFor();
  e.api.next.set('POST /v1/chat/conversations/1/handoff', { status: 502, body: { error: "We couldn't reach support. Try again.", code: 'handoff_failed' } });
  await page.locator('devbench-chat button.handoff').click();
  await page.locator('devbench-chat .status:text("We couldn\'t reach support. Try again.")').waitFor();
  assert.equal(await page.locator('devbench-chat button.handoff').isEnabled(), true);
  await page.locator('devbench-chat button.handoff').click();
  await page.locator('devbench-chat .msg.notice').waitFor();
});

test('identity_expired: "Reload the page to keep chatting" and no further calls', async (t) => {
  const e = await env(t);
  const page = await mounted(e, ['handoff', 'report']);
  await send(page, 'first');
  await page.locator('devbench-chat .msg.assistant').waitFor();
  e.api.expireAll = true;
  await send(page, 'second');
  await page.locator('devbench-chat .status:text("Reload the page to keep chatting.")').waitFor();
  const calls = e.api.chat().length;
  assert.equal(await page.locator('devbench-chat textarea').isDisabled(), true);

  // Force the controls back on, as a stale handler would: still no call.
  await page.locator('devbench-chat textarea').evaluate((x: HTMLTextAreaElement) => { x.disabled = false; x.value = 'third'; });
  await page.locator('devbench-chat textarea').press('Enter');
  await page.locator('devbench-chat button.handoff').evaluate((b: HTMLButtonElement) => { b.hidden = false; b.disabled = false; b.click(); });
  await page.waitForTimeout(500);
  assert.equal(e.api.chat().length, calls, 'the widget must stop calling once the identity expired');
});

test('a failed send keeps the text, and the retry reuses its client_id', async (t) => {
  const e = await env(t);
  const page = await mounted(e, ['report']);
  e.api.next.set('POST /v1/chat/conversations', { status: 500, body: { error: 'Something went wrong. Try again.' } });
  await send(page, 'retry me');
  await page.locator('devbench-chat .status:text("Something went wrong. Try again.")').waitFor();
  await page.locator('devbench-chat form button').click();
  await page.locator('devbench-chat .msg.assistant').waitFor();
  const ids = e.api.chat().filter((s) => s.method === 'POST').map((s) => (JSON.parse(s.body) as { client_id: string }).client_id);
  assert.equal(ids.length, 2);
  assert.equal(ids[0], ids[1]);
});

test('resume: the open conversation is shown; someone else\'s (404) starts fresh, silently', async (t) => {
  const e = await env(t);
  const tok = token();
  const create = async (tk: string, text: string): Promise<number> => {
    const r = await fetch(`${e.api.url}/v1/chat/conversations`, {
      method: 'POST', headers: { 'X-ADT-Key': CLIENT_KEY, 'X-ADT-Identity': tk, 'content-type': 'application/json' }, body: JSON.stringify({ text }),
    });
    assert.equal(r.status, 201);
    return ((await r.json()) as { conversation: { id: number } }).conversation.id;
  };
  const mine = await create(tok, 'from before');
  const page = await mounted(e, ['report'], { id: mine, state: 'open' });
  await page.locator('devbench-chat .msg.user', { hasText: 'from before' }).waitFor();
  await send(page, 'continuing');
  await page.locator('devbench-chat .msg.user', { hasText: 'continuing' }).waitFor();
  assert.ok(e.api.chat().some((s) => s.path === `/v1/chat/conversations/${mine}/messages`));

  const theirs = await create(token({ email: 'someone@else.com', account: 'x' }), 'not yours');
  const page2 = await mounted(e, ['report'], { id: theirs, state: 'open' });
  await page2.waitForTimeout(500);
  assert.equal(await page2.locator('devbench-chat .msg').count(), 0);
  assert.equal(await page2.locator('devbench-chat .status').textContent(), '');
});

test("the chat calls are the SDK's own: a failing bootstrap is never recorded as the app's failed request", async (t) => {
  const e = await env(t);
  e.api.bootstrap = () => ({ status: 403, body: { error: 'x', code: 'unavailable' } });
  const page = await open(e, { tag: { 'data-dsn': dsn(e), 'data-api': e.api.url, 'data-widget-identity': token() } });
  await waitDebug(e);
  // The page's own failing request, so the flush has something to carry.
  await page.evaluate((u) => fetch(`${u}/app/broken`).then(() => undefined), e.api.url);
  await page.evaluate(() => (window as unknown as { Devbench: { getSensor(): { flush(): Promise<void> } } }).Devbench.getSensor().flush());
  // Counts carry fingerprints, not URLs: the page's one failure is one count,
  // and a recorded bootstrap (403, another path) would be a second.
  const counts = e.api.seen
    .filter((s) => s.path === '/v1/flush' && s.method === 'POST')
    .flatMap((s) => (JSON.parse(s.body) as { counts: Array<{ fp: string }> }).counts);
  assert.equal(counts.length, 1, `expected only the page's own failure, got ${JSON.stringify(counts)}`);
});
