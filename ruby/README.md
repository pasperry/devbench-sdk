# Dev Bench for Ruby and Rails

Everything Sentry's Rails SDK captures by default, plus who was affected,
the server log lines of a failing request, the browser sensor, trace
propagation and the handled-failure header — so an app can turn Sentry
off. Zero runtime dependencies; loads in plain Ruby, hooks itself
into Rails when Rails is there.

## Install (Rails)

```sh
bundle add devbench
bin/rails generate devbench
```

Then set **one** value in the app's environment — the DSN `adt dsn create`
printed for this environment:

```sh
DEVBENCH_DSN=https://<public>:<secret>@adt-ingest.onrender.com
```

That is the whole install: no browser key, no sidecar, no service names,
no initializer, no middleware line. Check it:

```sh
bin/rails devbench:test
# Dev Bench: sending a test exception to https://adt-ingest.onrender.com (service "acme_shop")
#   HTTP 200: accepted (fingerprint 3f9a2b0c4d5e)
```

It prints the HTTP result, or says plainly what is wrong (`DEVBENCH_DSN is
not set`, `HTTP 401: the key in DEVBENCH_DSN was rejected`, `could not reach
…`) and exits non-zero. Outside Rake: `Devbench.test!`.

What the generator does, and nothing else (run it again and nothing
changes):

- puts `<%= devbench_script_tag %>` in `app/views/layouts/application.html.erb`
  just before `</head>` (`.haml` / `.slim`: `= devbench_script_tag` as the
  last line of `head`). Without that layout it prints what to add where.
- if `config/initializers/content_security_policy.rb` defines a policy,
  adds `https://unpkg.com` to `script_src`, and the ingest host and
  `https://*.storage.supabase.co` (evidence uploads) to `connect_src`.

### The browser tag

`devbench_script_tag` renders the browser sensor, version-pinned to this
gem (upgrading the gem upgrades the sensor), with **only the public part**
of `DEVBENCH_DSN`:

```html
<script src="https://unpkg.com/devbench@0.8.0/dist/devbench.min.js"
        data-dsn="https://<public>@adt-ingest.onrender.com" data-release="<release>" defer></script>
```

The secret never reaches a page. Without a DSN (or with a 0.5 single-key
DSN, or `DEVBENCH_ENABLED=false`) it renders nothing at all, so a
development or test environment without a DSN serves no tag. If the app
uses CSP nonces (`content_security_policy_nonce_generator`), the tag
carries the request's nonce.

**CSP:** if your policy is defined somewhere other than the standard
initializer, add these yourself:

```ruby
policy.script_src  ..., "https://unpkg.com"
policy.connect_src ..., "https://adt-ingest.onrender.com", "https://*.storage.supabase.co"
```

## Configuration

All optional except the DSN.

| Variable | Default | |
|---|---|---|
| `DEVBENCH_DSN` | — (falls back to `ADT_DSN`) | `https://<public>:<secret>@<host>[:port]` from `adt dsn create`. The server authenticates with the secret; the page gets the public part. A 0.5 `https://<key>@<host>` still works (no browser tag, see below). Plain `http://` is accepted only for localhost. |
| `DEVBENCH_SERVICE` | your app's module, underscored (`AcmeShop` → `acme_shop`); `app` outside Rails; **`<that>-sidekiq` in a Sidekiq process** | Groups this app's issues. Set it and it wins everywhere. |
| `DEVBENCH_RELEASE` | the first found of: a `REVISION` file in `Rails.root` (Capistrano writes one), `HEROKU_SLUG_COMMIT`, `KAMAL_VERSION`, `GITHUB_SHA`, `GIT_SHA`, `SOURCE_VERSION`, `RENDER_GIT_COMMIT`; else empty | Which deploy an occurrence came from. `c.release` in code wins over all of these. |
| `DEVBENCH_ENABLED` | on | `false` turns everything off: no middleware, no hooks, no tag, nothing sent. |
| `DEVBENCH_WIDGET_SECRET` | — | The chat widget's secret (`dbws_…`, from the account page). Set, signed-in pages carry the signed identity the widget needs (below). Never reaches the page. |

Or from code, e.g. `config/initializers/devbench.rb`:

