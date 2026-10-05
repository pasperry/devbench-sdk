// Builds what the npm package and the CDN serve:
//
//   dist/devbench.min.js   script tag (unpkg/jsdelivr default), global Devbench (+ ADT alias)
//   dist/devbench.js       the same, unminified
//   dist/devbench.esm.js   npm `import` entry, no side effects on import
//   dist/types/            TypeScript declarations
//   dist/adt.iife.js       legacy name of devbench.min.js, for existing installs
//   dist/adt.esm.js        legacy ESM name
import { build } from 'esbuild';
import { execFileSync } from 'node:child_process';
import { rmSync } from 'node:fs';
import { iife } from './bundle.mjs';

rmSync('dist', { recursive: true, force: true });

const esm = { entryPoints: ['src/index.ts'], bundle: true, target: ['es2017'], legalComments: 'none', format: 'esm' };

await build({ ...iife, outfile: 'dist/devbench.min.js' });
await build({ ...iife, minify: false, outfile: 'dist/devbench.js' });
await build({ ...iife, outfile: 'dist/adt.iife.js' });
await build({ ...esm, outfile: 'dist/devbench.esm.js' });
await build({ ...esm, minify: true, outfile: 'dist/adt.esm.js' });

execFileSync(
  process.execPath,
  ['node_modules/typescript/bin/tsc', '-p', 'tsconfig.json', '--noEmit', 'false', '--declaration', '--emitDeclarationOnly', '--outDir', 'dist/types'],
  { stdio: 'inherit' },
);

console.log('built dist/devbench.{min.js,js,esm.js}, dist/types, and legacy dist/adt.{iife,esm}.js');
