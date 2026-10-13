/**
 * Google Chrome driven over a bare DevTools Protocol socket, for the browser
 * soak (test/soak.browser.test.ts).
 *
 * Why not playwright-core, as widget.browser.test.ts uses: playwright enables
 * the DevTools Network domain on every page it drives, and Chrome's network
 * agent then keeps a copy of every request and response in the renderer's
 * own heap. Measured on a page with no SDK at all: ~330 bytes more per
 * request, every request, so a soak through playwright measures DevTools and
 * fails without any leak in the page (#228). Over a bare socket only the
 * domains asked for here are on, and none of them buffers per request.
 *
 * Chrome is found as playwright's `channel: 'chrome'` finds it; CHROME_PATH
 * overrides. No browser is a failure, never a skip.
 */
import { execFileSync, spawn, type ChildProcess } from 'node:child_process';
import { closeSync, existsSync, mkdirSync, mkdtempSync, openSync, readFileSync } from 'node:fs';
import { rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const DEFAULT_PATHS: Record<string, string> = {
  darwin: '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  linux: '/opt/google/chrome/chrome',
  win32: 'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
};

/**
 * How long Chrome gets to write DevToolsActivePort. A warm start takes well
 * under a second; a cold CI runner, with the widget's Chrome and the other
 * test files starting beside it, has taken over 10s (#317).
 */
export const PORT_DEADLINE_MS = 30_000;
/** How long close() waits for the browser, then for its helpers, before SIGKILL; then for SIGKILL. */
const EXIT_DEADLINE_MS = 5_000;
const KILL_DEADLINE_MS = 2_000;
const STDERR_TAIL = 4096;

export function chromePath(): string {
  const path = process.env.CHROME_PATH || DEFAULT_PATHS[process.platform];
  if (!path || !existsSync(path)) {
    throw new Error(`Google Chrome not found at ${path ?? `(no default for ${process.platform})`}; set CHROME_PATH`);
  }
  return path;
}

/** What close() had to do. Cleanup never fails a test: a problem is reported here and as a process warning. */
export interface CloseReport {
  /** Chrome's processes when close() began, by --type ("browser" for the browser itself). */
  atClose: string[];
  /** Chrome's own processes still running when the browser process had exited, by --type (renderer, utility, ...). */
  stragglers: string[];
  /** How long close() waited for them. */
  waitedMs: number;
  /** Processes that had to be SIGKILLed. */
  killed: number;
  /** The profile directory is gone. */
  removed: boolean;
  /** Why it is not, when it is not. */
  problem?: string;
}

export interface Chrome {
  port: number;
  /** The browser target's DevTools path, /devtools/browser/<id>. */
  browserPath: string;
  profile: string;
  pid: number;
  /** Stops Chrome and every process of it, then removes the profile. Idempotent; never rejects. */
  close(): Promise<CloseReport>;
}

export interface LaunchOptions {
  /** The executable; chromePath() when not given. */
  executable?: string;
  /** Chrome's environment; this process's when not given. */
  env?: NodeJS.ProcessEnv;
  portDeadlineMs?: number;
}

const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms));
const message = (err: unknown): string => (err instanceof Error ? err.message : String(err));

/**
 * Launches headless Chrome on a fresh profile and resolves once it has
 * written DevToolsActivePort. On any failure Chrome is stopped and its
 * profile removed before the error is thrown.
 */
export async function launchChrome(opts: LaunchOptions = {}): Promise<Chrome> {
  const executable = opts.executable ?? chromePath();
  // <dir>/profile is Chrome's; <dir>/stderr.log is its stderr, a file and not
  // a pipe: a helper that inherits a pipe and outlives Chrome would keep this
  // process's event loop, and the test file, alive.
  const dir = mkdtempSync(join(tmpdir(), 'devbench-soak-'));
  const profile = join(dir, 'profile');
  mkdirSync(profile);
  const stderrFile = join(dir, 'stderr.log');
  const stderrFd = openSync(stderrFile, 'w');
  const args = [
    '--headless=new', '--remote-debugging-port=0', `--user-data-dir=${profile}`,
    '--no-first-run', '--no-default-browser-check', '--disable-extensions',
    // As playwright does on Linux: the runner's AppArmor policy forbids the
    // user namespaces Chrome's sandbox needs.
    ...(process.platform === 'linux' ? ['--no-sandbox'] : []),
    'about:blank',
  ];
  let child: ChildProcess;
  try {
    child = spawn(executable, args, { stdio: ['ignore', 'ignore', stderrFd], env: opts.env ?? process.env });
  } finally {
    closeSync(stderrFd);
  }
  const stderr = (): string => {
    try {
      return readFileSync(stderrFile, 'utf8').slice(-STDERR_TAIL);
    } catch {
      return '';
    }
  };
  // 'exit' carries a death by signal too, which leaves exitCode null.
  let exit: { code: number | null; signal: NodeJS.Signals | null } | null = null;
  let spawnError: Error | null = null;
  const exited = new Promise<void>((resolve) => {
    child.once('exit', (code, signal) => {
      exit = { code, signal };
      resolve();
    });
    child.once('error', (err) => {
      spawnError = err;
      if (child.pid === undefined) resolve(); // never started: no 'exit' follows
    });
  });
  const running = (): boolean => exit === null && child.pid !== undefined;

  let closing: Promise<CloseReport> | null = null;
  const close = (): Promise<CloseReport> => (closing ??= stop(child, running, exited, profile, dir));

  try {
    const { port, browserPath } = await devtoolsPort(profile, opts.portDeadlineMs ?? PORT_DEADLINE_MS, stderr, () => {
      const err = spawnError as Error | null;
      if (err) return `Chrome could not be started (${executable}): ${err.message}`;
      const e = exit as { code: number | null; signal: NodeJS.Signals | null } | null;
      if (e) return `Chrome exited (code ${e.code}, signal ${e.signal}) before writing DevToolsActivePort`;
      return null;
    });
    return { port, browserPath, profile, pid: child.pid!, close };
  } catch (err) {
    await close();
    throw err;
  }
}

