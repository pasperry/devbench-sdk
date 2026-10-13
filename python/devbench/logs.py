"""In-process log capture (docs/server-sdk/configuration.md, "In-process log capture
(direct mode)"): recent log lines kept in memory, keyed by the trace that was
current when each was written, so triage can be handed the server lines of
one user action without a sidecar.

One way in: ``LogHandler``, a ``logging.Handler`` on the root logger
(``devbench.capture_logs()``; every integration installs it). It sees exactly
what the app's loggers already let through, so it never turns on a level the
app has off; it writes nothing anywhere, it only remembers.

Never harms the host: a record with no current trace costs one ContextVar
read; the buffer lock is held for a deque append, never across I/O or
redaction; nothing raises out of a logging call. Fork-safe: a child starts
with an empty buffer.
"""

from __future__ import annotations

import collections
import logging
import os
import threading
import time
from typing import Any, Callable, Deque, Dict, List, Optional

from . import context

MAX_LINES = 10_000
MAX_BYTES = 4 * 1024 * 1024
MAX_AGE = 15 * 60
MAX_LINE_BYTES = 4096
# Lines held from one multi-line message (an exception and its traceback):
# the top of it, so one framework traceback cannot push the rest of the
# request out of a 200-line slice.
MAX_MESSAGE_LINES = 50


class _Entry:
    __slots__ = ("key", "at", "text", "size")

    def __init__(self, key: str, at: float, text: str) -> None:
        self.key = key
        self.at = at
        self.text = text
        self.size = len(text.encode("utf-8", "replace"))


class Buffer:
    """Bounded, thread-safe, per-trace store of recent lines. Oldest evicted
    first, by count, bytes and age."""

    def __init__(self, max_lines: int = MAX_LINES, max_bytes: int = MAX_BYTES, max_age: float = MAX_AGE,
                 clock: Callable[[], float] = time.monotonic) -> None:
        self.max_lines = max_lines
        self.max_bytes = max_bytes
        self.max_age = max_age
        self._clock = clock
        self._lock = threading.Lock()
        self._reset()

    def _reset(self) -> None:
        self._pid = os.getpid()
        self._entries: Deque[_Entry] = collections.deque()
        self._by_key: Dict[str, Deque[_Entry]] = {}
        self._bytes = 0

    def push(self, key: str, text: str) -> None:
        """Keeps one line under a trace key."""
        now = self._clock()
        entry = _Entry(key, now, text)
        with self._lock:
            if self._pid != os.getpid():
                self._reset()
            self._entries.append(entry)
            self._by_key.setdefault(key, collections.deque()).append(entry)
            self._bytes += entry.size
            self._evict(now)

    def lookup(self, key: str, limit: int) -> List[str]:
        """The lines held for a key, oldest first, at most ``limit`` (the most
        recent ones when there are more). A copy: the caller redacts it
        outside the lock."""
        with self._lock:
            if self._pid != os.getpid():
                self._reset()
            self._evict(self._clock())
            held = self._by_key.get(key)
            if not held:
                return []
            items = list(held)
        return [e.text for e in items[-limit:]]

    def any(self) -> bool:
        """Whether any line younger than the age bound is held (by this
        process)."""
        with self._lock:
            if self._pid != os.getpid():
                self._reset()
            self._evict(self._clock())
            return bool(self._entries)

    def __len__(self) -> int:
        with self._lock:
            return len(self._entries)

    @property
    def bytes(self) -> int:
        with self._lock:
            return self._bytes

    def clear(self) -> None:
        with self._lock:
            self._reset()

    # Every entry is the oldest of its own key when it is the oldest overall,
    # so removing it from its key's deque is a popleft.
    def _evict(self, now: float) -> None:
        cutoff = now - self.max_age
        entries = self._entries
        while entries and (len(entries) > self.max_lines or self._bytes > self.max_bytes or entries[0].at < cutoff):
            oldest = entries.popleft()
            held = self._by_key.get(oldest.key)
            if held:
                held.popleft()
                if not held:
                    del self._by_key[oldest.key]
            self._bytes -= oldest.size


_buffer = Buffer()
_active = False
_polling_pid: Optional[int] = None
_state_lock = threading.Lock()
_formatter = logging.Formatter()


def buffer() -> Buffer:
    return _buffer


