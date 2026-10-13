"""The correlation id that ties a user action together across every service.

Wire form (docs/server-sdk/trace-propagation.md, Capability 1)::

    x-adt-trace: v1/<session>/<intent>/<hop>

This must agree exactly with the other SDKs and the sidecar
(testdata/trace_vectors.json). A trace the browser writes and Python mangles
is worse than no trace: the request still succeeds, so nothing looks wrong,
and the evidence simply cannot be joined.
"""

from __future__ import annotations

import re
from typing import Any, Optional

from .fingerprint import GO_SPACE

HEADER = "x-adt-trace"
WSGI_HEADER = "HTTP_X_ADT_TRACE"
MAX_HOP = 99
_ID = re.compile(r"[A-Za-z0-9_-]{1,64}\Z")
_HOP = re.compile(r"[0-9]{1,3}\Z")


class Trace:
    __slots__ = ("session", "intent", "hop")

    def __init__(self, session: str, intent: str, hop: int) -> None:
        self.session = session
        self.intent = intent
        self.hop = hop

    @classmethod
    def parse(cls, value: Any) -> "Optional[Trace]":
        """Parses a header value. Returns None rather than raising: Dev Bench
        must never fail a customer's request, and a request carrying a mangled
        correlation id is still a request the user wants served."""
        if isinstance(value, bytes):
            try:
                value = value.decode("ascii")
            except UnicodeDecodeError:
                return None
        if not isinstance(value, str):
            return None
        value = value.strip(GO_SPACE)  # Go: strings.TrimSpace
        if not value.startswith("v1/"):
            return None
        parts = value[3:].split("/")
        if len(parts) != 3:
            return None
        session, intent, hop = parts
        if not _ID.match(session) or not _ID.match(intent) or not _HOP.match(hop):
            return None
        number = int(hop)
        if number > MAX_HOP:
            return None
        return cls(session, intent, number)

    def __str__(self) -> str:
        return "v1/%s/%s/%d" % (self.session, self.intent, self.hop)

    def __repr__(self) -> str:
        return "Trace(%r)" % str(self)

    @property
    def key(self) -> str:
        """Identifies the user action, independent of which service saw it."""
        return "%s/%s" % (self.session, self.intent)

    def next_hop(self) -> "Optional[Trace]":
        """The trace to send to a downstream service, or None at the limit."""
        if self.hop >= MAX_HOP:
            return None
        return Trace(self.session, self.intent, self.hop + 1)

    def __eq__(self, other: object) -> bool:
        return isinstance(other, Trace) and (self.session, self.intent, self.hop) == (other.session, other.intent, other.hop)

    def __hash__(self) -> int:
        return hash((self.session, self.intent, self.hop))
