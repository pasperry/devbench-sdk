// Bundles src + test TypeScript to runnable JS for `node --test`.
// esbuild is already a dependency for size measurement, so this adds nothing.
import { build } from 'esbuild';
import { readdirSync, rmSync } from 'node:fs';

rmSync('.test-build', { recursive: true, force: true });

const entries = readdirSync('test')
  .filter((f) => f.endsWith('.test.ts'))
  .map((f) => `test/${f}`);

await build({
  entryPoints: entries,
  outdir: '.test-build',
  bundle: true,
  platform: 'node',
  format: 'esm',
  target: 'node22',
  sourcemap: 'inline',
  external: ['node:*'],
});
