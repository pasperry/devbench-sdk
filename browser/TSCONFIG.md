`tsconfig.json` sets `"types": []` on purpose.

`src/` targets a browser. Without Node's ambient types, a stray `process`,
`Buffer`, or `require` fails typecheck here rather than failing at runtime in a
customer's app. Tests and build scripts use `tsconfig.test.json`, which adds
`"types": ["node"]`.