```ruby
Devbench.configure do |c|
  c.dsn = Rails.application.credentials.devbench_dsn
  c.service = 'billing-api'
end
```

A DSN that does not parse logs one `[devbench]` warning and reports
nothing; it never raises into the app. Neither key is ever printed.

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

## Failed and slow requests

`Devbench::Middleware` also reports what a request does *without*
raising. Nothing to set up, and no new API:

| Signal | When | Reported as |
|---|---|---|
| **Failed** | the app answered 500–599 and no exception was reported during the request | `http_503 at PUT DealsController#update` |
| **Slow** | the app answered 100–499 after **more than 3 seconds** (3,000 ms exactly is not slow) | `slow_request at GET DealsController#index` |
| **429 spike** | one action answered at least ten 429s in one minute, and at least 10% of its requests | `http_429_spike at GET DealsController#index` |

- **The exception wins.** A 500 page Rails rendered for an exception, or a
  5xx after `Rails.error.report` / `Devbench.capture_exception`, is
  reported once, as the exception. An exception you ignore
  (`Devbench.ignore_exceptions`) does not come back as an `http_500`. A
  slow request is slow whatever else happened in it.
- **`report_handled` changes nothing here:** a 5xx after it is still
  failed, and the `x-adt-handled` header is unchanged.
- **Other 4xx are not reported.** A 404 or 422 is a status code, not a
  problem; only a burst of 429s is.
- The route is Rails' `Controller#action`, never the path (paths carry
  ids). A request no route matched, and every request in a plain Rack
  app, is `*`. The duration runs until the app hands back its response;
  streaming the body after that does not count, and streaming responses
  (`text/event-stream`, a websocket upgrade, a hijacked connection) are
  never slow.
- Failed and slow requests carry the trace and the user; a spike, which
  is many requests, carries neither. Spikes are counted per process
  (each Puma worker on its own) and reported when the minute is over, at
  the next flush, or when the process exits.
- The 3-second threshold is the same in every Dev Bench SDK and is not a
  setting yet.

## What is sent, and when

Like the browser sensor, the gem does not send one request per error:

- Each report is **fingerprinted and counted in the process** (class,
  templated message and in-app frames; the same algorithm as every other
  Dev Bench sensor). Repeats cost a counter increment.
- **Once a minute** a background thread sends the counts — fingerprint,
  how many, first/last seen, and up to 20 affected users each — in one
  small request. Nothing is sent for a quiet minute (except a small poll
  while the process holds log lines, below), and what is left is sent
  when the process exits (waiting at most 2 seconds).
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

## Server log lines

Triage reads the server's log lines for the user action that failed. With
`DEVBENCH_DSN` set, the gem keeps them itself — no sidecar:

- **What:** every line written to `Rails.logger` and `Sidekiq.logger`
  while a request or job carries a trace (`x-adt-trace` from the browser
  sensor; a job inherits the trace of the request that enqueued it — see
  [Background jobs](#background-jobs)).
  Untraced lines are not kept. Rails 7.1+: a capturing logger joins
  `Rails.logger`'s broadcast (it follows the app's level and `silence`,
  and writes nothing); older Rails and `Sidekiq.logger`: a tee after the
  logger's own write. **What your logger writes is unchanged.**
- **Bounded:** per process, the newest 10,000 lines or 4 MiB, nothing older
  than 15 minutes, 4 KiB per line. A logging call is never blocked on I/O
  and never raises because of this.
- **Sent only when asked:** when Dev Bench asks for a trace, the flush
  thread sends that trace's lines from this process — the newest ≤ 200
  lines / 64 KiB — **redacted first** with the same strict rules as
  evidence (quoted values, numbers, emails, cards, tokens, credentials,
  IPs and two-word names removed), plus any redaction rules Dev Bench has
  learned for your app. A process holding lines it was not asked about
  sends nothing but a small poll once a minute while it holds them.
- **Forking servers:** each Puma worker and Sidekiq process keeps and
  answers for its own lines; Dev Bench assembles them.

A Rack app without Rails: `Devbench.capture_logs(logger)`.

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

### Chat widget identity

The chat widget (off by default) is shown only to signed-in users your
server vouches for. With `DEVBENCH_WIDGET_SECRET` set, `devbench_script_tag`
signs the user `set_user` recorded for the request into
`data-widget-identity`, at render time. Nothing else to write. A signed-out
page (no `set_user`) gets no attribute and no widget.

Optionally, say what this user may do in the chat:

```ruby
Devbench.set_user(email: current_user.email, account: current_dealer.id,
                  role: current_user.role,          # your role name; mapped in the widget settings
                  chat: [:report, :howto, :handoff]) # overrides the mapping; [] = may not chat
```

Capabilities: `:report`, `:status`, `:howto`, `:feature_requests`,
`:handoff`. `role` and `chat` go only into the signed token, never with
sensor data. An identity that can't be signed exactly (a field over 255
characters, a control character such as a line break, an unknown
capability) gets no token rather than a truncated one, and one
`[devbench]` warning. Pages carrying a token are
per-user: don't serve them from a shared cache.

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

