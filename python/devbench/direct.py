"""Direct mode (docs/SERVER_SDK_SPEC.md, "Direct mode"): fingerprint and
count in-process, flush counts to ingest once a minute, upload one redacted
evidence bundle per fingerprint only when ingest asks, and answer ingest's
requests for the log lines of a trace (need_logs) from the in-process buffer,
redacted. The same phase-1/phase-2 protocol as the browser sensor and the
Ruby and Go SDKs, and the same signals and bundle the sidecar builds.

Never harms the host:

* The calling thread fingerprints the report (no I/O) and folds it into a
  count under a lock that is never held across I/O. Nothing is queued per
  occurrence: the table of counts *is* the buffer, bounded at
  MAX_FINGERPRINTS per window; further distinct fingerprints are counted in
  ``overflowed``.
* All network I/O is on a background daemon thread (or an explicit flush),
  with timeouts of at most 5 s, one retry with jitter, then discard.
* Nothing raises into the application.
* Fork-safe: gunicorn, uWSGI and Celery's prefork pool fork after import. A
  child notices the PID change on its first report, drops the parent's
  counts (the parent sends those), takes a new sensor id, and starts its own
  thread.
"""

from __future__ import annotations

import json
import os
import random
import re
import secrets
import threading
import time
import urllib.error
import urllib.request
from typing import IO, Any, Dict, List, Optional, Tuple

from . import fingerprint, logs, scrub
from .config import DSN
from .egress_policy import EgressPolicy
from .version import PROTOCOL_VERSION, VERSION

MAX_FINGERPRINTS = 512
MAX_USERS = 20
MAX_DETAILS = 512
MAX_COUNTS_PER_POST = 512
MAX_BODY_BYTES = 192 * 1024
MAX_MESSAGE = 2000
MAX_FRAMES = 50
MAX_IDENTITY = 255
MAX_RESPONSE_BYTES = 64 * 1024
# Log slices: 200 lines and 64 KiB are what the store keeps of a slice;
# sending more would spend the customer's bandwidth on lines truncated on
# arrival. 25 slices is what one delivery may carry.
MAX_SLICE_LINES = 200
MAX_SLICE_BYTES = 64 * 1024
MAX_SLICES_PER_POST = 25
HTTP_TIMEOUT = 5.0
EXIT_TIMEOUT = 2.0


class _Count:
    __slots__ = ("kind", "n", "first", "last", "users", "users_overflow")

    def __init__(self, kind: str, now: int) -> None:
        self.kind = kind
        self.n = 0
        self.first = now
        self.last = now
        self.users: List[dict] = []
        self.users_overflow = 0


class _Detail:
    __slots__ = ("kind", "text", "exemplar", "frames")

    def __init__(self, kind: str, text: str, exemplar: str, frames: list) -> None:
        self.kind = kind
        self.text = text
        self.exemplar = exemplar
        self.frames = frames


class _Signal:
    __slots__ = ("fp", "kind", "text", "user", "frames", "exemplar")

    def __init__(self, fp: str, kind: str, text: str, user: Optional[dict], frames: list, exemplar: str) -> None:
        self.fp = fp
        self.kind = kind
        self.text = text
        self.user = user
        self.frames = frames
        self.exemplar = exemplar


class _Response:
    __slots__ = ("code", "body", "reason")

    def __init__(self, code: int, body: bytes, reason: str) -> None:
        self.code = code
        self.body = body
        self.reason = reason