/**
 * Chrome writes "<port>\n<browser path>" to <profile>/DevToolsActivePort.
 * Polled against a wall-clock deadline; a file without both lines is not
 * finished yet. `failed` says why Chrome can no longer write it.
 */
async function devtoolsPort(profile: string, deadline: number, stderr: () => string, failed: () => string | null): Promise<{ port: number; browserPath: string }> {
  const file = join(profile, 'DevToolsActivePort');
  const started = Date.now();
  let seen: string | null = null;
  for (;;) {
    const why = failed();
    if (why) throw new Error(why + tail(stderr()));
    if (existsSync(file)) {
      seen = readFileSync(file, 'utf8');
      const m = /^(\d+)\r?\n(\/devtools\/browser\/\S+)/.exec(seen);
      if (m) return { port: Number(m[1]), browserPath: m[2]! };
    }
    if (Date.now() - started >= deadline) {
      const state = seen === null ? 'it never appeared' : `it holds only ${JSON.stringify(seen)}`;
      throw new Error(`Chrome did not write its DevTools port to ${file} within ${deadline / 1000}s: ${state}; ` +
        `Chrome is still running${tail(stderr())}`);
    }
    await sleep(50);
  }
}

const tail = (stderr: string): string =>
  stderr.trim() ? `. Chrome's stderr ends:\n${stderr.trimEnd()}` : ". Chrome's stderr is empty";

/**
 * This Chrome's live processes: every one whose command line names the
 * profile. Chrome hands --user-data-dir to every helper it starts (renderer,
 * GPU, network and storage services, the Linux zygote); a helper orphaned by
 * the browser's exit is no longer its descendant, but still names the
 * profile. Processes Chrome merely launched (macOS starts GoogleUpdater) do
 * not, and are not ours to wait for or kill. Zombies hold no files.
 */
function chromeProcesses(profile: string): Map<number, string> {
  const found = new Map<number, string>();
  if (process.platform === 'win32') return found;
  const out = execFileSync('ps', ['-A', '-ww', '-o', 'pid=,stat=,command='], { encoding: 'utf8', maxBuffer: 64 * 1024 * 1024 });
  for (const line of out.split('\n')) {
    const m = /^\s*(\d+)\s+(\S+)\s(.*)$/.exec(line);
    if (!m || m[2]!.startsWith('Z') || !m[3]!.includes(profile)) continue;
    const pid = Number(m[1]);
    if (pid !== process.pid) found.set(pid, /--type=(\S+)/.exec(m[3]!)?.[1] ?? 'browser');
  }
  return found;
}

/** Polls until none of this Chrome's processes is left or `ms` passes; returns those left. */
async function drain(profile: string, ms: number): Promise<Map<number, string>> {
  const until = Date.now() + ms;
  for (;;) {
    const left = chromeProcesses(profile);
    if (left.size === 0 || Date.now() >= until) return left;
    await sleep(25);
  }
}

/**
 * Stopping Chrome is not one process exiting. Its helpers notice the browser
 * is gone only afterwards and can still be writing into the profile, so
 * removing it the moment the browser exits races them: ENOTEMPTY on Default
 * (#317). So: SIGTERM the browser and wait for it, then wait for every
 * process that names the profile (SIGKILL after a deadline), and only then
 * remove it, with retries. Never rejects: a failure here is a warning.
 */
