/**
 * The footprint budget from ARCHITECTURE_PROPOSAL.md 4.1.
 *
 * Design principle #2: a diagnostics tool that slows down or leaks memory in
 * the customer's app is the worst possible failure. These are hard numbers, not
 * aspirations — the soak test asserts them and the size check fails the build.
 */
export const BUDGET = {
  /** Core bundle, gzipped. Evidence packaging loads lazily, only on a trigger. */
  bundleGzipBytes: 18 * 1024,

  /** Every buffer below is fixed-size and preallocated: memory is bounded by
   *  construction rather than by hoping the app is well behaved. */
  maxSignals: 128,
  maxIntents: 64,
  maxBreadcrumbs: 64,
  maxPendingVerifications: 32,

  /** Network. At most one beacon per interval, and never larger than this.
   *  Short is free: a flush with nothing counted returns before any network
   *  call, so a quiet page sends nothing however often the loop ticks. */
  flushIntervalMs: 15_000,
  maxBeaconBytes: 8 * 1024,
  maxCountsPerFlush: 64,
  /** Distinct identities listed per count per flush; more only increment
   *  `users_overflow` (SERVER_SDK_SPEC.md, "Phase 1 — counts"). */
  maxUsersPerCount: 20,

  /** Work slice: analysis runs in idle time and yields well inside a frame. */
  maxTaskMs: 4,

  /** Retry once with jitter, then discard. A diagnostics tool must never queue
   *  unboundedly or block a request. */
  maxFlushRetries: 1,
} as const;

export type Budget = typeof BUDGET;