## Background jobs

A job inherits the trace of the request that enqueued it, so its failure
and its log lines join the user action that caused them:

- **ActiveJob, on any adapter** — Solid Queue (Rails 8's default), GoodJob,
  Sidekiq, Resque, Delayed Job, `:async`, `:inline` and the rest. The trace
  rides in the job's own payload (`"adt_trace"`), so it survives the queue,
  a process restart and `retry_on`. Nothing to add: the Railtie installs it.
  ActiveJob without Rails: `Devbench::ActiveJobHooks.install(ActiveJob::Base)`.
- **Native Sidekiq jobs** (`Sidekiq::Job`), below.

The trace is the request's, unchanged; the job's identity is its own
(`set_user` inside the job; it ends with the job, and a worker thread's
previous job never lends it one). A job enqueued outside a request runs
untraced, as does one enqueued by an earlier version of this gem. On Sidekiq, an
ActiveJob job runs inside the Sidekiq middleware's scope — traced once, not
twice — and `:inline` runs inside the request's.

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
DSN, failed and slow requests) works the same. Plain Rack has no route
pattern, so failed and slow requests are reported under the route `*`.

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

## Optional: the sidecar

The Dev Bench sidecar is a separate process that reads your app's log
stream on the host and answers triage's requests for the log lines of one
user action. It is optional: with a DSN the gem keeps those lines itself
(above). It remains for setups that cannot load the gem.

Without a DSN, the gem reports to the sidecar's unix socket instead
(`$ADT_SIDECAR_SOCKET`, default `/tmp/adt-sidecar.sock`), exactly as 0.4
did, and the sidecar counts and uploads on its behalf. **It never uses
both**: with a DSN set, a sidecar on the same host still reads logs, but the
gem does not also write to its socket, so nothing is counted twice.
Fingerprints are identical in both modes, so moving between them keeps
every issue.

## Upgrading from 0.5

Nothing is required: a 0.5 single-key `DEVBENCH_DSN` keeps reporting
exactly as before. To get server log lines and the browser tag from the
same value, mint a pair and replace the DSN:

```sh
adt dsn create --tenant <tenant> --environment production
# https://<public>:<secret>@adt-ingest.onrender.com
```

then run `bin/rails generate devbench` for the tag and CSP. A page that
loaded the browser sensor with its own DSN can drop that and use the tag.

One default changed: a Sidekiq process without `DEVBENCH_SERVICE` now
reports as `<app>-sidekiq` (it was `<app>`), so job failures are grouped
apart from web failures — and an existing job issue reappears once under
the new name. Set `DEVBENCH_SERVICE` in the Sidekiq process to keep the
old name.

## Upgrading from `adt` (0.4)

- Change the Gemfile line to `gem 'devbench'`. `require 'adt'` and every
  `ADT.set_user`, `ADT.report_handled`, `ADT.capture_exception`,
  `ADT::Middleware` and `ADT::SidekiqHooks` keep working unchanged: `ADT`
  is `Devbench`.
- `config.middleware.insert_before 0, ADT::Middleware` can stay or go: the
  Railtie sees it and does not insert a second one.
- Running the sidecar and want to keep it that way? Change nothing else.
  To report directly instead, set `DEVBENCH_DSN` (from `adt dsn create`);
  see "Upgrading from 0.5".
