# Dev Bench SDK for Go

`github.com/pasperry/devbench-sdk/go` (package `devbench`) — standard library
only. The contract is [docs/SERVER_SDK_SPEC.md](../../../docs/SERVER_SDK_SPEC.md).

## Install

Three steps, no sidecar:

```sh
go get github.com/pasperry/devbench-sdk/go
```

```go
import devbench "github.com/pasperry/devbench-sdk/go"

// 1. Wrap your handler.
http.ListenAndServe(":8080", devbench.Middleware(devbench.Handled(mux)))

// 2. Send server log lines to triage (slog; see "Server logs" below).
slog.SetDefault(slog.New(devbench.NewLogHandler(slog.Default().Handler())))
```

```sh
# 3. Set the DSN Dev Bench gave you for this environment (`adt dsn create`).
DEVBENCH_DSN=https://<public key>:<secret key>@adt-ingest.onrender.com
```

That is all: the first request configures the SDK from the environment.
Panics, `ReportHandled` and `CaptureException` are counted and flushed every
minute; the log lines of a failing request reach triage when it asks for
them. Optionally `defer devbench.Close(context.Background())` in `main` to
send the last minute on graceful shutdown.

The package is at the module root, so the import path is the module path.
Its last element is `go`, not `devbench`; Go accepts that, and the alias
above just makes it explicit for readers and linters.

## Configure

One environment variable:

```sh
DEVBENCH_DSN=https://<public key>:<secret key>@adt-ingest.onrender.com
```

The secret part is this environment's server key: the SDK authenticates
with it, and it is what lets ingest ask this process for log lines. Keep the
whole value server-side (the browser SDK refuses a DSN with a secret in it).
A 0.5-style DSN with a single key, `https://<key>@host`, still works and
sends that key.

Or configure in code — every field is optional and falls back to its
environment variable:

```go
devbench.Init(devbench.Options{
    DSN:     os.Getenv("DEVBENCH_DSN"),
    Service: "billing", // default: DEVBENCH_SERVICE, else the main module's last path element, else "app"
    Release: gitSHA,    // default: DEVBENCH_RELEASE, GIT_SHA, SOURCE_VERSION, RENDER_GIT_COMMIT, the binary's vcs.revision
})
defer devbench.Close(context.Background()) // sends the last window on shutdown (bounded to 2 s)
```

| Variable | |
|---|---|
| `DEVBENCH_DSN` (fallback `ADT_DSN`) | ingest endpoint and key; set → direct mode. Plain `http://` is accepted only for a loopback ingest |
| `DEVBENCH_SERVICE` | service name |
| `DEVBENCH_RELEASE` | deployed build |
| `DEVBENCH_ENABLED=false` | turns everything off |

An invalid DSN logs one line (never either key), turns reporting off, and never
fails the application; `Init` also returns it. Check a DSN without waiting a
minute:

```go
if err := devbench.Test(ctx); err != nil {
    log.Fatal(err) // no DSN, rejected key (401/403), or ingest unreachable
}
```

## net/http

```go
mux := http.NewServeMux()
mux.HandleFunc("GET /deals/{id}", showDeal)

// Trace propagation, panic capture, and the x-adt-handled header.
http.ListenAndServe(":8080", devbench.Middleware(devbench.Handled(mux)))
```

## gin

A separate module, so the core SDK never depends on gin:

```sh
go get github.com/pasperry/devbench-sdk/go/gin
```

```go
import devbenchgin "github.com/pasperry/devbench-sdk/go/gin"

r := gin.New()
r.Use(gin.Logger(), gin.Recovery(), devbenchgin.Middleware()) // after Recovery
```

A panic is reported (with the gin route, e.g. `GET /deals/:id`, as its
symbol on Go 1.23+) and re-panicked, so `gin.Recovery` still answers 500.
Log with `slog.InfoContext(c.Request.Context(), ...)` or
`slog.InfoContext(c, ...)` — both are keyed by the request's trace.

## Use

| What | How | Reported as |
|---|---|---|
| Panics in a handler | automatic in the middleware: reported, then re-panicked with the same value | `exception`, context `request` |
| An error worth reporting | `devbench.CaptureException(ctx, err)` | `exception`, context `explicit` |
| A failure deliberately absorbed | `devbench.ReportHandled(ctx, err, "billing.Invoicer.Persist")` | `handled_failure` (+ the `x-adt-handled` count) |
| Who was affected | `ctx = devbench.WithUser(ctx, devbench.User{Email: u.Email, Account: acct.ID})` | affected users, never in evidence |
| Outbound trace | `&http.Client{Transport: devbench.WrapTransport(nil)}` | `x-adt-trace` header, hop + 1 |
| Server log lines for triage | `slog.New(devbench.NewLogHandler(h))`, log with the request's context | captured per trace; sent redacted when asked (below) |

Set the user once per request, in your auth middleware:

```go
ctx := devbench.WithUser(r.Context(), devbench.User{Email: user.Email, Account: account.ID})
next.ServeHTTP(w, r.WithContext(ctx))   // gin: c.Request = c.Request.WithContext(ctx)
```

```go
if err := deals.Save(ctx, d); err != nil {
    devbench.ReportHandled(ctx, err, "deals.Update") // handled: user sees a fallback
    renderFallback(w)
    return
}
```

## Server logs

When Dev Bench triages a failure the browser saw, it asks for the server
lines logged under that user action's trace. In direct mode the SDK keeps
recent lines in memory for that and needs no sidecar.

