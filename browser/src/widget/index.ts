/**
 * The chat widget bundle (dist/devbench-widget.min.js): loaded by the core
 * SDK's loader only when the chat API said this user gets a widget, then
 * mounted with what the loader hands it (`WidgetConfig`). Routes and error
 * codes: docs/CHAT_API.md.
 *
 * - Messages are plain text: rendered with `textContent`, line breaks kept.
 *   Only a message's `links` (https) are clickable. Never HTML or Markdown.
 * - `capabilities` drive the starting suggestions and the handoff button; the
 *   service enforces them regardless.
 * - `identity_expired` → "Reload the page to keep chatting." and no further
 *   calls. Any other 401/403 → unavailable, likewise final.
 * - Everything lives in a shadow root, so the page's CSS and ours never meet.
 */
import { ChatClient, ChatError } from './client.js';
import type { WidgetConfig, WidgetGlobal, WidgetHandle } from './types.js';

declare const __WIDGET_VERSION__: string;

interface Message {
  id?: number;
  role?: string;
  text?: string;
  links?: Array<{ title?: string; url?: string }>;
}

interface Conversation {
  id: number;
  state?: string;
}

interface Thread {
  conversation?: Conversation;
  messages?: Message[];
}

const MAX_TEXT = 4000;
const RELOAD = 'Reload the page to keep chatting.';
const UNAVAILABLE = "Chat isn't available right now.";
const NETWORK = "Couldn't reach the chat. Check your connection and try again.";

/** Starting suggestions, by capability: a label and the text it starts. */
const SUGGESTIONS: Record<string, [string, string]> = {
  report: ['Report a problem', "Something isn't working: "],
  status: ['Is something broken?', 'Is something broken right now? '],
  howto: ['How do I…?', 'How do I '],
  feature_requests: ['Request a feature', "I'd like to request a feature: "],
};

const CSS = `
:host{all:initial}
*{box-sizing:border-box;font-family:system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;font-size:14px;line-height:1.4}
.launcher{position:fixed;bottom:20px;z-index:2147483000;border:0;border-radius:24px;padding:12px 18px;color:#fff;cursor:pointer;box-shadow:0 2px 10px rgba(0,0,0,.25)}
.right{right:20px}.left{left:20px}
.panel{position:fixed;bottom:76px;z-index:2147483000;width:360px;max-width:calc(100vw - 40px);height:520px;max-height:calc(100vh - 100px);display:flex;flex-direction:column;background:#fff;color:#1f2328;border-radius:12px;box-shadow:0 6px 30px rgba(0,0,0,.25);overflow:hidden}
.panel[hidden]{display:none}
header{display:flex;align-items:center;justify-content:space-between;padding:12px 14px;color:#fff}
header h2{margin:0;font-size:15px;font-weight:600}
header button{background:none;border:0;color:inherit;font-size:20px;cursor:pointer;padding:0 4px}
.log{flex:1;overflow-y:auto;padding:12px;display:flex;flex-direction:column;gap:8px}
.msg{max-width:85%;padding:8px 11px;border-radius:10px;white-space:pre-wrap;overflow-wrap:anywhere}
.msg p{margin:0}
.user{align-self:flex-end;background:#e7f0fd}
.assistant{align-self:flex-start;background:#f3f4f6}
.notice{align-self:center;background:none;color:#57606a;font-size:13px;text-align:center}
.msg ul{margin:6px 0 0;padding-left:18px}
.msg a{color:#0969da}
.typing{align-self:flex-start;color:#57606a;font-style:italic}
.suggestions{display:flex;flex-wrap:wrap;gap:6px;padding:0 12px 8px}
.suggestions button,.actions button{border:1px solid #d0d7de;background:#fff;border-radius:14px;padding:5px 10px;cursor:pointer;color:#1f2328}
.actions{display:flex;gap:6px;padding:0 12px 8px}
.actions button[hidden],.suggestions[hidden]{display:none}
.status{padding:0 12px 8px;color:#b42318;font-size:13px}
.status:empty{display:none}
form{display:flex;gap:6px;padding:10px 12px;border-top:1px solid #eaeef2}
textarea{flex:1;resize:none;height:40px;border:1px solid #d0d7de;border-radius:8px;padding:9px;color:#1f2328;background:#fff}
form button{border:0;border-radius:8px;padding:0 14px;color:#fff;cursor:pointer}
button:disabled,textarea:disabled{opacity:.55;cursor:not-allowed}
`;

