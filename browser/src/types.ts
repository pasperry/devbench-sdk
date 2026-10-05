/** Shared sensor types. */
import type { Frame } from './fingerprint.js';

export type SignalKind = 'error' | 'silent_failure' | 'frustration' | 'degradation';

/** A semantic user action. Roles and labels, never raw DOM (proposal 4.1). */
export interface Intent {
  id: string;
  /** e.g. "click", "submit", "navigate" */
  action: string;
  /** Accessible label or test id — what the user thinks they pressed. */
  target: string;
  at: number;
}

/** One observed HTTP exchange, with URL and body reduced to shape. */
export interface HttpRecord {
  intentId: string;
  method: string;
  /** Templated path: /api/customers/<id> */
  urlTemplate: string;
  status: number;
  /** Whether the app treated this as success. A 200 can still be a failure. */
  ok: boolean;
  /**
   * How many failures the server handled while producing this response, from
   * the `x-adt-handled` header. Absent when the server SDK is not installed,
   * or when the header was not readable cross-origin.
   */
  handled?: number;
  at: number;
  /** Field names written by a mutation; values are never recorded here. */
  writtenFields?: string[];
  /** Field values, only where allow-listed, for readback comparison. */
  writtenValues?: Record<string, string>;
}

export interface CapturedSignal {
  kind: SignalKind;
  type: string;
  message: string;
  frames?: Frame[];
  intentId?: string;
  at: number;
}

/**
 * Who was affected (SERVER_SDK_SPEC.md, "Capability 5 — Identity").
 *
 * Normalized by `Sensor.setUser` before it is held. Travels in its own field
 * on a count and nowhere else: never in a fingerprint, a message, or evidence.
 */
export interface Identity {
  email?: string;
  account?: string;
}
