# Dev Bench SDKs

Dev Bench finds what is going wrong in your app — exceptions, failed and slow
requests, and the failures that never raise at all: a "success" the server
didn't mean, a save that didn't stick — and turns them into a punch list in
your GitHub, with who was affected.

These are the open-source sensors. Each is one line to install and one
setting (`DEVBENCH_DSN`) to configure.

| | Install | Configure |
|---|---|---|
| **Browser** | `<script src="https://unpkg.com/devbench" data-dsn="https://KEY@ingest…"></script>` or `npm install devbench` | `data-dsn`, or `Devbench.init({ dsn })` |
| **Rails / Ruby** | `gem 'devbench'` | `DEVBENCH_DSN` |
| **Go** | `go get github.com/pasperry/devbench-sdk/go` | `DEVBENCH_DSN`, then one middleware |
| **Python** (Django, FastAPI, Flask, Celery) | `pip install devbench` | `DEVBENCH_DSN` |

Details per SDK: [`browser/`](browser/README.md), [`ruby/`](ruby/README.md),
[`go/`](go/README.md), [`python/`](python/README.md).

## What the sensors send

Counts of distinct problems, not a stream of events or sessions: a failure
that happens ten thousand times is one fingerprint with a count. Detail
(stack, a redacted message) is sent once per new problem, when asked for.
Emails, phone numbers, card numbers, tokens and similar are removed in the
SDK before anything leaves your process; the same redaction test vectors
(`testdata/`) bind every SDK. Who was affected is sent only if your app
calls `setUser`, and never enters fingerprints or messages.

## Never harm the host

Every SDK is fire-and-forget: no call blocks your request path, nothing is
raised into your app, buffers are bounded, and if Dev Bench is unreachable
reports are dropped, not queued.

## Versions

Each SDK has its own version and is released on its own tag:
`browser-vX.Y.Z` (npm `devbench`, `Devbench.VERSION`), `ruby-vX.Y.Z`
(RubyGems `devbench`, `Devbench::VERSION`), `python-vX.Y.Z` (PyPI `devbench`,
`devbench.version.VERSION`) and `go-vX.Y.Z` (`devbench.Version`; the Go
modules themselves are tagged `go/vX.Y.Z` and `go/gin/vX.Y.Z`, which is what
`go get` uses). What the SDKs must agree on is the wire format: each
declares the protocol version it speaks, and Dev Bench accepts every
supported one.

## License

MIT.
