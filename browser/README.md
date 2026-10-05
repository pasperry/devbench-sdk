# devbench (browser)

The Dev Bench browser sensor. Deterministic, always-on, and strictly budgeted:
errors, failed and slow requests, slow pages and interactions, counted per
problem — traffic grows with distinct problems, not with page views.

## Install

**One script tag.** Put it before your app's scripts:

```html
<script src="https://unpkg.com/devbench@0.5.0/dist/devbench.min.js"
        data-dsn="https://<ingest key>@adt-ingest.onrender.com"
        data-release="<build sha>"></script>
```

That is the whole install: the bundle reads `data-dsn` from its own tag and
calls `Devbench.init` itself. `data-release` is optional. Without `data-dsn`
the tag does nothing but define `Devbench`. (jsDelivr works the same:
`https://cdn.jsdelivr.net/npm/devbench@0.5.0/dist/devbench.min.js`. Serving
the file yourself works too — copy `dist/devbench.min.js`.)

**Or npm, and one call:**

```sh
npm install devbench
```

```js
import * as Devbench from 'devbench';

Devbench.init({
  dsn: 'https://<ingest key>@adt-ingest.onrender.com',
  release: BUILD_SHA,                              // optional
  user: { email: me.email, account: me.accountId }, // optional, if known now
});
```

The DSN is the browser key Dev Bench minted for this environment
(`--source client`). It is public by design: the key is write-only. An invalid
DSN never throws — it logs one `console.warn` and the sensor stays inert.
`enabled: false` turns everything off silently (e.g. in development).

**Who is using the page.** On login, and whenever the user changes:

```js
Devbench.setUser({ email: currentUser.email, account: currentAccount.id });
Devbench.setUser(null); // on logout
```

`init` returns the sensor (an inert one when disabled), and every method is
safe to call either way; `Devbench.getSensor()` returns it later. Calling
`init` again keeps the first sensor and installs nothing twice;
`Devbench.close()` removes every hook. Every other `init` option is a sensor
option (`slowRequestMs`, `flushIntervalMs`, `autoFlush`, `redaction`, …;
see `SensorOptions` in `src/sensor.ts`).

**AngularJS** needs nothing extra. When `window.angular` exists at `init`, or
exists by `DOMContentLoaded` (so the script tag may come before or after
`angular.js`), Dev Bench hooks `$http` (trace headers, readback, intents) and
`$exceptionHandler` through AngularJS's core module — no `'adt'` dependency to
add. Only if AngularJS loads later than that (lazy-loaded, or a manual
`angular.bootstrap` after `DOMContentLoaded` from a script that arrived
late), call this before bootstrapping:

```js
Devbench.registerAngular(angular);
```

It is idempotent, and apps that already list the `'adt'` module keep working
and still get one interceptor, not two.

**Flushing** is automatic: every 15s when there is something to report,
nothing when there is not, and once more when the page is hidden or closed.

**Size.** 11.5KB gzipped (`dist/devbench.min.js`), against an 18KB budget the
build enforces. No runtime dependencies.

### Migrating from `ADT`

Nothing is required. The bundle still defines `window.ADT` — the very same
object as `Devbench` — so `new ADT.Sensor({ endpoint, tenant, ingestKey })`,
`ADT.installErrorCapture(sensor, window)` and `ADT.registerAngularModule(...)`
keep working, and `dist/adt.iife.js` is still built. To move to the one-line
install, replace that block with the script tag above (a DSN is
`https://<ingestKey>@<host of your endpoint>`); remove the old init code in
the same change, or `init` keeps the first sensor and warns.

## What is captured automatically

**Errors.** Uncaught errors, unhandled rejections, `console.error`,
and every `fetch` and `XMLHttpRequest` that fails: a response of 400 or more,
or no response at all (an abort the app asked for is not a failure). A
success carrying `x-adt-handled` from a server SDK is a silent failure. With
AngularJS, `$http` adds intents, trace headers and write-then-readback, and
one failed request (or one AngularJS exception) is still one count. The sensor
never records its own ingest
or evidence requests.

**What is detected as slow.** Slowness a user feels, as `degradation` problems
that are fingerprinted and counted exactly like errors:

| Type | When | Message (one fingerprint per) | Option, default |
|---|---|---|---|
| `slow_request` | a fetch/XHR takes this long (call to response) | `GET /api/deals/<id> was slow` | `slowRequestMs`, 3000 |
| `slow_page_load` | LCP at or above this, judged once per load | `page /#!/deals/<id> loaded slowly` | `slowPageLoadMs`, 4000 |
| `slow_interaction` | an interaction's input-to-paint time (Event Timing, as INP) | `interaction on /#!/deals/<id> was slow` | `slowInteractionMs`, 500 |

