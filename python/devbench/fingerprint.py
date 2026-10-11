"""A port of internal/fingerprint (Go): the same signal gives the same
fingerprint, byte for byte, as the sidecar, the browser, Ruby and Go compute.

If two implementations disagree, the same problem is counted as two: triage
cost doubles and one issue is split in half. testdata/fingerprint_vectors.json
holds all of them to one answer (tests/test_vectors.py).

Go's regexp and Python's differ in ways that matter here, so the patterns are
spelled out rather than copied:

* Go's ``\\b`` is an ASCII word boundary; Python's (str patterns) treats "é"
  as a word character. ``GO_B`` rebuilds Go's.
* Go's ``\\d`` is ASCII; Python's matches every Unicode digit. Spelled
  ``[0-9]``.
* Go's ``\\s`` is ``[\\t\\n\\f\\r ]``; Python's adds ``\\v`` and Unicode
  spaces. ``GO_WS`` is Go's.
* Go's strings.TrimSpace trims Unicode spaces (unicode.IsSpace), a set that
  differs from str.strip()'s. ``GO_SPACE`` is Go's.
"""

from __future__ import annotations

import hashlib
import re
from typing import Any, Iterable, Mapping

GO_W = "[0-9A-Za-z_]"
GO_B = "(?:(?<=%s)(?!%s)|(?<!%s)(?=%s))" % (GO_W, GO_W, GO_W, GO_W)
GO_WS = r"[\t\n\f\r ]"
GO_NOT_WS = r"[^\t\n\f\r ]"
# unicode.IsSpace, which strings.TrimSpace and strings.Fields use.
GO_SPACE = "\t\n\v\f\r \u0085  " + "".join(chr(c) for c in range(0x2000, 0x200B)) + "    　"
GO_SPACE_CLASS = "\t\n\v\f\r \u0085   -     　"


def go_re(source: str, flags: int = 0) -> "re.Pattern[str]":
    """Compiles a Go pattern: ``\\d`` (outside character classes) as ASCII
    digits and ``\\b`` as Go's ASCII word boundary."""
    return re.compile(source.replace(r"\d", "[0-9]").replace(r"\b", GO_B), flags)


# Ordered as in Go: more specific patterns first, or a UUID becomes a string
# of <num> fragments.
TEMPLATE_RULES = [
    (go_re(r"\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b", re.IGNORECASE), "<uuid>"),
    (go_re(r"\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?"), "<ts>"),
    (go_re(r"\b\d{4}-\d{2}-\d{2}\b"), "<date>"),
    (go_re(r"\b[^\t\n\f\r <>@]+@[^\t\n\f\r <>@]+\.[A-Za-z]{2,}\b"), "<email>"),
    (go_re(r"\b[a-z][a-z0-9+.-]*://" + GO_NOT_WS + "+"), "<url>"),
    (go_re(r"\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b"), "<ip>"),
    (go_re(r"\b(?:0x)?[0-9a-fA-F]{8,}\b"), "<hex>"),
    (re.compile(r'"[^"]*"'), "<str>"),
    (re.compile(r"'[^']*'"), "<str>"),
    (go_re(r"\b\d+(?:\.\d+)?" + GO_WS + r"?(?:ms|s|us|ns|µs)\b"), "<dur>"),
    (go_re(r"\b\d+(?:\.\d+)?\b"), "<num>"),
]

WHITESPACE = re.compile(GO_WS + "+")
MAX_FRAMES = 5
KEEP_SEGMENTS = 3

DEPENDENCY_FRAGMENTS = (
    "/node_modules/", "/vendor/", "/gems/", "/ruby/gems/", "/usr/local/go/src/",
    "/go/pkg/mod/", "/.bundle/", "<anonymous>",
)

ASSET_DIGEST = re.compile(r"(.+)[-.]([A-Za-z0-9_]{8,})((?:\.chunk|\.bundle)?\.(?:js|mjs|cjs|css)(?:\.map)?)\Z")
_SURROGATE = re.compile("[\ud800-\udfff]")
_LOWER_HEX = re.compile(r"[0-9a-f]*\Z")
_DIGIT = re.compile("[0-9]")
_UPPER = re.compile("[A-Z]")
_QUERY = re.compile(r"[?#]")


