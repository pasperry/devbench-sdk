/**
 * Fingerprint -> occurrence counter. The whole of phase 1.
 *
 * ARCHITECTURE_PROPOSAL.md 3.3, principle #1: a repeat occurrence of a known
 * problem costs a counter increment, not an upload. A noisy app throwing
 * thousands of errors a day produces a handful of entries here with accurate
 * counts, rather than thousands of network calls.
 */
import { BUDGET } from './budget.js';
import type { Identity } from './types.js';

export interface Count {
  fp: string;
  /** Occurrences since the last flush. */
  n: number;
  /** Unix seconds. */
  first: number;
  last: number;
  /**
   * The signal class: error, silent_failure, frustration, degradation.
   *
   * Metadata, not content — a short enum, no user data — and the server needs it
   * to know which detector produced a fingerprint and whether that class is
   * enabled for the tenant. Without it every signal lands labelled "error",
   * which is exactly the bug this field was added to fix.
   */
  kind: string;
  /**
   * The distinct identities held while this fingerprint was recorded since the
   * last flush, at most `BUDGET.maxUsersPerCount`. Omitted, with
   * `users_overflow`, when no identity was set.
   */
  users?: Identity[];
  /** Further identities beyond the list. */
  users_overflow?: number;
}

/** The identities seen for one fingerprint in the current window. */
interface UserSet {
  seen: Map<string, Identity>;
  overflow: number;
  /**
   * The last identity counted as overflow. Remembering every overflowed
   * identity would make memory grow with the number of users, so only the
   * most recent is kept: a browser holds one user at a time, so its repeats
   * arrive consecutively and are not counted twice. Interleaved users past the
   * cap can be over-counted; the list itself is always exact.
   */
  lastOverflowKey: string;
}

export class CounterTable {
  private readonly counts = new Map<string, Count>();
  private readonly users = new Map<string, UserSet>();
  private overflowed = 0;

  constructor(
    private readonly max: number = BUDGET.maxCountsPerFlush,
    private readonly maxUsers: number = BUDGET.maxUsersPerCount,
  ) {}

  /**
   * Records one occurrence of `fp`.
   *
   * Bounded: once `max` distinct fingerprints are held, further *new*
   * fingerprints are counted as overflow rather than admitted. Existing ones
   * still increment, so a flood of novel signatures cannot grow this map.
   */
  record(fp: string, kind: string, atSeconds: number, user?: Identity | null, userKey?: string): void {
    const existing = this.counts.get(fp);
    if (existing) {
      existing.n++;
      existing.last = atSeconds;
    } else if (this.counts.size >= this.max) {
      this.overflowed++;
      return;
    } else {
      this.counts.set(fp, { fp, n: 1, first: atSeconds, last: atSeconds, kind });
    }

    if (user && userKey) this.recordUser(fp, user, userKey);
  }

  private recordUser(fp: string, user: Identity, key: string): void {
    let set = this.users.get(fp);
    if (!set) {
      set = { seen: new Map(), overflow: 0, lastOverflowKey: '' };
      this.users.set(fp, set);
    }
    if (set.seen.has(key)) return;
    if (set.seen.size < this.maxUsers) {
      set.seen.set(key, user);
      return;
    }
    if (set.lastOverflowKey === key) return;
    set.lastOverflowKey = key;
    set.overflow++;
  }

  /** Returns the counts and resets. Called once per flush interval. */
  drain(): { counts: Count[]; overflowed: number } {
    const counts = Array.from(this.counts.values());
    for (const c of counts) {
      const set = this.users.get(c.fp);
      if (set && set.seen.size > 0) {
        c.users = Array.from(set.seen.values());
        c.users_overflow = set.overflow;
      }
    }
    const overflowed = this.overflowed;
    this.counts.clear();
    this.users.clear();
    this.overflowed = 0;
    return { counts, overflowed };
  }

  get size(): number {
    return this.counts.size;
  }

  /** Distinct fingerprints refused because the table was full. */
  get overflowCount(): number {
    return this.overflowed;
  }
}