class DirectTransport:
    def __init__(self, dsn: DSN, service: str, release: str, interval: float = 60.0,
                 warn: Optional[Any] = None) -> None:
        self.dsn = dsn
        self.service = service or ""
        self.release = release or ""
        self.interval = float(interval) if interval and float(interval) > 0 else 60.0
        self._warn = warn or (lambda kind, message: None)
        # Learned redaction rules from the last response that carried a set.
        # Not per process: a forked child keeps them until ingest re-sends.
        self.policy: Optional[EgressPolicy] = None
        self._fork_lock = threading.Lock()
        self._stopped = False
        self._reset_state()

    # ---- The app's side: called on the application's thread.

    def deliver(self, payload: dict) -> None:
        """Takes a report (the dict reporter builds). Never raises, never
        does I/O."""
        try:
            signal = self._signal_for(payload)
            if signal is None:
                return
            self._ensure_running()
            with self._lock:
                self._record(signal)
        except Exception:
            pass

    def flush(self, timeout: float = HTTP_TIMEOUT) -> bool:
        """Sends everything counted so far, now, waiting at most ``timeout``
        seconds. Returns True when nothing is left unsent."""
        try:
            self._check_fork()
            deadline = time.monotonic() + float(timeout)
            if not self._cycle.acquire(timeout=max(0.0, deadline - time.monotonic())):
                return False
            try:
                return self._flush_cycle(deadline)
            finally:
                self._cycle.release()
        except Exception:
            return False

    def stop(self) -> None:
        """A last flush (bounded to EXIT_TIMEOUT), then the thread stops."""
        try:
            self.flush(timeout=EXIT_TIMEOUT)
            self._stopped = True
            with self._wake:
                self._wake.notify_all()
            thread = self._thread
            if thread is not None and thread is not threading.current_thread():
                thread.join(0.5)
        except Exception:
            pass

    def start(self) -> None:
        """Starts the background flush thread if it is not running (in this
        process). Called when this process first holds a log line, so a
        worker that reports nothing still polls for log requests."""
        try:
            self._ensure_running()
        except Exception:
            pass

    def pending(self) -> Dict[str, int]:
        """Counts not yet flushed, by fingerprint. For tests and
        diagnostics."""
        with self._lock:
            return {fp: c.n for fp, c in self._window.items()}

    def self_test(self, out: IO[str]) -> bool:
        """``python -m devbench test``: asks ingest whether this DSN's key is
        valid, and for which tenant and environment. Creates nothing."""
        out.write("Dev Bench: checking the DSN against %s (service %s)\n" % (self.dsn, json.dumps(self.service)))
        response, error = self._attempt("POST", self.dsn.check_url, b"{}", self._flush_headers(), HTTP_TIMEOUT)
        if error is not None or response is None:
            out.write("  could not reach %s: %s: %s\n" % (self.dsn, type(error).__name__, _describe(error)))
            return False
        if not 200 <= response.code < 300:
            out.write("  HTTP %d: %s\n" % (response.code, self._explain(response)))
            return False
        try:
            info = json.loads(response.body.decode("utf-8", "replace") or "{}")
        except ValueError:
            info = {}
        if not isinstance(info, dict):
            info = {}
        out.write("  HTTP %d: accepted - tenant %s, environment %s. Nothing was recorded; real errors will appear as issues.\n"
                  % (response.code, json.dumps(info.get("tenant")), json.dumps(info.get("environment"))))
        return True

    # ---- Signals: exactly what the sidecar builds (internal/sidecar/control.go)

    def _signal_for(self, payload: Any) -> Optional[_Signal]:
        if not isinstance(payload, dict):
            return None
        kind = payload.get("kind")
        if kind == "exception":
            return self._exception_signal(payload)
        if kind == "handled_failure":
            return self._handled_signal(payload)
        return None

    def _exception_signal(self, payload: dict) -> Optional[_Signal]:
        type_ = _text(payload.get("error"))
        message = _text(payload.get("message"))[:MAX_MESSAGE]
        frames = list(payload.get("frames") or [])[:MAX_FRAMES]
        fp = fingerprint.compute("error", "server", self.service, type_, message, frames)
        if fp is None:
            return None
        symbol = _text(payload.get("symbol"))
        text = "%s at %s" % (type_, symbol) if symbol else type_
        return _Signal(fp, "error", text, _user(payload.get("user")), frames, _exemplar(payload))

    def _handled_signal(self, payload: dict) -> Optional[_Signal]:
        # Frames and message are not part of it, so fingerprints never move.
        symbol = _text(payload.get("symbol"))
        fp = fingerprint.compute("handled_failure", "server", self.service, _text(payload.get("error")), symbol)
        if fp is None:
            return None
        frames = list(payload.get("frames") or [])[:MAX_FRAMES]
        return _Signal(fp, "handled_failure", symbol, _user(payload.get("user")), frames, _exemplar(payload))

    # ---- The table (caller holds self._lock)

    def _record(self, signal: _Signal) -> None:
        now = int(time.time())
        entry = self._window.get(signal.fp)
        if entry is None:
            if len(self._window) >= MAX_FINGERPRINTS:
                self._overflowed += 1
                return
            entry = self._window[signal.fp] = _Count(signal.kind, now)
        entry.n += 1
        if now > entry.last:
            entry.last = now
        if signal.user is not None and signal.user not in entry.users:
            if len(entry.users) >= MAX_USERS:
                entry.users_overflow += 1
            else:
                entry.users.append(signal.user)
        self._remember(signal)

    def _remember(self, signal: _Signal) -> None:
        """The first occurrence's detail, kept across windows so evidence can
        be answered after the flush that reported it. Bounded, least recently
        seen first out."""
        detail = self._details.pop(signal.fp, None)
        if detail is None:
            detail = _Detail(signal.kind, signal.text, signal.exemplar, signal.frames)
        elif not detail.frames and signal.frames:
            detail.frames = signal.frames
        self._details[signal.fp] = detail
        while len(self._details) > MAX_DETAILS:
            self._details.pop(next(iter(self._details)))

    # ---- Lifecycle

    def _reset_state(self) -> None:
        self._pid = os.getpid()
        self._lock = threading.Lock()
        self._wake = threading.Condition(self._lock)
        self._cycle = threading.Lock()
        self._window: Dict[str, _Count] = {}
        self._details: Dict[str, _Detail] = {}
        self._overflowed = 0
        self._sensor_id = "py-" + secrets.token_hex(10)
        self._thread: Optional[threading.Thread] = None

    def _check_fork(self) -> None:
        if self._pid == os.getpid():
            return
        with self._fork_lock:
            if self._pid != os.getpid():
                self._reset_state()

    def _ensure_running(self) -> None:
        self._check_fork()
        thread = self._thread
        if self._stopped or (thread is not None and thread.is_alive()):
            return
        with self._fork_lock:
            thread = self._thread
            if self._stopped or (thread is not None and thread.is_alive()):
                return
            thread = threading.Thread(target=self._run, name="devbench-flush", daemon=True)
            self._thread = thread
            thread.start()

    def _run(self) -> None:
        next_at = time.monotonic() + self.interval
        while not self._stopped:
            with self._wake:
                while not self._stopped:
                    remaining = next_at - time.monotonic()
                    if remaining <= 0:
                        break
                    self._wake.wait(remaining)
            if self._stopped:
                break
            next_at = time.monotonic() + self.interval
            try:
                with self._cycle:
                    self._flush_cycle(time.monotonic() + HTTP_TIMEOUT * 4)
            except Exception:
                pass

    # ---- Flushing

    def _flush_cycle(self, deadline: float) -> bool:
        """Phase 1, then whatever the responses asked for: learned rules
        first (a rule arriving with a request for the shape it governs
        applies to that request), then log slices, then evidence."""
        with self._lock:
            counts, overflowed = self._window, self._overflowed
            self._window = {}
            self._overflowed = 0
        # Nothing counted: poll only while this process holds traced lines,
        # since need_logs only rides a flush response. Holding none, send
        # nothing.
        if not counts:
            if not logs.holding():
                return True
            posts = [self._envelope([], 0)]
        else:
            posts = self._bodies(counts, overflowed)

        ok = True
        wanted: List[dict] = []
        log_keys: List[str] = []
        rules: Optional[list] = None
        for body in posts:
            response = self._request("POST", self.dsn.flush_url, body, self._flush_headers(), deadline)
            if response is None:
                ok = False
                continue
            if 200 <= response.code < 300:
                evidence, keys, redaction = self._answers(response)
                wanted.extend(evidence)
                log_keys.extend(keys)
                if redaction is not None:
                    rules = redaction
            else:
                ok = False
                self._warn("http_%d" % response.code, "Dev Bench refused a flush: HTTP %d: %s" % (response.code, self._explain(response)))
        if rules is not None:
            self.policy = EgressPolicy.compile(rules)
        if log_keys:
            self._deliver_logs(list(dict.fromkeys(log_keys)), deadline)
        for ask in wanted:
            self._upload(ask, deadline)
        return ok

    def _bodies(self, counts: Dict[str, _Count], overflowed: int) -> List[bytes]:
        """Counts split so each request stays under MAX_BODY_BYTES (measured,
        not estimated) and MAX_COUNTS_PER_POST. ``overflowed`` rides on the
        first."""
        out: List[bytes] = []
        current: List[dict] = []
        size = 0
        budget = MAX_BODY_BYTES - len(self._envelope([], overflowed))
        for fp, c in counts.items():
            entry: Dict[str, Any] = {"fp": fp, "n": c.n, "first": c.first, "last": c.last, "kind": c.kind}
            if c.users:
                entry["users"] = c.users
            if c.users_overflow > 0:
                entry["users_overflow"] = c.users_overflow
            size_of = len(_json(entry)) + 1
            if current and (len(current) >= MAX_COUNTS_PER_POST or size + size_of > budget):
                out.append(self._envelope(current, 0 if out else overflowed))
                current = []
                size = 0
            current.append(entry)
            size += size_of
        if current:
            out.append(self._envelope(current, 0 if out else overflowed))
        return out

    def _envelope(self, counts: List[dict], overflowed: int) -> bytes:
        body: Dict[str, Any] = {"v": PROTOCOL_VERSION, "tenant": "", "source": "server", "service": self.service,
                                "release": self.release, "sensor_id": self._sensor_id, "counts": counts}
        if overflowed > 0:
            body["overflowed"] = overflowed
        return _json(body)

    def _flush_headers(self) -> Dict[str, str]:
        return {"content-type": "application/json", "x-adt-key": self.dsn.key,
                "user-agent": "devbench-python/%s" % VERSION}

    def _answers(self, response: _Response) -> Tuple[List[dict], List[str], Optional[list]]:
        """What a flush response asked for: evidence, log slices (need_logs
        trace keys) and the learned rule set (a list, or None when the
        response carried none, which leaves the rules in force, unlike an
        empty list, which withdraws them)."""
        body = response.body
        if not body or len(body) > MAX_RESPONSE_BYTES:
            return [], [], None
        try:
            parsed = json.loads(body.decode("utf-8", "replace"))
        except ValueError:
            # Counts were accepted; an unreadable answer only loses the asks.
            return [], [], None
        if not isinstance(parsed, dict):
            return [], [], None
        evidence = parsed.get("need_evidence")
        need_logs = parsed.get("need_logs")
        redaction = parsed.get("redaction")
        return (
            [a for a in evidence if isinstance(a, dict)] if isinstance(evidence, list) else [],
            [k for k in (_log_key(a) for a in need_logs) if k] if isinstance(need_logs, list) else [],
            redaction if isinstance(redaction, list) else None,
        )

    def _deliver_logs(self, keys: List[str], deadline: float) -> None:
        """Phase 3: the lines this process holds for each requested trace,
        redacted with the exemplars' strict egress plus the learned rules,
        POSTed with the secret key. A key with no lines here sends nothing:
        another process may hold them."""
        try:
            policy = self.policy
            slices = []
            for key in keys:
                held = logs.lookup(key, MAX_SLICE_LINES)
                if not held:
                    continue
                slices.append({"trace": key, "lines": _bound_slice([self._egress_line(line, policy) for line in held])})
            for body in _slice_bodies(slices):
                response = self._request("POST", self.dsn.logs_url, body, self._flush_headers(), deadline)
                if response is None or 200 <= response.code < 300:
                    continue
                self._warn("logs_http_%d" % response.code,
                           "Dev Bench refused log lines: HTTP %d: %s" % (response.code, self._explain(response)))
        except Exception:
            pass

    def _egress_line(self, line: str, policy: Optional[EgressPolicy]) -> str:
        """A held line is "[<trace>] <LEVEL> <message>". The bracketed trace
        is our own framing, kept as is; the rest is redacted exactly as the
        sidecar redacts a line's body."""
        own = _OWN_TRACE.match(line)
        if own is None:
            out = scrub.egress(line, policy, self.service)
        else:
            redacted = scrub.egress(line[own.end():], policy, self.service)
            framed = "[%s] " % own.group(1)
            out = redacted if redacted.startswith(framed) else framed + redacted
        data = out.encode("utf-8", "replace")
        if len(data) > logs.MAX_LINE_BYTES:
            out = data[: logs.MAX_LINE_BYTES].decode("utf-8", "ignore")
        return out

    def _upload(self, ask: dict, deadline: float) -> None:
        """Phase 2: the sidecar's bundle shape, PUT to the presigned URL with
        no key (it carries its own authorization)."""
        try:
            fp = ask.get("fp")
            url = ask.get("url")
            if not isinstance(fp, str) or not isinstance(url, str) or not url:
                return
            expires = ask.get("expires")
            if isinstance(expires, (int, float)) and 0 < expires <= time.time():
                return
            with self._lock:
                detail = self._details.get(fp)
            if detail is None:
                return
            policy = self.policy
            bundle: Dict[str, Any] = {
                "v": PROTOCOL_VERSION, "fp": fp, "kind": detail.kind,
                "template": scrub.template_text(detail.text, policy, fp), "service": self.service,
                "exemplars": [scrub.egress(detail.exemplar, policy, self.service)] if detail.exemplar else [],
            }
            if detail.frames:
                bundle["frames"] = detail.frames
            self._request("PUT", url, _json(bundle), {"content-type": "application/json"}, deadline)
        except Exception:
            pass

    # ---- HTTP

    def _request(self, method: str, url: str, body: bytes, headers: Dict[str, str], deadline: float) -> Optional[_Response]:
        """One try, then one retry with jitter on a network error or 5xx,
        inside the deadline. A 4xx is final. Returns the response or None."""
        for attempt in range(2):
            remaining = deadline - time.monotonic()
            if remaining < 0.05:
                return None
            response, error = self._attempt(method, url, body, headers, min(HTTP_TIMEOUT, remaining))
            if error is None and response is not None and response.code < 500:
                return response
            if error is not None:
                self._warn("unreachable", "could not reach Dev Bench at %s (%s); counts for that window were dropped"
                           % (self.dsn, type(error).__name__))
            jitter = 0.1 + random.random() * 0.4
            if attempt == 0 and deadline - time.monotonic() > jitter + 0.05:
                time.sleep(jitter)
        return None

    def _attempt(self, method: str, url: str, body: bytes, headers: Dict[str, str],
                 timeout: float) -> Tuple[Optional[_Response], Optional[BaseException]]:
        request = urllib.request.Request(url, data=body, headers=headers, method=method)
        try:
            with _OPENER.open(request, timeout=timeout) as resp:
                return _Response(resp.status, resp.read(MAX_RESPONSE_BYTES + 1), str(resp.reason or "")), None
        except urllib.error.HTTPError as e:
            try:
                data = e.read(MAX_RESPONSE_BYTES + 1)
            except Exception:
                data = b""
            finally:
                e.close()
            return _Response(e.code, data, str(e.reason or "")), None
        except Exception as e:  # network errors, timeouts, DNS
            return None, e

    def _explain(self, response: _Response) -> str:
        detail = None
        try:
            parsed = json.loads(response.body.decode("utf-8", "replace"))
            detail = parsed.get("error") if isinstance(parsed, dict) else None
        except ValueError:
            pass
        if response.code == 401:
            return "the key in DEVBENCH_DSN was rejected (wrong, revoked, or for another environment)"
        if response.code == 429:
            return "rate limited; counts for that window were dropped"
        if isinstance(detail, str):
            return detail
        return response.reason or "unexpected response"


