/**
 * Detector 1 — write-then-readback divergence.
 *
 * This is the canonical case from the brief: a user changes a customer's email,
 * the update fails silently inside a rescue block, the request returns 200, the
 * UI moves on, and a colleague sees the old address days later. No exception,
 * no error, nothing for an error monitor to key off.
 *
 * The check is deterministic: the client observed a mutation that reported
 * success, and a later read of the same resource does not reflect what was
 * written. No model, no heuristic, no judgment call — which is exactly why this
 * is one of the two detectors allowed to create issues in v1 (proposal 3.2).
 *
 * Rung 1 of proposal 3.7: verification runs in the user's own session through
 * the app's own API, so it needs no database credential from anyone.
 */
import { BUDGET } from './../budget.js';
import type { HttpRecord } from './../types.js';

export interface PendingWrite {
  intentId: string;
  urlTemplate: string;
  /** The concrete resource URL, so a later GET can be matched to it. */
  resourceKey: string;
  fields: Record<string, string>;
  at: number;
}

export interface Divergence {
  urlTemplate: string;
  field: string;
  intentId: string;
  /** Present only for allow-listed fields; otherwise a shape marker. */
  wrote: string;
  read: string;
}

/**
 * Tracks writes that reported success and checks subsequent reads against them.
 *
 * Bounded by construction: at most `maxPending` writes are tracked, oldest
 * evicted first.
 */
export class ReadbackTracker {
  private pending: PendingWrite[] = [];

  constructor(private readonly maxPending: number = BUDGET.maxPendingVerifications) {}

  /** Records a mutation the app reported as successful. */
  recordWrite(rec: HttpRecord, resourceKey: string): void {
    if (!rec.ok) return;
    if (!rec.writtenValues || Object.keys(rec.writtenValues).length === 0) return;

    this.pending.push({
      intentId: rec.intentId,
      urlTemplate: rec.urlTemplate,
      resourceKey,
      fields: { ...rec.writtenValues },
      at: rec.at,
    });

    if (this.pending.length > this.maxPending) this.pending.shift();
  }

  /**
   * Compares a read of `resourceKey` against any pending write to it.
   *
   * Returns every field whose stored value contradicts what was written. The
   * pending write is consumed either way: a read that agrees is proof the write
   * landed, and a read that disagrees has done its job.
   */
  checkRead(resourceKey: string, body: Record<string, unknown>): Divergence[] {
    const out: Divergence[] = [];
    const remaining: PendingWrite[] = [];

    for (const write of this.pending) {
      if (write.resourceKey !== resourceKey) {
        remaining.push(write);
        continue;
      }

      for (const [field, wrote] of Object.entries(write.fields)) {
        if (!(field in body)) continue;

        const read = String(body[field] ?? '');
        if (read !== wrote) {
          out.push({ urlTemplate: write.urlTemplate, field, intentId: write.intentId, wrote, read });
        }
      }
    }

    this.pending = remaining;
    return out;
  }

  /**
   * Writes never confirmed by a read. These are not divergences — they are
   * unverified, and they are what the next-session check settles (proposal 3.7
   * rung 1): the pending list is persisted and re-checked on the user's next
   * visit, which is what makes "we noticed this wasn't working for you" possible
   * days later.
   */
  unverified(): PendingWrite[] {
    return this.pending.slice();
  }

  get pendingCount(): number {
    return this.pending.length;
  }
}