def set_buffer(value: Buffer) -> None:
    """For tests: a buffer with other bounds or a controllable clock."""
    global _buffer
    _buffer = value


def active() -> bool:
    return _active


def activate() -> None:
    global _active, _polling_pid
    _polling_pid = None
    _active = True


def deactivate() -> None:
    global _active, _polling_pid
    _polling_pid = None
    _active = False


def holding() -> bool:
    """Whether this process holds any traced line: the transport polls ingest
    for log requests only then."""
    try:
        return _buffer.any()
    except Exception:
        return False


def lookup(key: str, limit: int) -> List[str]:
    try:
        return _buffer.lookup(key, limit)
    except Exception:
        return []


class LogHandler(logging.Handler):
    """Keeps every record logged under a trace, one entry per line, as
    ``[v1/<session>/<intent>/<hop>] LEVEL message``. Records with no current
    trace are not kept."""

    def __init__(self) -> None:
        super().__init__(logging.NOTSET)

    def handle(self, record: logging.LogRecord) -> bool:
        # No handler lock and no filters: emit touches only the buffer, which
        # has its own lock, and must cost nothing for an untraced record.
        self.emit(record)
        return True

    def emit(self, record: logging.LogRecord) -> None:
        try:
            # Our own warnings are never held: logging one while the
            # transport is being built must not start building it again.
            if not _active or record.name == "devbench":
                return
            trace = context.current_trace()
            if trace is None:
                return
            for line in lines_for(str(trace), record.levelname, _message(record)):
                _buffer.push(trace.key, line)
            _start_polling()
        except Exception:
            # Never handleError(): that prints to the app's stderr.
            pass


def install(logger: Optional[logging.Logger] = None) -> bool:
    """Adds the capturing handler to a logger (default: the root logger).
    Idempotent. Returns True when it is in place."""
    target = logger if logger is not None else logging.getLogger()
    with _state_lock:
        if not any(isinstance(h, LogHandler) for h in target.handlers):
            target.addHandler(LogHandler())
    return True


def lines_for(trace: str, level: Any, text: str) -> List[str]:
    """One held line per line of the message, each at most MAX_LINE_BYTES of
    valid UTF-8. A multi-line message (an exception and its traceback) is
    split so each line reaches triage on its own, blank lines dropped; at most
    MAX_MESSAGE_LINES, the last saying how many were left out."""
    prefix = "[%s] %s " % (trace, level) if level else "[%s] " % trace
    if text.endswith("\n"):
        text = text[:-1]
    if "\n" not in text:
        return [_bound(prefix + text)]
    parts = [p[:-1] if p.endswith("\r") else p for p in text.split("\n")]
    parts = [p for p in parts if p.strip()]
    if not parts:
        return []
    if len(parts) > MAX_MESSAGE_LINES:
        omitted = len(parts) - (MAX_MESSAGE_LINES - 1)
        parts = parts[: MAX_MESSAGE_LINES - 1] + ["... (%d more lines)" % omitted]
    return [_bound(prefix + p) for p in parts]


def _message(record: logging.LogRecord) -> str:
    # The record's own message, as Formatter.format builds it before
    # adding the app's format string: the text, then the traceback.
    text = record.getMessage()
    if record.exc_info:
        exc = _formatter.formatException(record.exc_info)
        text = text + "\n" + exc if text else exc
    elif record.exc_text:
        text = text + "\n" + record.exc_text if text else record.exc_text
    if record.stack_info:
        text = text + "\n" + record.stack_info
    return _valid(text)


def _valid(text: str) -> str:
    from .fingerprint import utf8

    return utf8(text)


def _bound(line: str) -> str:
    data = line.encode("utf-8", "replace")
    if len(data) <= MAX_LINE_BYTES:
        return line
    return data[:MAX_LINE_BYTES].decode("utf-8", "ignore")


def _start_polling() -> None:
    """The first line this process holds starts the transport's flush
    thread, so a process with nothing to report still polls for log requests
    (a forked worker included: the PID is per process)."""
    global _polling_pid
    pid = os.getpid()
    if _polling_pid == pid:
        return
    _polling_pid = pid
    from . import _core

    t = _core.transport()
    start = getattr(t, "start", None)
    if start is not None:
        start()

