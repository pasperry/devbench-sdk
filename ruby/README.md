# Dev Bench for Ruby and Rails

Everything Sentry's Rails SDK captures by default, plus who was affected,
trace propagation and the handled-failure header — so an app can turn
Sentry off. Zero runtime dependencies; loads in plain Ruby, hooks itself
into Rails when Rails is there.

## Install (Rails)

```ruby
# Gemfile
gem 'devbench'
```

```sh
# The DSN Dev Bench gave you for this environment
DEVBENCH_DSN=https://<key>@adt-ingest.onrender.com
```

That is the whole install. No middleware line, no initializer, no process
to run next to the app. Check it:

```sh
bin/rails devbench:test
# Dev Bench: sending a test exception to https://adt-ingest.onrender.com (service "acme_shop")
#   HTTP 200: accepted (fingerprint 3f9a2b0c4d5e)
```

It prints the HTTP result, or says plainly what is wrong (`DEVBENCH_DSN is
not set`, `HTTP 401: the key in DEVBENCH_DSN was rejected`, `could not reach
…`) and exits non-zero. Outside Rake: `Devbench.test!`.

## Configuration

All optional except the DSN.

| Variable | Default | |
|---|---|---|
| `DEVBENCH_DSN` | — (falls back to `ADT_DSN`) | `https://<key>@<host>[:port]`. Plain `http://` is accepted only for localhost. |
| `DEVBENCH_SERVICE` | your app's module, underscored (`AcmeShop` → `acme_shop`); `app` outside Rails | Groups this app's issues. |
| `DEVBENCH_RELEASE` | `GIT_SHA`, `SOURCE_VERSION`, `RENDER_GIT_COMMIT`, else empty | Which deploy an occurrence came from. |
| `DEVBENCH_ENABLED` | on | `false` turns everything off: no middleware, no hooks, nothing sent. |

Or from code, e.g. `config/initializers/devbench.rb`:

```ruby
Devbench.configure do |c|
  c.dsn = Rails.application.credentials.devbench_dsn
  c.service = 'billing-api'
end
```

A DSN that does not parse logs one `[devbench]` warning and reports
nothing; it never raises into the app.

## What is captured automatically

| What | How | `context` |
|---|---|---|
| An exception escaping the app | `Devbench::Middleware` (inserted first in the stack by the Railtie) reports it, then re-raises the same object, backtrace intact | `request` |
| An exception Rails rendered as a 500 page itself | `Devbench::Middleware` (`env['action_dispatch.exception']`), or `Rails.error` from Rails 7.1's executor | `request` |
| `Rails.error.report` / `Rails.error.handle` (Rails ≥ 7.0) | the Railtie subscribes; `handled` is passed through | `rails_error` |
| A failed ActiveJob | the Railtie subscribes to `perform.active_job` | `job` |
| A failed Sidekiq job (`Sidekiq::Job` / `Sidekiq::Worker`) | Sidekiq server middleware; reports, then re-raises the same object, so retries are unchanged | `job` |
| A Sidekiq error outside a job (Redis/fetch errors, a death handler that raised) | Sidekiq `error_handlers` | `job` |

Each report carries the class, the message (≤ 2,000 characters), where it
surfaced (`DealsController#update`, `InvoiceJob#perform`), up to 50 stack
frames with paths relative to `Rails.root`, the trace, and the user.

- **Once per exception.** An error that passes several hooks (a job failing
  inside a request reaches three) is reported once.
- **Status codes are not failures.** Not reported, as in Sentry:
  `ActionController::RoutingError`, `ActiveRecord::RecordNotFound`,
  `ActionController::InvalidAuthenticityToken`,
  `ActionController::UnknownFormat`,
  `ActionDispatch::Http::MimeNegotiation::InvalidType`, and subclasses. Add
  your own by name:

  ```ruby
  Devbench.ignore_exceptions << 'Pundit::NotAuthorizedError'
  ```

- **Never in the way.** No hook raises into the app; a failure while
  reporting is dropped, and the application's own exception always comes
  out unchanged.

## What is sent, and when

Like the browser sensor, the gem does not send one request per error:

- Each report is **fingerprinted and counted in the process** (class,
  templated message and in-app frames; the same algorithm as every other
  Dev Bench sensor). Repeats cost a counter increment.
- **Once a minute** a background thread sends the counts — fingerprint,
  how many, first/last seen, and up to 20 affected users each — in one
  small request. Nothing is sent for a quiet minute, and what is left is
  sent when the process exits (waiting at most 2 seconds).
