// Enforces the bundle budgets (proposal 4.1).
// This fails the build, not a report someone reads later: a budget nobody
// enforces is a number in a document.
//
// Two numbers: the core SDK (every page that installs Dev Bench) and the chat
// widget (only pages whose user the chat API admits; SERVER_SDK_SPEC.md
// Capability 6). The widget must never end up inside the core.
import { build } from 'esbuild';
import { createHash } from 'node:crypto';
import { gzipSync } from 'node:zlib';
import { iife, widget } from './bundle.mjs';

const BUDGET_BYTES = 18 * 1024;
const WIDGET_BUDGET_BYTES = 8 * 1024;

async function measure(label, options, budget) {
  const result = await build({ ...options, write: false });
  const raw = result.outputFiles[0].contents;
  const gz = gzipSync(raw, { level: 9 });
  const pct = ((gz.length / budget) * 100).toFixed(1);
  console.log(`${label}`);
  console.log(`  minified:     ${raw.length.toLocaleString()} bytes`);
  console.log(`  gzipped:      ${gz.length.toLocaleString()} bytes`);
  console.log(`  budget:       ${budget.toLocaleString()} bytes`);
  console.log(`  used:         ${pct}% of budget`);
  return { raw, gz };
}

// Exactly what ships as dist/devbench.min.js (scripts/bundle.mjs).
const core = await measure('core (dist/devbench.min.js)', iife, BUDGET_BYTES);
// Exactly what ships as dist/devbench-widget.min.js.
const w = await measure('chat widget (dist/devbench-widget.min.js)', widget, WIDGET_BUDGET_BYTES);
console.log(`  integrity:    sha384-${createHash('sha384').update(w.raw).digest('base64')}`);

let failed = false;
if (core.gz.length > BUDGET_BYTES) {
  console.error(`\nFAIL: the core bundle is ${core.gz.length - BUDGET_BYTES} bytes over budget.`);
  console.error('The budget is a hard constraint (ARCHITECTURE_PROPOSAL.md principle #2).');
  console.error('Move the new code behind the lazily-loaded evidence path, or make the case for raising it.');
  failed = true;
}
if (w.gz.length > WIDGET_BUDGET_BYTES) {
  console.error(`\nFAIL: the chat widget is ${w.gz.length - WIDGET_BUDGET_BYTES} bytes over its budget.`);
  failed = true;
}
// The widget's UI code must not have been pulled into the core.
// Markers only the widget has: its element name and its conversation routes.
const coreText = Buffer.from(core.raw).toString('utf8');
if (coreText.includes('devbench-chat') || coreText.includes('/v1/chat/conversations')) {
  console.error('\nFAIL: the chat widget UI is inside the core bundle; it must load lazily.');
  failed = true;
}
if (failed) process.exit(1);