async function stop(child: ChildProcess, running: () => boolean, exited: Promise<void>, profile: string, dir: string): Promise<CloseReport> {
  const report: CloseReport = { atClose: [], stragglers: [], waitedMs: 0, killed: 0, removed: false };
  const problems: string[] = [];
  try {
    report.atClose = [...chromeProcesses(profile).values()].sort();
    if (running()) {
      child.kill('SIGTERM');
      if (!(await Promise.race([exited.then(() => true), sleep(EXIT_DEADLINE_MS).then(() => false)]))) {
        child.kill('SIGKILL');
        report.killed++;
        await exited;
      }
    }
    const started = Date.now();
    report.stragglers = [...chromeProcesses(profile).values()].sort();
    let left = await drain(profile, EXIT_DEADLINE_MS);
    if (left.size > 0) {
      for (const pid of left.keys()) {
        try {
          process.kill(pid, 'SIGKILL');
          report.killed++;
        } catch {
          // gone already
        }
      }
      left = await drain(profile, KILL_DEADLINE_MS);
      if (left.size > 0) problems.push(`Chrome processes ${[...left.keys()].join(', ')} survived SIGKILL`);
    }
    report.waitedMs = Date.now() - started;
  } catch (err) {
    problems.push(`stopping Chrome: ${message(err)}`);
  }
  try {
    await rm(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 });
    report.removed = !existsSync(dir);
    if (!report.removed) problems.push('it is still there after rm');
  } catch (err) {
    problems.push(`removing it: ${message(err)}`);
  }
  if (problems.length > 0) {
    report.problem = `could not clean up the Chrome profile ${profile}: ${problems.join('; ')}`;
    process.emitWarning(report.problem);
  }
  return report;
}

export interface Tab {
  /** One DevTools command on the page's target. Rejects on a protocol error. */
  send<T = Record<string, unknown>>(method: string, params?: Record<string, unknown>): Promise<T>;
  /** Evaluates `expression` in the page, awaiting a promise; returns its JSON value. */
  evaluate<T>(expression: string): Promise<T>;
  /** A real (trusted) left click at the centre of the first `selector` match. */
  click(selector: string): Promise<void>;
  /** Chrome's profile directory. */
  profile: string;
  /** Closes the DevTools socket, then Chrome.close(). Never rejects. */
  close(): Promise<CloseReport>;
}

/** Launches headless Chrome on `url` and resolves once the page has loaded. */
export async function openTab(url: string, opts: LaunchOptions = {}): Promise<Tab> {
  const chrome = await launchChrome(opts);
  const port = chrome.port;
  let socket: WebSocket | null = null;

  const close = async (): Promise<CloseReport> => {
    socket?.close();
    return chrome.close();
  };

  try {
    const targets = (await (await fetch(`http://127.0.0.1:${port}/json/list`)).json()) as Array<{ type: string; webSocketDebuggerUrl: string }>;
    const target = targets.find((t) => t.type === 'page');
    if (!target) throw new Error('Chrome started without a page target');

    const ws = new WebSocket(target.webSocketDebuggerUrl);
    socket = ws;
    await new Promise<void>((resolve, reject) => {
      ws.onopen = () => resolve();
      ws.onerror = () => reject(new Error('could not connect to the DevTools socket'));
    });

    let seq = 0;
    const pending = new Map<number, { resolve(v: unknown): void; reject(e: Error): void; method: string }>();
    ws.onmessage = (m: MessageEvent) => {
      const msg = JSON.parse(String(m.data)) as { id?: number; result?: unknown; error?: { message: string } };
      if (msg.id === undefined) return; // an event: no domain that emits any is enabled
      const p = pending.get(msg.id);
      if (!p) return;
      pending.delete(msg.id);
      if (msg.error) p.reject(new Error(`${p.method}: ${msg.error.message}`));
      else p.resolve(msg.result);
    };
    ws.onclose = () => {
      for (const p of pending.values()) p.reject(new Error(`${p.method}: the DevTools socket closed`));
      pending.clear();
    };

    const send = <T>(method: string, params: Record<string, unknown> = {}): Promise<T> =>
      new Promise<T>((resolve, reject) => {
        const id = ++seq;
        pending.set(id, { resolve: resolve as (v: unknown) => void, reject, method });
        ws.send(JSON.stringify({ id, method, params }));
      });

    const evaluate = async <T>(expression: string): Promise<T> => {
      const r = await send<{ result: { value?: T }; exceptionDetails?: { text: string; exception?: { description?: string } } }>(
        'Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
      if (r.exceptionDetails) {
        throw new Error(`page threw: ${r.exceptionDetails.exception?.description ?? r.exceptionDetails.text}`);
      }
      return r.result.value as T;
    };

    const click = async (selector: string): Promise<void> => {
      const at = await evaluate<{ x: number; y: number } | null>(
        `(() => { const e = document.querySelector(${JSON.stringify(selector)}); if (!e) return null;
          const r = e.getBoundingClientRect(); return { x: r.left + r.width / 2, y: r.top + r.height / 2 }; })()`);
      if (!at) throw new Error(`click: nothing matches ${selector}`);
      for (const type of ['mousePressed', 'mouseReleased']) {
        await send('Input.dispatchMouseEvent', { type, x: at.x, y: at.y, button: 'left', clickCount: 1 });
      }
    };

    await send('Page.navigate', { url });
    for (let i = 0; ; i++) {
      if ((await evaluate<string>('document.readyState')) === 'complete') break;
      if (i >= 200) throw new Error(`${url} did not finish loading`);
      await new Promise((r) => setTimeout(r, 50));
    }

    return { send, evaluate, click, profile: chrome.profile, close };
  } catch (err) {
    await close();
    throw err;
  }
}