- **Detail only on request.** When Dev Bench sees a fingerprint for the
  first time it asks for evidence, and the gem uploads the stack and one
  example message — **templated and scrubbed first** (emails, numbers,
  quoted values, tokens, card numbers, credentials and two-word names are
  removed). Who was affected never goes into evidence.
- **Never harms the app.** No network I/O on the request thread; 5-second
  timeouts on the background thread, one retry, then the data is dropped;
  memory is bounded (512 distinct problems per minute, the rest only
  counted). Safe under forking servers (Puma, Unicorn, Sidekiq): each
  worker reports on its own.

## Who was affected

Set it once per request, typically in `ApplicationController`:

```ruby
before_action do
  Devbench.set_user(email: current_user&.email, account: current_account&.id)
end
```

- `email` is trimmed and lowercased; `account` is your own tenant/customer
  id, sent as a string. Both optional, each truncated to 255 characters.
- It is sent in its own field with the counts, never inside a message or
  evidence, and triage sees counts of affected users and accounts.
- The middleware clears it when the request ends (Sidekiq: when the job
  ends), so the next request on the same thread never inherits it.

## Reporting what you caught

An exception you rescued but want seen, like Sentry's `capture_exception`:

```ruby
rescue Faraday::Error => e
  Devbench.capture_exception(e, symbol: 'Billing::Sync#run')
  retry_later
end
```

A failure you handled and turned into a normal response — the case this
product exists for:

```ruby
rescue ActiveRecord::RecordInvalid => e
  Devbench.report_handled(e, symbol: 'CustomersController#update', reason: 'validation')
  render json: { ok: true }   # <- the user is about to be told this worked
end
```

`report_handled` also marks the response with `x-adt-handled: <count>`. The
browser already knows whether the user was shown an error; this is the one
fact it cannot know on its own. The header carries a count and nothing else,
and it is added even when reporting is down.

## Sidekiq

Under Rails there is nothing to add: the Railtie installs the hooks at boot
when Sidekiq is loaded. A Sidekiq process **without Rails** installs them
itself, wherever it configures Sidekiq:

```ruby
require 'sidekiq'
require 'devbench'
Devbench::SidekiqHooks.install
```

That adds a server middleware (first in the chain; reports, then re-raises,
so retries and the dead set are unchanged), identity scoped to each job, the
request's trace carried into jobs it enqueues (`"adt_trace"` in the job
payload), and an error handler for failures outside a job. Sidekiq's own
control flow (`Sidekiq::Shutdown`, `JobRetry::Handled`/`Skip`,
`Job::Interrupted`) is never reported. Tested against Sidekiq 7.3; not a
dependency of this gem.

## Without Rails

```ruby
require 'devbench'
use Devbench::Middleware   # Rack
```

Everything else (`set_user`, `capture_exception`, `report_handled`, the
DSN) works the same.

## Trace propagation

The correlation id the browser sent (`x-adt-trace`) is available as
`Devbench::Current.trace` for the duration of the request. Forward it on
outbound calls to other instrumented services:

```ruby
Net::HTTP.post(uri, body, Devbench::HTTP.headers)
```

## Cross-origin

If your JavaScript is served from a different origin than this API, the
browser cannot read `x-adt-handled` unless the server lists it in
`Access-Control-Expose-Headers`. The middleware adds it and appends rather
than overwrites. **A CORS layer that replaces the response header set will
strip it** and detection will silently never fire; check the ordering.

## Optional: the sidecar (server logs)

The Dev Bench sidecar is a separate process that reads your app's log
stream on the host and answers triage's requests for the log lines of one
user action. It is optional; nothing above needs it.

Without a DSN, the gem reports to the sidecar's unix socket instead
(`$ADT_SIDECAR_SOCKET`, default `/tmp/adt-sidecar.sock`), exactly as 0.4
did, and the sidecar counts and uploads on its behalf. **It never uses
both**: with a DSN set, a sidecar on the same host still reads logs, but the
gem does not also write to its socket, so nothing is counted twice.
Fingerprints are identical in both modes, so moving between them keeps
every issue.

## Upgrading from `adt` (0.4)

- Change the Gemfile line to `gem 'devbench'`. `require 'adt'` and every
  `ADT.set_user`, `ADT.report_handled`, `ADT.capture_exception`,
  `ADT::Middleware` and `ADT::SidekiqHooks` keep working unchanged: `ADT`
  is `Devbench`.
- `config.middleware.insert_before 0, ADT::Middleware` can stay or go: the
  Railtie sees it and does not insert a second one.
- Running the sidecar and want to keep it that way? Change nothing else.
  To report directly instead, set `DEVBENCH_DSN` (a key minted with
  `--source server`); the sidecar then only serves logs.