# ---- Helpers


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    # The key header must never follow a redirect to another host.
    def redirect_request(self, *args: Any, **kwargs: Any) -> None:  # type: ignore[override]
        return None


# The default handlers stay (a customer behind an HTTPS proxy configures it
# in the environment, as for any client); only redirects are refused.
_OPENER = urllib.request.build_opener(_NoRedirect)

_OWN_TRACE = re.compile(r"\[(v1/[A-Za-z0-9_-]{1,64}/[A-Za-z0-9_-]{1,64}/[0-9]{1,3})\] ")


def _json(value: Any) -> bytes:
    return json.dumps(value, separators=(",", ":"), ensure_ascii=False).encode("utf-8", "replace")


def _text(value: Any) -> str:
    return "" if value is None else fingerprint.utf8(value)


def _user(raw: Any) -> Optional[dict]:
    """Identity travels only in the count's users list, never in a
    fingerprint, template or bundle."""
    if not isinstance(raw, dict):
        return None
    email = fingerprint.trim(_text(raw.get("email"))).lower()[:MAX_IDENTITY]
    account = fingerprint.trim(_text(raw.get("account")))[:MAX_IDENTITY]
    out = {}
    if email:
        out["email"] = email
    if account:
        out["account"] = account
    return out or None


def _exemplar(payload: dict) -> str:
    """The sidecar's exemplar line: ``Error: message [context, handled]
    reason=...``. Raw here; templated and scrubbed when it leaves."""
    line = _text(payload.get("error"))
    message = _text(payload.get("message"))[:MAX_MESSAGE]
    if message:
        line += ": " + message.replace("\n", " ")
    ctx = _text(payload.get("context"))
    if ctx:
        line += " [" + ctx
        if payload.get("kind") == "exception" and payload.get("handled"):
            line += ", handled"
        line += "]"
    reason = _text(payload.get("reason"))
    if reason:
        line += " reason=" + reason
    return line