function el<K extends keyof HTMLElementTagNameMap>(tag: K, cls?: string, text?: string): HTMLElementTagNameMap[K] {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

function newClientId(): string {
  const c = (globalThis as { crypto?: Crypto }).crypto;
  if (c && typeof c.randomUUID === 'function') return c.randomUUID();
  return `w${Date.now().toString(36)}${Math.random().toString(36).slice(2, 12)}`;
}

/** The page URL for `context.page_url`: http(s), at most 2,048 characters. */
function pageUrl(): string | undefined {
  try {
    const href = location.href;
    return /^https?:\/\//i.test(href) && href.length <= 2048 ? href : undefined;
  } catch {
    return undefined;
  }
}

class Widget implements WidgetHandle {
  private readonly host: HTMLElement;
  private readonly root: ShadowRoot;
  private readonly client: ChatClient;
  private readonly caps: string[];
  private readonly launcher: HTMLButtonElement;
  private readonly panel: HTMLElement;
  private readonly log: HTMLElement;
  private readonly suggestions: HTMLElement;
  private readonly statusLine: HTMLElement;
  private readonly input: HTMLTextAreaElement;
  private readonly send: HTMLButtonElement;
  private readonly handoffBtn: HTMLButtonElement;
  private readonly newBtn: HTMLButtonElement;

  private convId: number | null = null;
  private closed = false;
  private dead = false;
  private busy = false;
  private hasMessages = false;
  private resumeId: number | null;
  private resumed = false;
  private coolUntil = 0;
  private coolTimer: ReturnType<typeof setTimeout> | undefined;
  /** The unsent message: a retry of the same text reuses its client_id. */
  private pending: { text: string; clientId: string } | null = null;

  constructor(private readonly cfg: WidgetConfig) {
    const b = cfg.bootstrap;
    this.caps = Array.isArray(b.capabilities) ? b.capabilities.filter((c) => typeof c === 'string') : [];
    this.resumeId = b.resume && typeof b.resume.id === 'number' ? b.resume.id : null;
    this.client = new ChatClient(cfg.api, cfg.key, cfg.token, (...a) => fetch(...a));

    const color = typeof b.color === 'string' && /^#[0-9a-f]{3,8}$/i.test(b.color) ? b.color : '#1d76db';
    const side = b.position === 'bottom-left' ? 'left' : 'right';
    const name = typeof b.name === 'string' && b.name.trim() ? b.name.trim() : 'Help';

    this.host = document.createElement('devbench-chat');
    this.root = this.host.attachShadow({ mode: 'open' });
    this.style();

    this.launcher = el('button', `launcher ${side}`, name);
    this.launcher.type = 'button';
    this.launcher.style.background = color;
    this.launcher.setAttribute('aria-expanded', 'false');
    this.launcher.addEventListener('click', () => this.toggle());

    this.panel = el('section', `panel ${side}`);
    this.panel.hidden = true;
    this.panel.setAttribute('role', 'dialog');
    this.panel.setAttribute('aria-label', name);
    const header = el('header');
    header.style.background = color;
    const close = el('button', '', '×');
    close.type = 'button';
    close.setAttribute('aria-label', 'Close');
    close.addEventListener('click', () => this.toggle(false));
    header.append(el('h2', '', name), close);

    this.log = el('div', 'log');
    this.log.setAttribute('role', 'log');
    this.log.setAttribute('aria-live', 'polite');

    this.suggestions = el('div', 'suggestions');
    for (const cap of this.caps) {
      const s = SUGGESTIONS[cap];
      if (!s) continue;
      const btn = el('button', '', s[0]);
      btn.type = 'button';
      btn.dataset.capability = cap;
      btn.addEventListener('click', () => {
        this.input.value = s[1];
        this.input.focus();
      });
      this.suggestions.append(btn);
    }

    const actions = el('div', 'actions');
    this.handoffBtn = el('button', 'handoff', 'Talk to a person');
    this.handoffBtn.type = 'button';
    this.handoffBtn.addEventListener('click', () => void this.handoff());
    this.newBtn = el('button', 'new', 'New conversation');
    this.newBtn.type = 'button';
    this.newBtn.addEventListener('click', () => this.startOver());
    actions.append(this.handoffBtn, this.newBtn);

    this.statusLine = el('div', 'status');
    this.statusLine.setAttribute('role', 'status');

    const form = el('form');
    this.input = el('textarea');
    this.input.setAttribute('aria-label', 'Message');
    this.input.placeholder = 'Type a message';
    this.input.maxLength = MAX_TEXT;
    this.input.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !e.shiftKey) {
        e.preventDefault();
        void this.submit();
      }
    });
    this.send = el('button', '', 'Send');
    this.send.type = 'submit';
    this.send.style.background = color;
    form.append(this.input, this.send);
    form.addEventListener('submit', (e) => {
      e.preventDefault();
      void this.submit();
    });

    this.panel.append(header, this.log, this.suggestions, actions, this.statusLine, form);
    this.root.append(this.launcher, this.panel);
    document.body.appendChild(this.host);
    this.refresh();
  }

  destroy(): void {
    this.dead = true;
    this.client.abortAll();
    clearTimeout(this.coolTimer);
    this.host.remove();
  }

  private style(): void {
    const root = this.root as ShadowRoot & { adoptedStyleSheets?: CSSStyleSheet[] };
    // A constructed sheet is not subject to the page's style-src CSP.
    try {
      if (typeof CSSStyleSheet === 'function' && 'replaceSync' in CSSStyleSheet.prototype && Array.isArray(root.adoptedStyleSheets)) {
        const sheet = new CSSStyleSheet();
        sheet.replaceSync(CSS);
        root.adoptedStyleSheets = [sheet];
        return;
      }
    } catch {
      // Fall through to a <style> element.
    }
    const s = el('style', '', CSS);
    if (this.cfg.nonce) s.nonce = this.cfg.nonce;
    root.append(s);
  }

  private toggle(open?: boolean): void {
    const show = open === undefined ? this.panel.hidden : open;
    this.panel.hidden = !show;
    this.launcher.setAttribute('aria-expanded', String(show));
    if (show) {
      if (!this.resumed) void this.resume();
      this.input.focus();
    }
  }

  /** The bootstrap's `resume`: this user's open conversation, shown once. */
  private async resume(): Promise<void> {
    this.resumed = true;
    if (this.resumeId === null || this.dead) return;
    const id = this.resumeId;
    this.setBusy(true);
    try {
      const t = await this.client.request<Thread>('GET', `/v1/chat/conversations/${id}`);
      this.apply(t);
    } catch (err) {
      // A conversation that is gone just means starting fresh.
      if (!(err instanceof ChatError && err.code === 'not_found')) this.fail(err);
    } finally {
      this.setBusy(false);
    }
  }

  private async submit(): Promise<void> {
    if (this.dead || this.busy || this.closed || Date.now() < this.coolUntil) return;
    const text = this.input.value.trim();
    if (!text) return;
    if (text.length > MAX_TEXT) return this.say('Messages can be up to 4,000 characters.');

    if (!this.pending || this.pending.text !== text) this.pending = { text, clientId: newClientId() };
    const context: Record<string, string> = {};
    const url = pageUrl();
    if (url) context.page_url = url;
    if (this.cfg.session && /^[A-Za-z0-9_-]{1,128}$/.test(this.cfg.session)) context.session = this.cfg.session;
    const body = { text, client_id: this.pending.clientId, context };
    const path = this.convId === null ? '/v1/chat/conversations' : `/v1/chat/conversations/${this.convId}/messages`;

    this.say('');
    this.setBusy(true, true);
    try {
      const t = await this.client.request<Thread>('POST', path, body);
      this.pending = null;
      this.input.value = '';
      this.apply(t);
    } catch (err) {
      this.fail(err);
    } finally {
      this.setBusy(false);
    }
  }

  private async handoff(): Promise<void> {
    if (this.dead || this.busy || this.convId === null) return;
    this.say('');
    this.setBusy(true);
    try {
      const t = await this.client.request<Thread>('POST', `/v1/chat/conversations/${this.convId}/handoff`, {});
      this.apply(t);
    } catch (err) {
      this.fail(err);
    } finally {
      this.setBusy(false);
    }
  }

  private startOver(): void {
    if (this.dead) return;
    this.convId = null;
    this.closed = false;
    this.hasMessages = false;
    this.pending = null;
    this.log.textContent = '';
    this.say('');
    this.refresh();
    this.input.focus();
  }

  /** A conversation and messages from any route: render, and track state. */
  private apply(t: Thread): void {
    if (this.dead) return;
    const c = t.conversation;
    if (c && typeof c.id === 'number') {
      this.convId = c.id;
      // Unknown states are treated as open (CHAT_API.md, Conversation).
      this.closed = c.state === 'handed_off';
    }
    for (const m of Array.isArray(t.messages) ? t.messages : []) this.render(m);
    this.refresh();
  }

  private render(m: Message): void {
    const role = m.role === 'user' || m.role === 'notice' ? m.role : 'assistant';
    const box = el('div', `msg ${role}`);
    box.append(el('p', '', typeof m.text === 'string' ? m.text : ''));
    const links = Array.isArray(m.links) ? m.links : [];
    const list = el('ul');
    for (const l of links.slice(0, 5)) {
      if (!l || typeof l.url !== 'string' || !/^https:\/\//i.test(l.url)) continue;
      const a = el('a', '', typeof l.title === 'string' && l.title ? l.title : l.url);
      a.href = l.url;
      a.target = '_blank';
      a.rel = 'noopener noreferrer';
      const li = el('li');
      li.append(a);
      list.append(li);
    }
    if (list.childNodes.length) box.append(list);
    this.log.append(box);
    this.hasMessages = true;
    this.log.scrollTop = this.log.scrollHeight;
  }

  private fail(err: unknown): void {
    if (this.dead) return;
    const e = err instanceof ChatError ? err : new ChatError(0, '', '', null);
    switch (e.code) {
      case 'identity_expired':
        return this.stop(RELOAD);
      case 'key_invalid':
      case 'identity_invalid':
      case 'unavailable':
      case 'origin_not_allowed':
        return this.stop(UNAVAILABLE);
      case 'conversation_closed':
        this.closed = true;
        this.refresh();
        return this.say(e.message || 'This conversation is closed. Start a new one to keep chatting.');
      case 'not_found':
        this.convId = null;
        this.refresh();
        return this.say('That conversation is no longer available. Send again to start a new one.');
      case 'rate_limited': {
        const secs = e.retryAfter !== null && e.retryAfter > 0 ? Math.min(e.retryAfter, 3600) : 30;
        this.coolUntil = Date.now() + secs * 1000;
        clearTimeout(this.coolTimer);
        this.coolTimer = setTimeout(() => {
          this.coolUntil = 0;
          this.say('');
          this.refresh();
        }, secs * 1000);
        this.refresh();
        return this.say(`You're sending messages quickly. Try again in ${secs} seconds.`);
      }
      default:
        if (e.status === 401 || e.status === 403) return this.stop(UNAVAILABLE);
        return this.say(e.code === 'network' ? NETWORK : e.message || 'Something went wrong. Try again.');
    }
  }

  /** Final: no further calls from this widget. */
  private stop(message: string): void {
    this.dead = true;
    this.client.abortAll();
    this.say(message);
    this.refresh();
  }

  private say(message: string): void {
    this.statusLine.textContent = message;
  }

  private typing: HTMLElement | null = null;

  private setBusy(busy: boolean, typing = false): void {
    this.busy = busy;
    if (this.typing) {
      this.typing.remove();
      this.typing = null;
    }
    if (busy && typing) {
      this.typing = el('div', 'typing', 'Typing…');
      this.typing.setAttribute('aria-label', 'The assistant is typing');
      this.log.append(this.typing);
      this.log.scrollTop = this.log.scrollHeight;
    }
    this.refresh();
  }

  private refresh(): void {
    const cooling = Date.now() < this.coolUntil;
    const off = this.dead || this.busy;
    this.input.disabled = this.dead || this.closed;
    this.send.disabled = off || this.closed || cooling;
    this.suggestions.hidden = this.dead || this.closed || this.hasMessages;
    this.handoffBtn.hidden = this.caps.indexOf('handoff') < 0 || this.convId === null || this.closed || this.dead;
    this.handoffBtn.disabled = off;
    this.newBtn.hidden = !this.closed || this.dead;
  }
}

let current: Widget | null = null;

const api: WidgetGlobal = {
  version: typeof __WIDGET_VERSION__ === 'string' ? __WIDGET_VERSION__ : 'dev',
  /** Mounts the widget; a second mount (a replaced token) replaces the first. */
  mount(config: WidgetConfig): WidgetHandle {
    if (current) current.destroy();
    const w = new Widget(config);
    current = w;
    return {
      destroy(): void {
        w.destroy();
        if (current === w) current = null;
      },
    };
  },
};

(globalThis as { DevbenchWidget?: WidgetGlobal }).DevbenchWidget = api;
