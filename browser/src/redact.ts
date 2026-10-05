/**
 * Edge redaction (ARCHITECTURE_PROPOSAL.md 3.5), and risk #2.
 *
 * Evidence comes from a real customer's real users. Redaction happens here,
 * before anything leaves the browser, because it cannot be retrofitted: bundles
 * collected before the rule existed are already gone.
 *
 * The posture is allow-list. Input values are never captured unless a field is
 * explicitly listed in the control-repo config. Deny-lists fail open, and
 * failing open with someone's personal data is not a failure mode we accept.
 */

import { sha256Hex } from './sha256.js';

/**
 * Ordered most-specific first.
 *
 * The phone pattern is deliberately greedy — it has to be, to catch the many
 * shapes a phone number takes — so it will happily swallow an SSN or a card
 * number if it runs first. Everything is still redacted either way, but the
 * label would be wrong, and a mislabelled redaction is a worse signal for
 * whoever reads the issue later.
 */
const PII_PATTERNS: Array<[RegExp, string]> = [
  [/\b[^\s<>@]+@[^\s<>@]+\.[A-Za-z]{2,}\b/g, '<redacted:email>'],
  [/\b\d{3}-\d{2}-\d{4}\b/g, '<redacted:ssn>'],
  [/\b(?:\d[ -]*?){13,19}\b/g, '<redacted:card>'],
  // No leading \b. A word boundary cannot match before `+`, so the previous
  // pattern left the `+` of `+1 (555) 123-4567` outside the redaction. Caught
  // by testdata/redaction_vectors.json, which the sidecar checks too — the
  // two implementations have to agree or one leaks what the other removes.
  [/\+?\d[\d\s().-]{7,}\d/g, '<redacted:phone>'],
];

export interface RedactionConfig {
  /** Field names whose values may be captured verbatim. Empty by default. */
  allowInputValues: string[];
}

export const DEFAULT_REDACTION: RedactionConfig = { allowInputValues: [] };

/**
 * A random per-session salt for redacted comparison tokens.
 *
 * Per-session, not global, so a token cannot be correlated across sessions or
 * users, and cannot be reversed with a precomputed table. Readback comparison
 * only ever happens within one session, so this costs nothing.
 */
export function newSessionSalt(): string {
  return `${Math.random().toString(36).slice(2)}${Date.now().toString(36)}`;
}

/** Replaces PII-shaped substrings in free text (messages, log exemplars). */
export function redactText(input: string): string {
  let out = input;
  for (const [re, with_] of PII_PATTERNS) out = out.replace(re, with_);
  return out;
}

/**
 * Decides what to record for a form field.
 *
 * Returns the value only when the field is explicitly allow-listed. Otherwise
 * it returns a salted comparison token: enough to tell whether two values are
 * the same, never enough to learn what either one is.
 *
 * It must be a hash, not a length. `old@example.com` and `new@example.com` are
 * both fifteen characters, so a length token makes the brief's own canonical
 * silent failure — an email update that never persisted — invisible to the
 * detector. That is the exact bug this product exists to find, so getting it
 * wrong here would be quietly self-defeating.
 */
export function fieldValue(name: string, value: string, cfg: RedactionConfig, salt: string): string {
  if (cfg.allowInputValues.indexOf(name) >= 0) return value;
  if (value === '') return '<empty>';
  return `<h:${sha256Hex(salt + '\u001e' + value).slice(0, 12)}>`;
}

/**
 * Reduces a URL to a template: keeps the path shape, drops identifiers and the
 * entire query string.
 *
 * `/api/customers/9912?token=abc` becomes `/api/customers/<id>`. The path shape
 * is what identifies an endpoint; the identifiers and query are where the
 * customer data and the credentials are.
 */
export function redactUrl(raw: string): string {
  let path = raw;

  const scheme = path.indexOf('://');
  if (scheme >= 0) {
    const rest = path.slice(scheme + 3);
    const slash = rest.indexOf('/');
    path = slash >= 0 ? rest.slice(slash) : '/';
  }

  const q = path.search(/[?#]/);
  if (q >= 0) path = path.slice(0, q);

  return path
    .split('/')
    .map((seg) => {
      if (seg === '') return seg;
      if (/^\d+$/.test(seg)) return '<id>';
      if (/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(seg)) return '<uuid>';
      if (/^[0-9a-f]{16,}$/i.test(seg)) return '<hash>';
      return seg;
    })
    .join('/');
}
