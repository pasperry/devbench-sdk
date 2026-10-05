/**
 * The never-throw boundary.
 *
 * Every entry point the host application can reach passes through here. A bug
 * in ADT must never surface as a bug in the customer's app: we swallow, we
 * disable ourselves, and we stay quiet.
 *
 * This is the one place in the codebase where swallowing an error is correct —
 * everywhere else it is the exact behavior the product exists to find.
 */

let disabled = false;
let lastError: unknown = null;

/** True once an internal failure has shut the sensor down for this session. */
export function isDisabled(): boolean {
  return disabled;
}

/** The internal error that caused shutdown, for the SDK's own tests. */
export function internalError(): unknown {
  return lastError;
}

/** Disables the sensor for the remainder of the session. */
export function disable(err?: unknown): void {
  disabled = true;
  if (err !== undefined) lastError = err;
}

/** Test-only: restore the initial state. */
export function resetForTests(): void {
  disabled = false;
  lastError = null;
}

/**
 * Runs `fn`, returning `fallback` if the sensor is disabled or `fn` throws.
 *
 * A throw disables the sensor: if our own code is broken we would rather lose
 * all telemetry than repeatedly run broken code inside someone's application.
 */
export function safe<T>(fn: () => T, fallback: T): T {
  if (disabled) return fallback;
  try {
    return fn();
  } catch (err) {
    disable(err);
    return fallback;
  }
}

/** `safe` for functions whose result is not used. */
export function safeVoid(fn: () => void): void {
  safe(fn, undefined as void);
}