Page and interaction thresholds are web.dev's "poor" bands; `0` disables
each. Messages carry the route template and never a duration, so every
occurrence of the same slow thing is one fingerprint. A request that *fails*
is recorded as that failure only, not also as slow, and a handled-header
silent failure is not also slow: one bad request is one problem. Browsers
without `PerformanceObserver` or the entry types simply report no page or
interaction slowness.

**Why there is no tracing.** Sentry's BrowserTracing sends a transaction with
spans for every page load and navigation, so its cost grows with traffic;
ADT's design is that traffic grows only with distinct problems. The question
tracing is bought to answer — what is still too slow, for whom, how often —
is answered here by slow requests, slow page loads and slow interactions
counted per route, with the affected users on each count and the evidence
bundle (intents, requests) fetched once per new fingerprint. Like Session
Replay (DECISIONS #157), span storage is a Sentry feature deliberately not
matched: no waterfalls, no percentiles, no per-view timings.

**Identity.** `setUser` takes `email` (trimmed, lowercased) and `account` (your
own customer id); each optional, each truncated to 255 characters. It travels
on counts in its own `users` field — never in a fingerprint, a message or
evidence — and emails inside messages are still redacted.

## The two rules this package exists under

**It must never harm the host app.** Every buffer is fixed-size and preallocated,
every entry point is wrapped so an internal failure disables the sensor rather
than throwing into the application, and the bundle budget fails the build rather
than appearing in a report. Currently **11.5KB gzipped against an 18KB budget**.

**It must never leak user data.** Redaction happens here, before anything leaves
the browser, because it cannot be retrofitted. Field values are captured only
when explicitly allow-listed; otherwise they are reduced to a salted,
per-session comparison token — enough to tell whether two values differ, never
enough to learn what either is.

## Fingerprints are a cross-language contract

`src/fingerprint.ts` is a port of Go's `internal/fingerprint`, and
`test/vectors.test.ts` asserts the two agree on `testdata/fingerprint_vectors.json`.

If they diverge, the same problem is counted as two: triage cost doubles and one
issue splits in half with neither half showing true impact. Changing a vector
orphans every fingerprint already stored and requires every implementation to
move together.

## Testing

```sh
npm run check                      # typecheck, tests, size budget
ADT_SOAK_EVENTS=5000000 npm test   # the long soak
npm run build                      # dist/ only (npm test builds it too)
```

No mocks. The transport is tested against a real HTTP server, because it talks
to a route we own. `npm test` builds `dist/` first, so the shipped bundles
themselves are tested (`test/bundle.test.ts`). `npm publish` runs the checks
first (`prepublishOnly`).

### What is *not* covered yet

Stated explicitly rather than left to be discovered:

- **No real-browser soak.** `test/soak.test.ts` drives the sensor under Node and
  asserts our own structures stay bounded — which is where the leak risk lives —
  but it does not measure DOM overhead, long tasks under a real AngularJS digest
  cycle, or heap as a browser actually reports it. A Playwright soak against the
  customer app is needed before shipping to a customer.
- **No source-map resolution.** Minified frames are normalized but not resolved;
  that happens server-side at ingest (M2). If a customer's build emits unusable
  maps, fingerprint stability degrades — risk #10, a ten-minute check worth doing
  early.
- **`init` and `installErrorCapture` are unit-tested against injected
  window-like targets, not a real `window`.** The shipped bundles are
  evaluated in a fresh global (`test/bundle.test.ts`), and the AngularJS hook
  was checked once against real AngularJS 1.8 under jsdom, not in CI. fetch and XHR capture run against a real HTTP server, but the
  XHR is a minimal Node stand-in (`test/helpers/xhr.ts`), not a browser's.
  The browser soak above covers this when it lands.
- **Real-browser LCP and Event Timing are not exercised.** Node has neither
  entry type, so `test/vitals.test.ts` feeds browser-shaped entries through a
  platform stand-in (`test/helpers/perf.ts`); Node's real
  `PerformanceObserver` covers only the unsupported path. Whether a browser
  emits the entries at the moments and values expected (and what a customer's
  real LCP and interaction times are) needs the browser soak.
- **Page-hide flush is tested for its contract** (the keepalive request has
  started before the handler returns), not in a browser that is actually
  unloading.
