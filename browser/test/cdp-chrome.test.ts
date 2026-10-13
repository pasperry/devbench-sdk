/**
 * The soak's Chrome launcher and cleanup (helpers/cdp-chrome.ts), against a
 * stand-in Chrome: a real executable that behaves as Chrome did when the
 * soak failed in CI (#317). Real processes, a real profile directory, real
 * signals; only the browser is a stand-in, so each failure is reproduced on
 * purpose instead of waiting for a slow runner.
 *
 *   - Chrome's helper processes outlive the browser process for a moment and
 *     keep writing into the profile: removing it then raced them (ENOTEMPTY
 *     on Default) and failed the test from its after hook.
 *   - Chrome is slow to write DevToolsActivePort on a cold runner: the old
 *     10s poll gave up first. A crash by signal was reported as that timeout.
 */
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { launchChrome, PORT_DEADLINE_MS, type LaunchOptions } from './helpers/cdp-chrome.js';

/**
 * Writes into <FAKE_PROFILE>/Default as fast as it can, as Chrome's network
 * and storage services do, recreating the directory if it goes. FAKE_HELPER:
 * `linger:<ms>` exits that long after its parent does (a Chrome helper
 * noticing the browser is gone); `stubborn` ignores SIGTERM and never stops.
 */
const HELPER = `
const { writeFileSync, mkdirSync } = require('node:fs');
const { join } = require('node:path');
const dir = join(process.env.FAKE_PROFILE, 'Default');
const mode = process.env.FAKE_HELPER;
if (mode === 'stubborn') process.on('SIGTERM', () => {});
const parent = process.ppid;
let orphanedAt = null;
for (let n = 0; ; n++) {
  if (orphanedAt === null && process.ppid !== parent) orphanedAt = Date.now();
  if (mode.startsWith('linger:') && orphanedAt !== null && Date.now() - orphanedAt > Number(mode.slice(7))) process.exit(0);
  try { writeFileSync(join(dir, 'f' + (n % 50)), 'x'); } catch { try { mkdirSync(dir, { recursive: true }); } catch {} }
}
`;

/** The stand-in Chrome. Takes Chrome's arguments; FAKE_* variables choose how it misbehaves. */
const FAKE_CHROME = `#!${process.execPath}
const { writeFileSync } = require('node:fs');
const { spawn } = require('node:child_process');
const { join } = require('node:path');
const env = process.env;
const profile = process.argv.find((a) => a.startsWith('--user-data-dir=')).slice('--user-data-dir='.length);
if (env.FAKE_PID_FILE) writeFileSync(env.FAKE_PID_FILE, String(process.pid));
if (env.FAKE_STDERR) process.stderr.write(env.FAKE_STDERR + '\\n');
if (env.FAKE_EXIT) process.exit(Number(env.FAKE_EXIT));
if (env.FAKE_SIGNAL) process.kill(process.pid, env.FAKE_SIGNAL);
if (env.FAKE_PARTIAL_PORT) writeFileSync(join(profile, 'DevToolsActivePort'), env.FAKE_PARTIAL_PORT);
const delay = Number(env.FAKE_PORT_DELAY_MS ?? 0);
if (delay >= 0) setTimeout(() => writeFileSync(join(profile, 'DevToolsActivePort'), '9222\\n/devtools/browser/fake-id'), delay);
if (env.FAKE_HELPER) {
  // As Chrome's helpers do, it names the profile on its command line.
  const h = spawn(process.execPath, ['-e', ${JSON.stringify(HELPER)}, '--', '--type=fake-helper', '--user-data-dir=' + profile],
    { stdio: 'ignore', env: { ...env, FAKE_PROFILE: profile } });
  if (env.FAKE_HELPER_PID_FILE) writeFileSync(env.FAKE_HELPER_PID_FILE, String(h.pid));
}
setInterval(() => {}, 1000);
`;

const scratch = mkdtempSync(join(tmpdir(), 'devbench-fake-chrome-'));
const executable = join(scratch, 'chrome');
writeFileSync(executable, FAKE_CHROME);
chmodSync(executable, 0o755);
test.after(() => rmSync(scratch, { recursive: true, force: true }));

let files = 0;
const pidFile = (): string => join(scratch, `pid-${++files}`);

function launch(env: Record<string, string>, opts: LaunchOptions = {}) {
  return launchChrome({ executable, env: { ...process.env, ...env }, ...opts });
}

function alive(pid: number): boolean {
  try {
    process.kill(pid, 0);
  } catch {
    return false;
  }
  // A zombie still answers signal 0 but holds nothing.
  return !/Z/.test(readStat(pid));
}

function readStat(pid: number): string {
  try {
    return readFileSync(`/proc/${pid}/stat`, 'utf8').split(') ')[1]?.[0] ?? '';
  } catch {
    return ''; // macOS: no /proc; a reaped process fails kill(0) above
  }
}

async function waitFor(what: string, cond: () => boolean, ms = 3000): Promise<void> {
  const until = Date.now() + ms;
  while (!cond()) {
    if (Date.now() > until) throw new Error(`timed out waiting for ${what}`);
    await new Promise((r) => setTimeout(r, 20));
  }
}

const readPid = (file: string): number => Number(readFileSync(file, 'utf8'));

