/**
 * Signal -> stable identity. This is a direct port of `internal/fingerprint`
 * (Go), and the two MUST agree.
 *
 * If they disagree, the same problem is counted twice: triage cost doubles and
 * one issue splits in half with neither half showing its true impact. That is
 * why `test/vectors.test.ts` checks this implementation against the same
 * `testdata/fingerprint_vectors.json` the Go tests use.
 *
 * Any change here is a change to the wire contract. See ARCHITECTURE_PROPOSAL.md
 * risk #1 before touching the rules.
 *
 * Note: the Go implementation gates each rule behind a cheap character scan,
 * because it runs in the sidecar against high log volume. This one does not —
 * a browser templates a handful of messages per session, and the branchless
 * version is smaller. The two must still produce identical output, which is
 * exactly what the shared vectors check.
 */

import { sha256Hex } from './sha256.js';

/** How many application frames take part in the identity. */
const MAX_FRAMES = 5;

/** How many trailing path segments survive normalization. */
const KEEP_SEGMENTS = 3;

export interface Frame {
  function: string;
  file: string;
  /** Recorded for humans. Never fingerprinted — see the module comment. */
  line?: number;
}

export interface Signal {
  kind: string;
  source: string;
  service?: string;
  type?: string;
  message?: string;
  frames?: Frame[];
}

export interface Normalized {
  kind: string;
  source: string;
  service: string;
  type: string;
  template: string;
  frames: string[];
}

export interface FingerprintResult {
  fp: string;
  normalized: Normalized;
}

/** Frames from dependencies and runtimes never participate: they move on every
 *  upgrade, and including them would make `npm update` look like a wave of new
 *  problems. */
const DEPENDENCY_FRAGMENTS = [
  '/node_modules/',
  '/vendor/',
  '/gems/',
  '/ruby/gems/',
  '/usr/local/go/src/',
  '/go/pkg/mod/',
  '/.bundle/',
  '<anonymous>',
];

/** Ordered. More specific patterns run first, or a UUID decomposes into <num>s. */
const TEMPLATE_RULES: Array<[RegExp, string]> = [
  [/\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b/gi, '<uuid>'],
  [/\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?/g, '<ts>'],
  [/\b\d{4}-\d{2}-\d{2}\b/g, '<date>'],
  [/\b[^\s<>@]+@[^\s<>@]+\.[A-Za-z]{2,}\b/g, '<email>'],
  [/\b[a-z][a-z0-9+.-]*:\/\/\S+/g, '<url>'],
  [/\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b/g, '<ip>'],
  [/\b(?:0x)?[0-9a-fA-F]{8,}\b/g, '<hex>'],
  [/"[^"]*"/g, '<str>'],
  [/'[^']*'/g, '<str>'],
  [/\b\d+(?:\.\d+)?\s?(?:ms|s|us|ns|µs)\b/g, '<dur>'],
  [/\b\d+(?:\.\d+)?\b/g, '<num>'],
];

/**
 * Reduces a message to its shape, replacing values that vary between
 * occurrences with holes. Also what collapses a log stream into templates.
 */
export function template(msg: string | undefined): string {
  let out = (msg ?? '').trim();
  if (out === '') return '';

  for (const [re, with_] of TEMPLATE_RULES) {
    out = out.replace(re, with_);
  }

  return out.replace(/\s+/g, ' ').trim();
}

/**
 * Reduces a file path to a stable repo-relative tail.
 *
 * Absolute build paths differ between a laptop, CI, and each deploy; bundle
 * URLs carry cache-busting query strings. Neither should change a problem's
 * identity.
 */
export function normalizePath(p: string | undefined): string {
  let out = (p ?? '').trim();
  if (out === '') return '';

  const scheme = out.indexOf('://');
  if (scheme >= 0) {
    const rest = out.slice(scheme + 3);
    const slash = rest.indexOf('/');
    out = slash >= 0 ? rest.slice(slash) : rest;
  }

  const q = out.search(/[?#]/);
  if (q >= 0) out = out.slice(0, q);

  out = out.replace(/\\/g, '/');

  const segments = out.replace(/^\/+|\/+$/g, '').split('/');
  const kept = segments.length > KEEP_SEGMENTS ? segments.slice(-KEEP_SEGMENTS) : segments;
  const last = kept.length - 1;
  kept[last] = stripAssetDigest(kept[last] ?? '');
  return kept.join('/');
}

// A bundle filename carrying a build hash: application-<64 hex>.js
// (Sprockets), main.<hex>.js and vendor.<hex>.chunk.js (webpack),
// index-<8 base64url>.js (Vite). Mirrors Go's stripAssetDigest exactly; the
// shared vectors pin both. Without it every deploy makes every browser error
// a new fingerprint (risk #1).
const ASSET_DIGEST = /^(.+)[-.]([A-Za-z0-9_]{8,})((?:\.chunk|\.bundle)?\.(?:js|mjs|cjs|css)(?:\.map)?)$/;

function stripAssetDigest(name: string): string {
  const m = ASSET_DIGEST.exec(name);
  if (!m || !looksLikeDigest(m[2] ?? '')) return name;
  return (m[1] ?? '') + (m[3] ?? '');
}

function looksLikeDigest(tok: string): boolean {
  let hasDigit = false;
  let hasUpper = false;
  let hexOnly = true;
  for (const ch of tok) {
    if (ch >= '0' && ch <= '9') hasDigit = true;
    else if (ch >= 'a' && ch <= 'f') continue;
    else if (ch >= 'A' && ch <= 'Z') {
      hasUpper = true;
      hexOnly = false;
    } else hexOnly = false;
  }
  if (hexOnly && hasDigit && tok.length >= 8) return true;
  return tok.length === 8 && hasDigit && hasUpper;
}

function isDependencyFrame(f: Frame): boolean {
  const hay = `${f.file ?? ''} ${f.function ?? ''}`;
  return DEPENDENCY_FRAGMENTS.some((frag) => hay.indexOf(frag) >= 0);
}

function normalizeFrames(frames: Frame[] | undefined): string[] {
  const out: string[] = [];
  for (const f of frames ?? []) {
    if (isDependencyFrame(f)) continue;

    const fn = (f.function ?? '').trim();
    const file = normalizePath(f.file);
    if (fn === '' && file === '') continue;

    out.push(`${fn}@${file}`);
    if (out.length === MAX_FRAMES) break;
  }
  return out;
}

/** The exact bytes that get hashed. Separators cannot occur in the fields. */
function canonical(n: Normalized): string {
  return [
    'v1', n.kind, n.source, n.service, n.type, n.template, n.frames.join('\u001f'),
  ].join('\u001e');
}

export class FingerprintError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'FingerprintError';
  }
}

/**
 * Computes the fingerprint for `sig`.
 *
 * Pure and synchronous: the same signal always produces the same hash, which is
 * what lets a sensor count repeats locally without talking to anyone.
 */
export function compute(sig: Signal): FingerprintResult {
  if (!sig.kind) throw new FingerprintError('kind is required');
  if (!sig.source) throw new FingerprintError('source is required');
  if (!sig.type && !sig.message && !(sig.frames && sig.frames.length)) {
    throw new FingerprintError('signal has no type, message, or frames');
  }

  const normalized: Normalized = {
    kind: sig.kind,
    source: sig.source,
    service: sig.service ?? '',
    type: (sig.type ?? '').trim(),
    template: template(sig.message),
    frames: normalizeFrames(sig.frames),
  };

  return { fp: sha256Hex(canonical(normalized)), normalized };
}
