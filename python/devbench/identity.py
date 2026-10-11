"""Who was affected (docs/SERVER_SDK_SPEC.md, Capability 5).

Identity travels in its own field and nowhere else: never in a message, a
symbol or a log line, so it can never move a fingerprint.
"""

from __future__ import annotations

from typing import Any, Optional

from . import context
from .fingerprint import utf8

MAX = 255


def normalize(email: Any, account: Any) -> Optional[dict]:
    """``{"email", "account"}`` with empty fields dropped (email trimmed and
    lowercased, each at most 255 characters), or None when neither is
    present."""
    user = {}
    e = _text(email).strip().lower()
    if e:
        user["email"] = e[:MAX]
    a = _text(account).strip()
    if a:
        user["account"] = a[:MAX]
    return user or None


def set_user(email: Any = None, account: Any = None) -> None:
    """Records who the current request (or job) is for. Call once per
    request, e.g. from middleware after authentication; the Dev Bench
    middleware ends it with the request. Each field is optional; calling
    again replaces the whole identity, and calling with neither clears it.
    Never raises."""
    try:
        context.ensure().user = normalize(email, account)
    except Exception:
        pass


def _text(value: Any) -> str:
    # An identity that cannot be read is dropped rather than raised.
    if value is None:
        return ""
    try:
        return utf8(value)
    except Exception:
        return ""
