# Dev Bench SDK for Go

`github.com/pasperry/devbench-sdk/go` (package `devbench`) — standard library
only. The contract is [docs/SERVER_SDK_SPEC.md](../../../docs/SERVER_SDK_SPEC.md).

## Install

```sh
go get github.com/pasperry/devbench-sdk/go
```

```go
import devbench "github.com/pasperry/devbench-sdk/go"
```

The package is at the module root, so the import path is the module path.
Its last element is `go`, not `devbench`; Go accepts that, and the alias
above just makes it explicit for readers and linters.

## Configure

Set one environment variable, from the key Dev Bench minted for this
environment (`--source server`):

```sh
DEVBENCH_DSN=https://<ingest key>@adt-ingest.onrender.com
```

That is all: the first report configures the SDK from the environment.
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
| `DEVBENCH_DSN` (fallback `ADT_DSN`) | ingest endpoint and key; set → direct mode |
| `DEVBENCH_SERVICE` | service name |
| `DEVBENCH_RELEASE` | deployed build |
| `DEVBENCH_ENABLED=false` | turns everything off |

An invalid DSN logs one line (never the key), turns reporting off, and never
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

## Use

| What | How | Reported as |
|---|---|---|
| Panics in a handler | automatic in the middleware: reported, then re-panicked with the same value | `exception`, context `request` |
| An error worth reporting | `devbench.CaptureException(ctx, err)` | `exception`, context `explicit` |
| A failure deliberately absorbed | `devbench.ReportHandled(ctx, err, "billing.Invoicer.Persist")` | `handled_failure` (+ the `x-adt-handled` count) |
| Who was affected | `ctx = devbench.WithUser(ctx, devbench.User{Email: u.Email, Account: acct.ID})` | affected users, never in evidence |
| Outbound trace | `&http.Client{Transport: devbench.WrapTransport(nil)}` | `x-adt-trace` header, hop + 1 |
| Trace in logs | `slog.New(devbench.NewLogHandler(h))` | `adt_trace` attribute |

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

The sidecar is now an **optional add-on for logs**: run it next to a
direct-mode app and it still indexes and ships log slices, while reports go
direct. Learned per-shape redaction rules are applied only by the sidecar;
direct mode always uses the strict base rules.

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
