"""Learned redaction rules, as ingest sends them on a flush response
(``redaction``), compiled and applied exactly as the sidecar does
(internal/sidecar/rules.go and Sidecar.redactBody)::

    {"fp": <shape fp>, "egress": "masked"|"none"|"full"}   shape rule
    {"kind": "field", "target": "license"}                 mask that field's value
    {"kind": "term",  "target": "Whitfield"}               mask that literal

Immutable once built; the transport swaps a whole new one in when a response
carries a rule set, and keeps the current one when a response carries none
(absent is not empty: ingest omits the field when it could not look the
rules up).
"""

from __future__ import annotations

import re
from typing import Any, Dict, List, Optional

from .fingerprint import GO_WS as WS

FIELD = "field"
TERM = "term"

# store.NormalizeRule's field-name shape; anything else is ignored, so a
# malformed rule never becomes an odd pattern here.
FIELD_NAME = re.compile(r"[a-z0-9_.\-]{1,64}\Z")
# A shorter term would mask fragments of ordinary words everywhere.
MIN_TERM_BYTES = 3


class EgressPolicy:
    __slots__ = ("_shapes", "_terms", "_fields")

    def __init__(self, shapes: Dict[str, str], fields: List[str], terms: List[str]) -> None:
        self._shapes = dict(shapes)
        self._terms = tuple(terms)
        self._fields = _field_pattern(fields) if fields else None

    @classmethod
    def compile(cls, rules: Any) -> "EgressPolicy":
        """rules: the parsed JSON array. Entries that are not objects, or not
        understood, are skipped (a newer ingest may send kinds this SDK cannot
        enforce; the local passes still run)."""
        shapes: Dict[str, str] = {}
        fields: List[str] = []
        terms: List[str] = []
        for rule in rules if isinstance(rules, list) else []:
            if not isinstance(rule, dict):
                continue
            kind = rule.get("kind")
            kind = kind if isinstance(kind, str) else ""
            target = rule.get("target")
            target = target if isinstance(target, str) else ""
            if kind == "":
                fp = rule.get("fp")
                if isinstance(fp, str) and fp:
                    egress = rule.get("egress")
                    shapes[fp] = egress if isinstance(egress, str) else ""
            elif kind == FIELD:
                name = target.strip().lower()
                if FIELD_NAME.match(name) and name not in fields:
                    fields.append(name)
            elif kind == TERM:
                if len(target.encode("utf-8", "replace")) >= MIN_TERM_BYTES and target not in terms:
                    terms.append(target)
        return cls(shapes, fields, terms)

    def rule_for(self, fp: Optional[str]) -> Optional[str]:
        """The shape rule for a fingerprint, or None."""
        return self._shapes.get(fp) if fp else None

    def has_shapes(self) -> bool:
        return bool(self._shapes)

    @property
    def terms(self) -> tuple:
        return self._terms

    def mask(self, value: str) -> str:
        """Field and term rules. Additive only: it removes text, never
        restores any."""
        if self._fields is not None:
            value = self._fields.sub(r"\g<1>\g<2>\g<3>\g<4><redacted:field>", value)
        for term in self._terms:
            value = value.replace(term, "<redacted:term>")
        return value


def _field_pattern(fields: List[str]) -> "re.Pattern[str]":
    """The field name, exactly ("license" must not catch "licensed" or
    "driver_license"), optionally quoted or a Ruby symbol, then a separator,
    then one value: a quoted string or a bare token. Go's pattern, with Go's
    \\s and with \\A for Go's unanchored-mode ^."""
    names = "|".join(re.escape(f) for f in fields)
    return re.compile(
        r"(\A|[^a-z0-9_.\-])([\"':]?)(" + names + ")"
        r"([\"']?" + WS + "*(?:=>|=|:)" + WS + "*)"
        r"(\"(?:[^\"\\]|\\.)*\"|'(?:[^'\\]|\\.)*'|[^\t\n\f\r ,;&)}\]]+)",
        re.IGNORECASE,
    )