test('close() waits for Chrome helpers still writing into the profile after the browser exits, then removes it', async () => {
  const helperPid = pidFile();
  const chrome = await launch({ FAKE_HELPER: 'linger:500', FAKE_HELPER_PID_FILE: helperPid });
  await waitFor('the helper to start writing', () => existsSync(join(chrome.profile, 'Default')));
  const helper = readPid(helperPid);

  const report = await chrome.close();

  assert.equal(report.problem, undefined);
  assert.equal(report.removed, true);
  assert.equal(existsSync(chrome.profile), false, 'the profile directory is still there');
  assert.deepEqual(report.atClose, ['browser', 'fake-helper']);
  assert.deepEqual(report.stragglers, ['fake-helper'], 'the helper outlived the browser');
  assert.ok(report.waitedMs >= 400, `close() waited ${report.waitedMs}ms for a helper that writes for 500ms more`);
  assert.equal(alive(helper), false, 'the helper is still running');
});

test('close() SIGKILLs a helper that ignores SIGTERM and never stops, then removes the profile', async () => {
  const helperPid = pidFile();
  const chrome = await launch({ FAKE_HELPER: 'stubborn', FAKE_HELPER_PID_FILE: helperPid });
  await waitFor('the helper to start writing', () => existsSync(join(chrome.profile, 'Default')));
  const helper = readPid(helperPid);

  const report = await chrome.close();

  assert.equal(report.problem, undefined);
  assert.equal(existsSync(chrome.profile), false, 'the profile directory is still there');
  assert.ok(report.killed >= 1, `nothing was SIGKILLed: ${JSON.stringify(report)}`);
  assert.equal(alive(helper), false, 'the stubborn helper is still running');
});

test('a profile close() cannot remove is a warning, never a failure', async () => {
  const chrome = await launch({});
  // A directory this user may not delete from: rm fails the same way every time.
  const locked = join(chrome.profile, 'Default', 'locked');
  mkdirSync(locked, { recursive: true });
  writeFileSync(join(locked, 'Cookies'), 'x');
  chmodSync(locked, 0o555);
  try {
    const warned = new Promise<string>((resolve) => process.once('warning', (w) => resolve(w.message)));

    const report = await chrome.close(); // resolves: the assertion is that it does not reject

    assert.equal(report.removed, false);
    assert.match(report.problem ?? '', new RegExp(`^could not clean up the Chrome profile ${chrome.profile}: removing it: EACCES`));
    assert.equal(await warned, report.problem);
    assert.equal(await chrome.close(), report, 'close() is not idempotent');
  } finally {
    chmodSync(locked, 0o755);
    rmSync(chrome.profile, { recursive: true, force: true });
  }
});

test(`a DevTools port written after 12s is waited for (the deadline is ${PORT_DEADLINE_MS / 1000}s)`, { timeout: 30_000 }, async () => {
  const chrome = await launch({ FAKE_PORT_DELAY_MS: '12000' });
  try {
    assert.equal(chrome.port, 9222);
  } finally {
    await chrome.close();
  }
});

test('a half-written DevToolsActivePort is not read as the port', async () => {
  const chrome = await launch({ FAKE_PARTIAL_PORT: '92', FAKE_PORT_DELAY_MS: '300' });
  try {
    assert.equal(chrome.port, 9222);
    assert.equal(chrome.browserPath, '/devtools/browser/fake-id');
  } finally {
    await chrome.close();
  }
});

test('no DevTools port by the deadline: the error names the file, the deadline and Chrome\'s stderr; Chrome is stopped and its profile removed', async () => {
  const pid = pidFile();
  await assert.rejects(
    launch({ FAKE_PORT_DELAY_MS: '-1', FAKE_PID_FILE: pid, FAKE_STDERR: '[fake] GPU process failed to start' }, { portDeadlineMs: 1000 }),
    (err: Error) => {
      assert.match(err.message, /did not write its DevTools port to \S+DevToolsActivePort within 1s: it never appeared; Chrome is still running/);
      assert.match(err.message, /\[fake\] GPU process failed to start/);
      const profile = /to (\S+)\/DevToolsActivePort/.exec(err.message)![1]!;
      assert.equal(existsSync(profile), false, 'the profile was left behind');
      return true;
    });
  assert.equal(alive(readPid(pid)), false, 'Chrome was left running');
});

test('Chrome exiting before it opens DevTools fails at once, with its exit code and stderr', async () => {
  const started = Date.now();
  await assert.rejects(launch({ FAKE_EXIT: '3', FAKE_STDERR: 'Missing X server or $DISPLAY' }),
    /Chrome exited \(code 3, signal null\) before writing DevToolsActivePort[^]*Missing X server/);
  assert.ok(Date.now() - started < 5000, 'waited for the port deadline instead of seeing the exit');
});

test('Chrome killed by a signal before it opens DevTools fails at once, naming the signal', async () => {
  const started = Date.now();
  await assert.rejects(launch({ FAKE_SIGNAL: 'SIGABRT' }), /Chrome exited \(code null, signal SIGABRT\) before writing DevToolsActivePort/);
  assert.ok(Date.now() - started < 5000, 'a crash by signal was reported as a port timeout');
});

test('a Chrome that cannot be started is an error, not a crash of the test process', async () => {
  await assert.rejects(launchChrome({ executable: join(scratch, 'no-such-chrome') }),
    /Chrome could not be started \(\S+no-such-chrome\): spawn \S+ ENOENT/);
});
