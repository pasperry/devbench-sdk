/**
 * The script-tag bundle's entry (dist/devbench.js, devbench.min.js and the
 * legacy adt.iife.js): the whole public API, plus auto-init from the tag's
 * `data-dsn`. The npm entry (index.ts) has no side effects on import.
 */
import { autoInit } from './autoinit.js';

export * from './index.js';
export { autoInit } from './autoinit.js';

autoInit(globalThis as unknown as Parameters<typeof autoInit>[0]);
