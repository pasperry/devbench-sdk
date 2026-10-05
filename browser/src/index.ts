/**
 * Dev Bench browser sensor (npm `devbench`, global `Devbench`; `ADT` kept as
 * an alias) — public entry point.
 *
 * Budgets, and why they are not negotiable, are in `budget.ts`. The short
 * version: this runs inside a customer's application, and a diagnostics tool
 * that slows it down or leaks memory is the worst failure available to us.
 */
export { Sensor, type SensorOptions, type SensorStats, type SlowThresholds } from './sensor.js';
export { BUDGET } from './budget.js';
export { parseDsn, type Dsn } from './dsn.js';
export { compute, template, normalizePath, type Signal, type Frame } from './fingerprint.js';
export { redactText, redactUrl, fieldValue, type RedactionConfig } from './redact.js';
export { installErrorCapture, parseStack } from './capture/errors.js';
export { installNetworkCapture } from './capture/network.js';
export { installVitalsCapture, routeTemplate } from './capture/vitals.js';
export { registerAngularModule, attachAngular, type AngularStatic } from './adapters/angularjs.js';
export { init, setUser, registerAngular, getSensor, close, type InitOptions, type InitScope, type UserInput } from './init.js';
export { ReadbackTracker, type Divergence } from './detect/readback.js';
export { isDisabled } from './safe.js';
export { TRACE_HEADER, formatTrace, parseTrace, newId, type Trace } from './trace.js';
export { SESSION_HEADER, SESSION_META, readSessionToken } from './trace.js';
export { HANDLED_HEADER, readHandled } from './trace.js';
export { RingBuffer } from './buffer.js';
export { CounterTable, type Count } from './counters.js';
export { Transport, type FlushRequest, type FlushResponse } from './transport.js';

/** SDK version. Bumped on every change to any SDK; all three SDKs share it. */
export const VERSION = '0.6.0';
