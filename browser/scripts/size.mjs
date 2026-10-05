// Enforces the bundle budget from src/budget.ts (proposal 4.1).
// This fails the build, not a report someone reads later: a budget nobody
// enforces is a number in a document.
import { build } from 'esbuild';
import { gzipSync } from 'node:zlib';
import { iife } from './bundle.mjs';

const BUDGET_BYTES = 18 * 1024;

// Exactly what ships as dist/devbench.min.js (scripts/bundle.mjs).
const result = await build({ ...iife, write: false });

const raw = result.outputFiles[0].contents;
const gz = gzipSync(raw, { level: 9 });
const pct = ((gz.length / BUDGET_BYTES) * 100).toFixed(1);

console.log(`minified:       ${raw.length.toLocaleString()} bytes`);
console.log(`gzipped:        ${gz.length.toLocaleString()} bytes`);
console.log(`budget:         ${BUDGET_BYTES.toLocaleString()} bytes`);
console.log(`used:           ${pct}% of budget`);

if (gz.length > BUDGET_BYTES) {
  console.error(`\nFAIL: bundle is ${gz.length - BUDGET_BYTES} bytes over budget.`);
  console.error('The budget is a hard constraint (ARCHITECTURE_PROPOSAL.md principle #2).');
  console.error('Move the new code behind the lazily-loaded evidence path, or make the case for raising it.');
  process.exit(1);
}