```go
slog.SetDefault(slog.New(devbench.NewLogHandler(slog.Default().Handler())))

func charge(w http.ResponseWriter, r *http.Request) {
    slog.InfoContext(r.Context(), "charging card", "deal", id) // captured under the request's trace
}
```

Wrapping `slog.Default().Handler()` is safe: slog's built-in handler writes
through the `log` package, which `slog.SetDefault` points back at slog, so
`NewLogHandler` swaps it for an equivalent (same `LEVEL message k=v`
format after the `log` date prefix, same level, same writer) that does not
loop. Any other handler (`slog.NewJSONHandler(os.Stdout, nil)`, …) is
wrapped as is.

What is captured:

- **slog records logged with the request's context** (`r.Context()`, or
  anything derived from it; under gin also `c`) through `NewLogHandler`,
  in a request whose `x-adt-trace` header `Middleware` accepted. Rendered
  as one line: time, level, message, `key=value` attributes, the trace.
  Records below the wrapped handler's level are not captured.
- **Standard `log` lines that carry a trace id** in their text, with
  `log.SetOutput(devbench.LogWriter(os.Stderr))`. `devbench.Logger(ctx)`
  returns a `*log.Logger` that writes the id for you.

What is not:

- Lines logged without the request's context (`slog.Info(...)`,
  `context.Background()`), and plain `log.Printf` lines without a trace id.
  The `log` package passes no context and Go has no goroutine-local state a
  library could read, so there is no honest way to key them.
- Requests with no trace (no Dev Bench browser SDK upstream, or a malformed
  header): nothing could ever ask for their lines.
- Anything in sidecar mode (no DSN): the sidecar reads your logs instead.
- Output of other loggers (zap, zerolog, logrus) unless they log through
  slog with the context.

Bounds: the most recent 10,000 lines or 4 MiB per process, whichever is
smaller, nothing older than 15 minutes, 4 KiB per line, oldest dropped
first. Capturing never blocks on I/O, never panics into a log call, and
never changes what your handler or writer outputs — your own logs stay
exactly as they were.

Lines leave the process only when Dev Bench asks for a trace: the newest
200 lines for it (≤ 64 KiB), redacted with the same strict rules as
evidence plus any redaction rules Dev Bench has learned for this tenant,
sent to ingest with the secret key from the background goroutine. A
process holding no lines for a trace sends nothing. Dev Bench's request
rides the response to a flush, so while the process holds captured lines
and has nothing to report it sends one empty flush per minute as a poll;
a request is answered within one flush interval. A process holding no
lines sends nothing at all.

## How reports travel

Exactly one transport, decided at start — never both, or every report would
be counted twice:

- **Direct** (a DSN is set; the default install). The SDK does in-process
  what the sidecar does: fingerprints each report with the same algorithm
  (so moving between modes keeps every issue), counts repeats, and flushes
  counts to ingest every 60 s from a background goroutine. Evidence —
  the redacted message and the stack — is uploaded only when Dev Bench asks
  for it, redacted in the SDK with the sidecar's strict rules (values
  templated, emails/cards/tokens/IPs scrubbed, two-word names masked).
  Identity is never part of evidence.
- **Sidecar** (no DSN). Reports go as line-delimited JSON to the local
  sidecar's unix socket (`$ADT_SIDECAR_SOCKET`, default
  `/tmp/adt-sidecar.sock`), which does the rest.

The sidecar is optional: direct mode captures and ships log lines itself
(above). Run one only for logs the SDK cannot see (another process, another
logging library). Learned redaction rules arrive on every flush response
and are applied in the SDK exactly as the sidecar applies them, to log
lines and evidence alike; no rule can lift the strict base redaction.

Never harming the host: callers never wait on I/O (a bounded queue drops and
counts — see `devbench.Stats()`); network calls time out within 5 s on the
background goroutine, are retried once with jitter, then discarded; nothing
panics into the application.

## Migrating from `.../adt/sdk/server/go/adt` (0.4.x)

1. Remove the vendored copy and its `replace`:
   `go mod edit -dropreplace=github.com/pasperry/adt/sdk/server/go -droprequire=github.com/pasperry/adt/sdk/server/go`
2. `go get github.com/pasperry/devbench-sdk/go`
3. Change the import to `devbench "github.com/pasperry/devbench-sdk/go"` and
   `adt.` to `devbench.` (same API: `Middleware`, `Handled`, `ReportHandled`,
   `CaptureException`, `WithUser`, `WrapTransport`, `NewLogHandler`,
   `Close`, …).
4. Set `DEVBENCH_DSN` to switch to direct mode, or leave it unset to keep
   reporting through your sidecar. Fingerprints are the same either way.

## Notes

- **Error type names.** `error` is the first named type in the `Unwrap`
  chain, skipping `*fmt.wrapError`, `*errors.errorString` and the like, so
  `fmt.Errorf("save: %w", pgErr)` groups as `*pgconn.PgError`.
- **Symbol** for a panic is the `ServeMux` route pattern (`GET /deals/{id}`)
  or gin's route; with another router it is empty, never the raw path.
- **Goroutines.** Only the request goroutine is covered: a panic in a
  goroutine the handler starts is beyond any middleware, as with Sentry.
- Go 1.22+. Route symbols need 1.23+.

## Test

```sh
go vet ./... && go test -race ./...           # the SDK
(cd gin && go vet ./... && go test -race ./...) # the gin adapter
```

Tests use real servers (an httptest ingest, a real unix socket under `/tmp`).
Inside the Dev Bench repository the shared vectors in `testdata/` are read;
a vendored copy skips them unless `ADT_REQUIRE_VECTORS=1`.
