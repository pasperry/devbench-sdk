# Dev Bench for Python

Errors, who they hit, and the server log lines of the request that failed,
from Django, FastAPI, Flask and Celery to Dev Bench. Zero runtime
dependencies (the standard library only); Python 3.9+.

## Install

```sh
pip install devbench
```

Set **one** value in the app's environment, the DSN `adt dsn create`
printed for this environment:

```sh
DEVBENCH_DSN=https://<public>:<secret>@adt-ingest.onrender.com
```

Then add one line where your framework wants it.

### Django

In `settings.py`:

```python
INSTALLED_APPS = [
    "devbench.django",
    # ...
]
```

That is the whole install: at startup it puts its middleware first in
`MIDDLEWARE` (not again if you list `devbench.django.middleware.DevbenchMiddleware`
yourself), reports what Django answers with a 500, keeps log lines, and,
when Celery is loaded, installs the Celery hooks. WSGI and ASGI. Django 4.2+.

### FastAPI (and Starlette)

```python
from fastapi import FastAPI
from devbench.fastapi import DevbenchMiddleware

app = FastAPI()
app.add_middleware(DevbenchMiddleware)
```

Async and sync endpoints alike. Any other ASGI app: `devbench.asgi.DevbenchMiddleware`.

### Flask

```python
from flask import Flask
from devbench.flask import Devbench

app = Flask(__name__)
Devbench(app)          # or: Devbench().init_app(app)
```

Any other WSGI app: `application = devbench.wsgi.DevbenchMiddleware(application)`.

### Celery

Under Django there is nothing to add. Otherwise, where your Celery app is
created:

```python
import devbench.celery
devbench.celery.install()
```

### Check it

```sh
python -m devbench test             # Django: python manage.py devbench_test
# Dev Bench: checking the DSN against https://adt-ingest.onrender.com (service "acme_shop")
#   HTTP 200: accepted - tenant "acme", environment "production". Nothing was recorded; real errors will appear as issues.
```

It prints the HTTP result, or says plainly what is wrong (`DEVBENCH_DSN is
not set`, `HTTP 401: the key in DEVBENCH_DSN was rejected`, `could not reach
…`) and exits non-zero. It records nothing.

## Configuration

All optional except the DSN.

