/**
 * Script-tag install: the bundle initializes itself from its own tag.
 *
 *   <script src="https://unpkg.com/devbench/dist/devbench.min.js"
 *           data-dsn="https://<key>@adt-ingest.onrender.com"></script>
 *
 * Reads `data-dsn` (required) and `data-release` (optional) from
 * `document.currentScript`. Where that is null, the last script whose src
 * names this bundle and that carries `data-dsn` is used instead. Without a
 * `data-dsn`, nothing happens: a page that loads the bundle and calls
 * `Devbench.init` itself is untouched.
 */
import { init, type InitScope } from './init.js';
import type { Sensor } from './sensor.js';

interface ScriptLike {
  src?: string;
  dataset?: Record<string, string | undefined>;
  getAttribute?(name: string): string | null;
}

interface DocLike {
  currentScript?: ScriptLike | null;
  getElementsByTagName?(name: string): ArrayLike<ScriptLike>;
}

/** The bundle's own file names: devbench.js, devbench.min.js, adt.iife.js, ... */
const OWN_SRC = /(^|\/)(devbench|adt)[^/]*\.js(\?|#|$)/i;

export function autoInit(scope: InitScope & { document?: unknown }): Sensor | null {
  try {
    const doc = scope.document as DocLike | undefined;
    if (!doc) return null;

    const el = doc.currentScript || findOwnScript(doc);
    const dsn = data(el, 'dsn');
    if (!dsn) return null;

    return init({ dsn, release: data(el, 'release') || undefined }, scope);
  } catch {
    return null;
  }
}

function findOwnScript(doc: DocLike): ScriptLike | null {
  if (typeof doc.getElementsByTagName !== 'function') return null;
  const scripts = doc.getElementsByTagName('script');
  for (let i = scripts.length - 1; i >= 0; i--) {
    const s = scripts[i];
    if (s && OWN_SRC.test(s.src || '') && data(s, 'dsn')) return s;
  }
  return null;
}

function data(el: ScriptLike | null | undefined, name: string): string {
  if (!el) return '';
  const v = el.dataset?.[name] ?? (typeof el.getAttribute === 'function' ? el.getAttribute(`data-${name}`) : null);
  return typeof v === 'string' ? v.trim() : '';
}
