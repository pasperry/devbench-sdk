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