def utf8(text: Any) -> str:
    """Go treats invalid UTF-8 as U+FFFD. A Python str cannot hold invalid
    UTF-8, but it can hold lone surrogates (surrogateescape'd bytes from a
    path or an environment variable), which cannot be encoded: each becomes
    U+FFFD, as Go would have read the byte."""
    if isinstance(text, bytes):
        return text.decode("utf-8", "replace")
    if not isinstance(text, str):
        text = "" if text is None else str(text)
    if _SURROGATE.search(text):
        return _SURROGATE.sub("�", text)
    return text


def trim(text: str) -> str:
    return text.strip(GO_SPACE)


def compute(kind: str, source: str, service: str = "", type: str = "", message: str = "",
            frames: Iterable[Mapping[str, Any]] | None = None) -> str | None:
    """The fingerprint (64 hex chars) of a signal, or None when the signal has
    no kind/source, or no type, message or frames (Go returns an error)."""
    frames = list(frames or [])
    if not kind or not source:
        return None
    if not type and not message and not frames:
        return None
    norm = normalize(kind, source, service, type, message, frames)
    return hashlib.sha256(canonical(norm).encode("utf-8")).hexdigest()


def normalize(kind: str, source: str, service: str, type: str, message: str,
              frames: Iterable[Mapping[str, Any]] | None) -> dict:
    """The canonical form, field by field, as Go's Normalized."""
    return {
        "kind": utf8(kind),
        "source": utf8(source),
        "service": utf8(service),
        "type": trim(utf8(type)),
        "template": template(message),
        "frames": _normalize_frames(frames or []),
    }


def template(message: Any) -> str:
    """Reduces a message to its shape, replacing values that vary between
    occurrences with holes."""
    out = trim(utf8(message))
    if not out:
        return ""
    for pattern, hole in TEMPLATE_RULES:
        out = pattern.sub(hole, out)
    return trim(WHITESPACE.sub(" ", out))


def normalize_path(path: Any) -> str:
    p = trim(utf8(path))
    if not p:
        return ""
    i = p.find("://")
    if i >= 0:
        rest = p[i + 3:]
        j = rest.find("/")
        p = rest[j:] if j >= 0 else rest
    m = _QUERY.search(p)
    if m:
        p = p[: m.start()]
    p = p.replace("\\", "/")
    segments = p.strip("/").split("/")
    segments = segments[-KEEP_SEGMENTS:]
    segments[-1] = _strip_asset_digest(segments[-1])
    return "/".join(segments)


def canonical(norm: Mapping[str, Any]) -> str:
    return "\x1e".join([
        "v1", norm["kind"], norm["source"], norm["service"], norm["type"], norm["template"],
        "\x1f".join(norm["frames"]),
    ])


def _normalize_frames(frames: Iterable[Mapping[str, Any]]) -> list:
    out = []
    for f in frames:
        function = _field(f, "function")
        file = _field(f, "file")
        if _dependency(file, function):
            continue
        fn = trim(function)
        path = normalize_path(file)
        if not fn and not path:
            continue
        # Deliberately no line number.
        out.append(fn + "@" + path)
        if len(out) == MAX_FRAMES:
            break
    return out


def _field(frame: Any, name: str) -> str:
    if not isinstance(frame, Mapping):
        return ""
    value = frame.get(name)
    return "" if value is None else utf8(value)


def _dependency(file: str, function: str) -> bool:
    hay = file + " " + function
    return any(frag in hay for frag in DEPENDENCY_FRAGMENTS)


def _strip_asset_digest(name: str) -> str:
    m = ASSET_DIGEST.match(name)
    if m is None or not _digest(m.group(2)):
        return name
    return m.group(1) + m.group(3)


def _digest(token: str) -> bool:
    digit = _DIGIT.search(token) is not None
    upper = _UPPER.search(token) is not None
    hex_only = _LOWER_HEX.match(token) is not None
    if hex_only and digit and len(token) >= 8:
        return True
    return len(token) == 8 and digit and upper