def _log_key(ask: Any) -> Optional[str]:
    key = ask.get("trace") if isinstance(ask, dict) else None
    if isinstance(key, str) and key and len(key.encode("utf-8", "replace")) <= 256:
        return key
    return None


def _bound_slice(lines: List[str]) -> List[str]:
    """The most recent lines that fit the store's per-slice byte bound."""
    total = 0
    kept: List[str] = []
    for line in reversed(lines):
        total += len(line.encode("utf-8", "replace"))
        if total > MAX_SLICE_BYTES:
            break
        kept.append(line)
    kept.reverse()
    return kept


def _slice_bodies(slices: List[dict]) -> List[bytes]:
    """At most MAX_SLICES_PER_POST slices and MAX_BODY_BYTES per request."""
    out: List[bytes] = []
    current: List[dict] = []
    size = 0
    budget = MAX_BODY_BYTES - len(_json({"slices": []}))
    for s in slices:
        size_of = len(_json(s)) + 1
        if current and (len(current) >= MAX_SLICES_PER_POST or size + size_of > budget):
            out.append(_json({"slices": current}))
            current = []
            size = 0
        current.append(s)
        size += size_of
    if current:
        out.append(_json({"slices": current}))
    return out


def _describe(error: Optional[BaseException]) -> str:
    if error is None:
        return "no response"
    reason = getattr(error, "reason", None)
    return str(reason if reason is not None else error)
