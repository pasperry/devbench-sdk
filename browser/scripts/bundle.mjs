// The script-tag bundle's esbuild options, shared by build.mjs and size.mjs so
// the budget measures exactly what ships.
//
// The global is `Devbench`; the footer keeps `ADT` as an alias of the same
// object, so installs written against the old global work unchanged.
export const iife = {
  entryPoints: ['src/browser.ts'],
  bundle: true,
  minify: true,
  target: ['es2017'],
  legalComments: 'none',
  format: 'iife',
  globalName: 'Devbench',
  footer: { js: 'var ADT=Devbench;' },
};

// The chat widget (src/widget/index.ts): a separate script the core SDK
// inserts only when the chat API says this user gets a widget
// (SERVER_SDK_SPEC.md Capability 6). Never part of the core bundle or its
// budget; it has its own (WIDGET_BUDGET_BYTES in size.mjs).
import { readFileSync } from 'node:fs';

const pkgVersion = JSON.parse(readFileSync(new URL('../package.json', import.meta.url), 'utf8')).version;

export const widget = {
  entryPoints: ['src/widget/index.ts'],
  bundle: true,
  minify: true,
  target: ['es2017'],
  legalComments: 'none',
  format: 'iife',
  define: { __WIDGET_VERSION__: JSON.stringify(pkgVersion) },
};
