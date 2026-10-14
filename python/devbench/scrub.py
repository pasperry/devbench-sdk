"""A port of the sidecar's strict egress (internal/scrub and
Sidecar.forEgress): what this SDK does to an exemplar or a log line before it
leaves the process, so leaving out the sidecar does not loosen redaction
(DECISIONS #160). Held to testdata/redaction_vectors.json.

Strict mode is the only mode: template (values the format marks: quoted
strings, numbers, durations), then scrub (values with a shape: emails, cards,
tokens, credentials, IPs), then the prose heuristic (runs of capitalised words
that are not framework vocabulary).

Patterns are Go's, with Go's ``\\b``, ``\\d`` and ``\\s`` (see fingerprint).
"""

from __future__ import annotations

import re
from typing import TYPE_CHECKING, Optional, Tuple

from . import fingerprint as fp_mod
from .fingerprint import GO_NOT_WS as NOT_WS
from .fingerprint import GO_WS as WS
from .fingerprint import go_re, utf8

if TYPE_CHECKING:  # pragma: no cover
    from .egress_policy import EgressPolicy

# The card and phone rules (#186; internal/scrub/numbers.go, the `numbers`
# vectors): a candidate regex plus checks on the whole candidate, run in the
# replacement callback. A rejected candidate is left as written and matching
# carries on after it; it is never retried shorter. No lookaround in the
# candidates (the Go SDK is RE2), so every SDK finds the same ones.
CARD = "<redacted:card>"
PHONE = "<redacted:phone>"
CARD_CANDIDATE = go_re(r"\b(?:\d[ \-]*?){13,19}\b")
# Group 1 is the character before the number (or the start of the text): a
# phone number is not glued to a letter, digit, `_` or `-`, which keeps a
# UUID's tail and `ord_123456789` out. Group 2 is the candidate. Spelled out
# rather than through go_re, whose \d rewrite does not apply inside a class.
PHONE_CANDIDATE = re.compile(r"(^|[^A-Za-z0-9_\-])(\+?[0-9][0-9\t\n\f\r ().\-]{7,}[0-9])")
ISO_DATE_PREFIX = re.compile(r"[0-9]{4}-[0-9]{2}-[0-9]{2}(?:\Z|[^0-9])")
DECIMAL = re.compile(r"[0-9]+\.[0-9]+")
WORD_CHAR = re.compile(r"[A-Za-z0-9_]")
NOT_DIGIT = re.compile(r"[^0-9]")
# E.164 caps a phone number at 15 digits; anything longer is an id.
MIN_PHONE_DIGITS = 7
MAX_PHONE_DIGITS = 15


def _is_timestamp(digits: str) -> bool:
    """A bare Unix timestamp in seconds or milliseconds (2001 to 2033)."""
    return len(digits) in (10, 13) and digits[0] == "1"


def _luhn(digits: str) -> bool:
    if not digits:
        return False
    total = 0
    for i, c in enumerate(reversed(digits)):
        d = ord(c) - 48
        if i % 2 == 1:
            d *= 2
            if d > 9:
                d -= 9
        total += d
    return total % 10 == 0


def _is_card(candidate: str) -> bool:
    """The digits pass the Luhn check, and the candidate is not a bare
    13-digit run starting with 1 (a millisecond timestamp)."""
    digits = NOT_DIGIT.sub("", candidate)
    if digits == candidate and _is_timestamp(digits):
        return False
    return _luhn(digits)


def _is_phone(candidate: str, after: str) -> bool:
    """Judged with the character after it ('' at the end of the text). Not a
    phone when a letter, digit or `_` follows; it starts with an ISO date; it
    is a decimal; it has fewer than 7 or more than 15 digits; or it is a bare
    10- or 13-digit run starting with 1 (a timestamp)."""
    if WORD_CHAR.match(after):
        return False
    if ISO_DATE_PREFIX.match(candidate) or DECIMAL.fullmatch(candidate):
        return False
    digits = NOT_DIGIT.sub("", candidate)
    if not MIN_PHONE_DIGITS <= len(digits) <= MAX_PHONE_DIGITS:
        return False
    return not (digits == candidate and _is_timestamp(digits))