| Variable | Default | |
|---|---|---|
| `DEVBENCH_DSN` | — (falls back to `ADT_DSN`) | `https://<public>:<secret>@<host>[:port]` from `adt dsn create`. The server authenticates with the secret. Plain `http://` is accepted only for localhost. Unset: nothing is sent (there is no sidecar mode). |
| `DEVBENCH_SERVICE` | Django: the project (`acme_shop` from `acme_shop.settings`); Flask: the app's import name; else `app`; **`<that>-celery` in a Celery worker** | Groups this app's issues. Set it and it wins everywhere. |
| `DEVBENCH_RELEASE` | the first found of: a `REVISION` file in the app root (Django's `BASE_DIR`, else the working directory), `HEROKU_SLUG_COMMIT`, `KAMAL_VERSION`, `GITHUB_SHA`, `GIT_SHA`, `SOURCE_VERSION`, `RENDER_GIT_COMMIT`; else empty | Which deploy an occurrence came from. |
| `DEVBENCH_ENABLED` | on | `false` turns everything off: no hooks, no headers, nothing sent. |

Or from code, before the first report:

```python
import devbench
devbench.configure(dsn=secrets["devbench_dsn"], service="billing-api")
```

A DSN that does not parse logs one `[devbench]` warning (on the `devbench`
logger) and reports nothing; it never raises into the app. Neither key is
ever printed.

## What is captured automatically

| What | How | `context` |
|---|---|---|
| A Django view or middleware error answered with a 500 | `got_request_exception` | `request` |
| A Flask view error answered with a 500 | `got_request_exception` | `request` |
| An exception escaping a FastAPI/Starlette app (still answered 500 by Starlette, and your exception handlers still run) | the ASGI middleware reports, then re-raises the same object | `request` |
| A failed Celery task (the last attempt; `self.retry()` is not a failure) | `task_failure` | `job` |

Each report carries the class (`module.QualName`; builtins plainly, e.g.
`KeyError`), the message (≤ 2,000 characters), where it surfaced (the view,
endpoint or task: `shop.views.checkout`, `acme.charge`), up to 50 stack
frames (innermost first, paths relative to the app root), the trace, and
the user.

- **Once per exception.** An error that passes several hooks is reported
  once.
- **Status codes are not failures.** Django's `Http404`, `PermissionDenied`,
  `BadRequest`, `SuspiciousOperation`; werkzeug and Starlette
  `HTTPException`s; Celery's `Retry`, `Ignore`, `Reject`; and anything that
  is not an `Exception` (`SystemExit`, `KeyboardInterrupt`,
  `asyncio.CancelledError`) are never reported. Add your own by name:

  ```python
  devbench.ignore_exceptions().append("myapp.errors.NotAuthorized")
  ```

- **Never in the way.** No hook raises into the app; a failure while
  reporting is dropped, and the application's own exception always comes
  out unchanged.

### Failed, slow and rate-limited requests

The same middleware also watches what each request does *without* raising
(no code needed; Django, Flask, FastAPI/Starlette and bare WSGI/ASGI):

| Signal | When | Reported as |
|---|---|---|
| Failed | the app answered 500–599 and nothing was reported as an exception during the request | kind `error`, type `http_<status>` |
| Slow | the app took **more than 3,000 ms** and answered 100–499 | kind `degradation`, type `slow_request` |
| 429 spike | in one unix minute, a route answered at least ten 429s and at least 10% of its requests were 429s | kind `error`, type `http_429_spike` |

- **Exceptions win.** An unhandled error, or a `capture_exception` during
  the request, is the report on its own: its 500 is not also failed. A
  `report_handled` changes nothing: a 5xx after it is still failed, and the
  `x-adt-handled` header is unchanged. Each status is its own problem
  (`http_500`, `http_503`). Raising an `HTTPException` with a 5xx status is
  a response, so it is failed.
- **Slow** is measured on a monotonic clock from the middleware's entry
  until the app hands back its response (the WSGI callable returns, the
  ASGI app's coroutine completes, Django's `get_response` returns).
  Exactly 3,000 ms is not slow. Event streams (`text/event-stream`) are
  never slow; a streaming 5xx is still failed. The threshold is a
  constant, not a setting, for now.
- **429 spikes** are counted per process and reported once per route per
  minute, when the minute closes (the next request in a later minute, the
  next flush, or process exit), with no trace or user.
- Other 4xx are not reported. Each signal is identified by the method and
  the route **pattern**, never the raw path: Django's
  `resolver_match.route` (`orders/<int:id>`), Flask's URL rule
  (`/orders/<int:id>`), the FastAPI/Starlette route path
  (`/orders/{id}`), or `*` when no route matched (and for a bare WSGI/ASGI
  app). A failed or slow request carries its trace and user.

## What is sent, and when

- Each report is **fingerprinted and counted in the process** (class,
  templated message and in-app frames; the same algorithm as every other
  Dev Bench SDK). Repeats cost a counter increment.
- **Once a minute** a background thread sends the counts (fingerprint, how
  many, first/last seen, up to 20 affected users each) in one small request.
  Nothing is sent for a quiet minute (except a small poll while the process
  holds log lines, below), and what is left is sent when the process exits
  (waiting at most 2 seconds; a Celery prefork child flushes at shutdown).
- **Detail only on request.** When Dev Bench sees a fingerprint for the
  first time it asks for evidence, and the SDK uploads the stack and one
  example message, **templated and scrubbed first** (emails, numbers, quoted
  values, tokens, card numbers, credentials and two-word names are removed).
  Who was affected never goes into evidence.
- **Never harms the app.** No network I/O on a request thread; 5-second
  timeouts on the background thread, one retry, then the data is dropped;
  memory is bounded (512 distinct problems per minute, the rest only
  counted). Safe under forking servers (gunicorn, uWSGI, Celery prefork):
  each process reports on its own.

## Server log lines

Triage reads the server's log lines for the user action that failed. The
SDK keeps them itself, no sidecar:

- **What:** every record that reaches the root logger's handlers while a
  request or task carries a trace (`x-adt-trace` from the browser sensor; a
  Celery task inherits the trace of the request that enqueued it). It sees
  exactly what your logging configuration already lets through: it never
  turns on a level you have off, writes nothing anywhere, and leaves your
  handlers unchanged. A logger with `propagate = False` is not seen. An
  exception's traceback is held one line each. Untraced records are not
  kept.
- **Bounded:** per process, the newest 10,000 lines or 4 MiB, nothing older
  than 15 minutes, 4 KiB per line. A logging call never waits on I/O and
  never raises because of this.
- **Sent only when asked:** when Dev Bench asks for a trace, the flush
  thread sends that trace's lines from this process (the newest ≤ 200 lines
  / 64 KiB), **redacted first** with the same strict rules as evidence, plus
  any redaction rules Dev Bench has learned for your app.
- **Several processes:** each gunicorn worker and Celery process keeps and
  answers for its own lines (a task's lines are in the worker, under the
  trace of the request that enqueued it); Dev Bench assembles them.

A plain WSGI/ASGI app without one of the integrations: `devbench.capture_logs()`.

## Who was affected

Set it once per request, after your authentication has run (a middleware
after Django's `AuthenticationMiddleware`, a FastAPI dependency, a Flask
`before_request`):

```python
devbench.set_user(email=request.user.email, account=request.user.account_id)
```

- `email` is trimmed and lowercased; `account` is your own tenant/customer
  id, sent as a string. Both optional, each truncated to 255 characters.
- It is sent in its own field with the counts, never inside a message or
  evidence.
- It ends with the request (a Celery task: with the task), so the next
  request on the same thread never inherits it.

## Reporting what you caught

An exception you caught but want seen, like Sentry's `capture_exception`:

```python
except requests.RequestException as e:
    devbench.capture_exception(e, symbol="billing.sync")
    retry_later()
```

A failure you handled and turned into a normal response, the case this
product exists for:

```python
except IntegrityError as e:
    devbench.report_handled(e, symbol="customers.views.update", reason="uniqueness")
    return JsonResponse({"ok": True})   # <- the user is about to be told this worked
```

`report_handled` also marks the response with `x-adt-handled: <count>` (and
lists it in `Access-Control-Expose-Headers`, appending to what is there).
The browser already knows whether the user was shown an error; this is the
one fact it cannot know on its own. The header carries a count and nothing
else, and it is added even when reporting is down.

## Trace propagation

The correlation id the browser sent (`x-adt-trace`) is `devbench.current_trace()`
for the duration of the request. Forward it on outbound calls to other
instrumented services:

```python
requests.post(url, json=body, headers=devbench.trace_headers())
```

## Not in this release

- No sidecar transport: without a DSN nothing is sent.
- `devbench.track` (custom events) and the chat widget's signed identity
  are in the Ruby and Go SDKs; they come to Python later.
