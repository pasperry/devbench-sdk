/**
 * Error capture: uncaught exceptions, unhandled rejections, console.error,
 * (via ./network.ts) failed and slow fetch and XMLHttpRequest calls, and (via
 * ./vitals.ts) slow page loads and interactions.
 *
 * Every hook is wrapped so a failure inside ADT can never surface as a failure
 * in the host app, and every hook chains to whatever was installed before us.
 * Replacing someone's existing error handler would be a way to *cause* the
 * silent failures this product exists to find.
 */
import { safeVoid } from './../safe.js';
import { template, type Frame } from './../fingerprint.js';
import type { Sensor } from './../sensor.js';
import { installNetworkCapture, type NetworkCaptureTargets } from './network.js';
import { installVitalsCapture, type VitalsCaptureTargets } from './vitals.js';

/** Parses a V8/SpiderMonkey stack into frames. Line numbers are kept for humans
 *  and ignored by fingerprinting. */
export function parseStack(stack: string | undefined): Frame[] {
  if (!stack) return [];

  const out: Frame[] = [];
  for (const raw of stack.split('\n')) {
    const line = raw.trim();
    if (line === '' || line.startsWith('Error')) continue;

    // "at Ctrl.save (https://host/app.js:12:9)" | "Ctrl.save@https://host/app.js:12:9"
    let m = /^at\s+(.+?)\s+\((.+?):(\d+):(\d+)\)$/.exec(line);
    if (!m) m = /^at\s+()(.+?):(\d+):(\d+)$/.exec(line);
    if (!m) m = /^(.+?)@(.+?):(\d+):(\d+)$/.exec(line);
    if (!m) continue;

    out.push({ function: (m[1] ?? '').trim(), file: m[2] ?? '', line: Number(m[3] ?? 0) });
    if (out.length >= 20) break;
  }
  return out;
}

/** Letters of any script with letters; leaves out digits, punctuation and
 *  the symbol blocks banners are drawn with (U+0080–U+00BF, U+2000–U+2BFF:
 *  box drawing, blocks, arrows, dashes) and emoji (surrogate pairs). ES2017
 *  has no \p{L}. */
const LETTER = /[A-Za-z\u00C0-\u1FFF\u2C00-\uD7FF]/;
/** A template hole such as <num>, <uuid>, <str>. */
const HOLE = /<[a-z]+>/g;
/** `String(x)` of a non-Error object: `[object Event]`, `[object Object]`. */
const OBJECT_TAGS = /^\[object [A-Za-z0-9_$]*\](?: \[object [A-Za-z0-9_$]*\])*$/;
/** A bare code: `Error code: 31005`, `Error: 20101`, `code 31009`. */
const BARE_CODE = /^(?:err(?:or)?\s*)?(?:code|status|errno)?\s*[:#=-]?\s*<num>\.?$/i;

/**
 * True for a console.error line with nothing in it to tell one incident from
 * another (#518): no letters once its values are holed (banners of `=`,
 * an empty call), only `[object <Name>]`, or a bare code (`Error code: N`,
 * where the number is a hole). Fingerprinted, every such line from every
 * incident counts as one problem; the incident's other lines still record it.
 */
export function isContentFreeConsoleLine(message: string): boolean {
  const t = template(message);
  if (!LETTER.test(t.replace(HOLE, ' '))) return true;
  return OBJECT_TAGS.test(t) || BARE_CODE.test(t);
}

export interface ErrorCaptureTargets extends NetworkCaptureTargets, VitalsCaptureTargets {
  addEventListener?: Window['addEventListener'];
  console?: { error?: (...args: unknown[]) => void };
}

/** Installs the hooks. Returns a function that removes them again. */
export function installErrorCapture(sensor: Sensor, target: ErrorCaptureTargets): () => void {
  const teardown: Array<() => void> = [];

  safeVoid(() => {
    if (target.addEventListener) {
      const onError = (ev: Event): void => {
        const e = ev as ErrorEvent;
        const err = e.error as Error | undefined;
        sensor.record(
          'error',
          err?.name ?? 'Error',
          err?.message ?? e.message ?? 'unknown error',
          parseStack(err?.stack),
        );
      };

      const onRejection = (ev: Event): void => {
        const reason = (ev as PromiseRejectionEvent).reason as Error | string | undefined;
        const err = reason instanceof Error ? reason : undefined;
        sensor.record(
          'error',
          err?.name ?? 'UnhandledRejection',
          err?.message ?? String(reason ?? 'unknown rejection'),
          parseStack(err?.stack),
        );
      };

      target.addEventListener('error', onError);
      target.addEventListener('unhandledrejection', onRejection);
      teardown.push(() => {
        (target as unknown as Window).removeEventListener?.('error', onError);
        (target as unknown as Window).removeEventListener?.('unhandledrejection', onRejection);
      });
    }

    // console.error is wrapped, never replaced: the original is always called,
    // first, so the app's own logging and any other tool still sees everything.
    const con = target.console;
    if (con && typeof con.error === 'function') {
      const unwrapped = con.error;
      const original = unwrapped.bind(con);
      con.error = (...args: unknown[]): void => {
        original(...args);
        safeVoid(() => {
          if (sensor.isConsoleMuted()) return;
          const first = args[0];
          const err = first instanceof Error ? first : undefined;
          const message = err?.message ?? args.map((a) => String(a)).join(' ');
          // Content-free lines (#518) are dropped: they would merge unrelated
          // incidents into one fingerprint. An Error keeps its name and stack.
          if (!err && isContentFreeConsoleLine(message)) return;
          sensor.record('error', err?.name ?? 'ConsoleError', message, parseStack(err?.stack));
        });
      };
      teardown.push(() => {
        con.error = unwrapped;
      });
    }
  });

  // Global HTTP capture rides on the same one-line install, so
  // `installErrorCapture(sensor, window)` catches what Sentry's does.
  teardown.push(installNetworkCapture(sensor, target));
  // And so does slowness users feel: slow page loads and interactions.
  teardown.push(installVitalsCapture(sensor, target));

  return () => {
    for (const fn of teardown) safeVoid(fn);
  };
}