# Ordered most-specific first, and the order is load-bearing: see the
# comments on internal/scrub's rules (a credential containing an email, a
# phone pattern eating an SSN).
RULES = [
    (re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----"), "<redacted:private-key>"),
    (go_re(r"\beyJ[A-Za-z0-9_\-]{4,}\.[A-Za-z0-9_\-]{4,}\.[A-Za-z0-9_\-]{4,}\b"), "<redacted:jwt>"),
    (go_re(r"([a-zA-Z][a-zA-Z0-9+.\-]*://)[^\t\n\f\r :/@]+:[^\t\n\f\r /@]+@"), r"\g<1><redacted:credentials>@"),
    (go_re(r"\b(authorization|proxy-authorization)(" + WS + "*[:=]" + WS + "*)(?:" + NOT_WS + r"+[ \t]+)?" + NOT_WS + "+",
           re.IGNORECASE), r"\g<1>\g<2><redacted:authorization>"),
    (go_re(r"\b(bearer|basic)" + WS + r"+[A-Za-z0-9._\-=/+]{8,}", re.IGNORECASE), r"\g<1> <redacted:token>"),
    (go_re(r"\bAKIA[0-9A-Z]{16}\b"), "<redacted:aws-key>"),
    (go_re(r"\b(password|passwd|pwd|secret|api[_\-]?key|access[_\-]?token|refresh[_\-]?token|"
           r"client[_\-]?secret|private[_\-]?key|session[_\-]?id|csrf[_\-]?token)"
           "(" + WS + "*[:=]" + WS + "*)(\"[^\"]*\"|'[^']*'|" + NOT_WS + "+)", re.IGNORECASE),
     r"\g<1>\g<2><redacted:secret>"),
    # The local part stops at `=` and `:` (#148): without that it starts at
    # the key, and `customer=sam@example.com` or `"email":"sam@..."` loses
    # the key that says which field held the address.
    (go_re(r"\b[^\t\n\f\r <>@=:]+@[^\t\n\f\r <>@]+\.[A-Za-z]{2,}\b"), "<redacted:email>"),
    (go_re(r"\b\d{3}-\d{2}-\d{4}\b"), "<redacted:ssn>"),
    # The card and phone rules (#186) judge each candidate in the callback.
    (CARD_CANDIDATE, lambda m: CARD if _is_card(m.group(0)) else m.group(0)),
    (go_re(r"\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b"), "<redacted:ip>"),
    (PHONE_CANDIDATE,
     lambda m: m.group(1) + PHONE if _is_phone(m.group(2), m.string[m.end():m.end() + 1]) else m.group(0)),
]

# internal/sidecar/trace.go's traceRe.
TRACE = go_re(r"\bv1/([A-Za-z0-9_-]{1,64})/([A-Za-z0-9_-]{1,64})/(\d{1,3})\b")

# What Rails, Go and HTTP say, lowercased (internal/scrub/prose.go). Masking
# any of these would destroy diagnostic content.
VOCABULARY = frozenset("""
ok created accepted content moved permanently found modified bad request
unauthorized payment required forbidden not method allowed acceptable
timeout conflict gone unsupported media type unprocessable entity too many
requests internal server error implemented gateway service unavailable no
see other temporary redirect
started completed processing rendered rendering redirected filter chain
halted parameters views load exists destroy update create transaction
rollback commit cache performed performing enqueued retrying rescued
validation failed invalid email has already been taken
panic goroutine runtime fatal context deadline exceeded canceled cancelled
connection refused such host timeout_ closed reset peer broken pipe
temporarily
warning warn info debug trace exception failure retry skipping unknown
missing expired denied true false nil null none
""".split())

FIELD = re.compile("[^" + fp_mod.GO_SPACE_CLASS + "]+")
TRAILING_PUNCT = re.compile(r"[.,;:!?)\"']+\Z")
CODE_CHARS = re.compile(r"[:#/()_=<>@\[\]{}\"']")
ASCII_DIGIT = re.compile("[0-9]")

# Shape rules (EgressPolicy).
MASKED = "masked"
NONE = "none"
FULL = "full"


def text(line: str) -> str:
    """internal/scrub.Text: every shape rule, in order."""
    out = utf8(line)
    for pattern, replacement in RULES:
        out = pattern.sub(replacement, out)
    return out


def prose(line: str) -> str:
    """internal/scrub.Prose: runs of two or more capitalised words that are
    not framework vocabulary become <redacted:name>."""
    if not line:
        return line
    fields = FIELD.findall(line)
    if len(fields) < 2:
        return line
    out = []
    run: list = []

    def flush() -> None:
        if len(run) >= 2:
            out.append("<redacted:name>")
        elif len(run) == 1:
            out.append(run[0])
        del run[:]

    for field in fields:
        if _proper_noun(field):
            run.append(field)
        else:
            flush()
            out.append(field)
    flush()
    return " ".join(out)


def egress(line: str, policy: "Optional[EgressPolicy]" = None, service: str = "") -> str:
    """The sidecar's forEgress in strict mode, for one line: any trace kept as
    is, the rest templated, scrubbed and prose-masked, then the learned rules
    (an EgressPolicy), exactly as Sidecar.redactBody applies them:

    * shape 'none': the content never leaves: <withheld:shape:<fp12>>
    * shape 'full': lifts only the prose heuristic, never base redaction
    * shape 'masked': prose heuristic (already on in strict mode)
    * field / term: masked afterwards, whatever the shape rule says
    """
    line = utf8(line)
    match = TRACE.search(line)
    body = TRACE.sub("<trace>", line) if match else line
    fp, rule = _shape_rule(policy, service, body)
    redacted = withheld(fp) if rule == NONE else _redact_body(body, rule, policy)
    return "[%s] %s" % (match.group(0), redacted) if match else redacted


def template_text(value: str, policy: "Optional[EgressPolicy]" = None, fp: Optional[str] = None) -> str:
    """A bundle's template text: as the sidecar's answerEvidenceRequests does,
    the shape's own rule (by the signal's fingerprint), strict redaction, then
    a second shape pass. Empty when the shape's content never leaves."""
    rule = policy.rule_for(fp) if policy is not None and fp else None
    if rule == NONE:
        return ""
    return text(_redact_body(utf8(value), rule, policy))


def withheld(fp: Optional[str]) -> str:
    """What stands in for a line whose shape is marked 'none'."""
    return "<withheld:shape:%s>" % (fp or "")[:12]


def _redact_body(body: str, rule: Optional[str], policy: "Optional[EgressPolicy]") -> str:
    redacted = text(fp_mod.template(body))
    if rule != FULL:
        redacted = prose(redacted)
    return policy.mask(redacted) if policy is not None else redacted


def _shape_rule(policy: "Optional[EgressPolicy]", service: str, body: str) -> Tuple[Optional[str], Optional[str]]:
    if policy is None or not policy.has_shapes():
        return None, None
    fp = fp_mod.compute(kind="log_template", source="sidecar", service=service or "", message=body)
    if fp is None:
        return None, None
    return fp, policy.rule_for(fp)


def _proper_noun(token: str) -> bool:
    trimmed = TRAILING_PUNCT.sub("", token)
    if len(trimmed.encode("utf-8", "replace")) < 2:
        return False
    if CODE_CHARS.search(trimmed):
        return False
    if trimmed == trimmed.upper():
        return False
    first = trimmed[0]
    if not ("A" <= first <= "Z"):
        return False
    if ASCII_DIGIT.search(trimmed):
        return False
    return trimmed.lower() not in VOCABULARY
