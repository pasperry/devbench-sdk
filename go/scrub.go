package devbench

import (
	"regexp"
	"strings"
)

// Redaction for direct mode: what the sidecar guarantees at egress, done in
// the SDK because in direct mode nothing else stands between the
// application and our storage (DECISIONS #160).
//
// A copy of the backend's internal/scrub (Text, Prose) and of the sidecar's
// strict egress path (Sidecar.forEgress / redactBody in strict mode, with no
// learned rules). Held to testdata/redaction_vectors.json, like the browser
// SDK and the sidecar; change nothing here without changing those in
// lockstep.
//
// What it cannot do, stated plainly: a name interpolated into a sentence by
// hand is caught only by the two-capitalised-words heuristic, and a
// single-word surname is not caught at all. Identity set with WithUser never
// passes through here — it travels in its own field and never enters a
// bundle.

type scrubRule struct {
	re   *regexp.Regexp
	with string
}

// Most specific first; the order is load-bearing (see internal/scrub).
var scrubRules = []scrubRule{
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
		`<redacted:private-key>`},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{4,}\.[A-Za-z0-9_\-]{4,}\.[A-Za-z0-9_\-]{4,}\b`),
		`<redacted:jwt>`},
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^\s:/@]+:[^\s/@]+@`),
		`${1}<redacted:credentials>@`},
	{regexp.MustCompile(`(?i)\b(authorization|proxy-authorization)(\s*[:=]\s*)(?:\S+[ \t]+)?\S+`),
		`${1}${2}<redacted:authorization>`},
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._\-=/+]{8,}`),
		`${1} <redacted:token>`},
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), `<redacted:aws-key>`},
	{regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|api[_\-]?key|access[_\-]?token|refresh[_\-]?token|client[_\-]?secret|private[_\-]?key|session[_\-]?id|csrf[_\-]?token)(\s*[:=]\s*)("[^"]*"|'[^']*'|\S+)`),
		`${1}${2}<redacted:secret>`},
	{regexp.MustCompile(`\b[^\s<>@]+@[^\s<>@]+\.[A-Za-z]{2,}\b`), `<redacted:email>`},
	{regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`), `<redacted:ssn>`},
	{regexp.MustCompile(`\b(?:\d[ \-]*?){13,19}\b`), `<redacted:card>`},
	{regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`), `<redacted:ip>`},
	{regexp.MustCompile(`\+?\d[\d\s().\-]{7,}\d`), `<redacted:phone>`},
}

// scrubText is internal/scrub.Text: everything with a recognisable shape.
func scrubText(s string) string {
	for _, r := range scrubRules {
		s = r.re.ReplaceAllString(s, r.with)
	}
	return s
}

// scrubProse is internal/scrub.Prose: two or more consecutive capitalised
// words that are not framework vocabulary become <redacted:name>.
func scrubProse(s string) string {
	if s == "" {
		return s
	}
	fields := strings.Fields(s)
	if len(fields) < 2 {
		return s
	}

	out := make([]string, 0, len(fields))
	run := make([]string, 0, 4)
	flush := func() {
		switch {
		case len(run) >= 2:
			out = append(out, "<redacted:name>")
		case len(run) == 1:
			out = append(out, run[0])
		}
		run = run[:0]
	}
	for _, field := range fields {
		if suspectProperNoun(field) {
			run = append(run, field)
			continue
		}
		flush()
		out = append(out, field)
	}
	flush()
	return strings.Join(out, " ")
}

func suspectProperNoun(token string) bool {
	trimmed := strings.TrimRight(token, ".,;:!?)\"'")
	if len(trimmed) < 2 {
		return false
	}
	if strings.ContainsAny(trimmed, "::#/()_=<>@[]{}\"'") {
		return false
	}
	if trimmed == strings.ToUpper(trimmed) {
		return false
	}
	first := trimmed[0]
	if first < 'A' || first > 'Z' {
		return false
	}
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] >= '0' && trimmed[i] <= '9' {
			return false
		}
	}
	return !frameworkVocabulary[strings.ToLower(trimmed)]
}

// frameworkVocabulary is internal/scrub's: what Rails, Go and HTTP say.
var frameworkVocabulary = map[string]bool{
	"ok": true, "created": true, "accepted": true, "content": true,
	"moved": true, "permanently": true, "found": true, "modified": true,
	"bad": true, "request": true, "unauthorized": true, "payment": true,
	"required": true, "forbidden": true, "not": true, "method": true,
	"allowed": true, "acceptable": true, "timeout": true, "conflict": true,
	"gone": true, "unsupported": true, "media": true, "type": true,
	"unprocessable": true, "entity": true, "too": true, "many": true,
	"requests": true, "internal": true, "server": true, "error": true,
	"implemented": true, "gateway": true, "service": true, "unavailable": true,
	"no": true, "see": true, "other": true, "temporary": true, "redirect": true,

	"started": true, "completed": true, "processing": true, "rendered": true,
	"rendering": true, "redirected": true, "filter": true, "chain": true,
	"halted": true, "parameters": true, "views": true, "load": true,
	"exists": true, "destroy": true, "update": true, "create": true,
	"transaction": true, "rollback": true, "commit": true, "cache": true,
	"performed": true, "performing": true, "enqueued": true, "retrying": true,
	"rescued": true, "validation": true, "failed": true, "invalid": true,
	"email": true, "has": true, "already": true, "been": true, "taken": true,

	"panic": true, "goroutine": true, "runtime": true, "fatal": true,
	"context": true, "deadline": true, "exceeded": true, "canceled": true,
	"cancelled": true, "connection": true, "refused": true, "such": true,
	"host": true, "timeout_": true, "closed": true, "reset": true, "peer": true,
	"broken": true, "pipe": true, "temporarily": true,

	"warning": true, "warn": true, "info": true, "debug": true, "trace": true,
	"exception": true, "failure": true, "retry": true, "skipping": true,
	"unknown": true, "missing": true, "expired": true, "denied": true,
	"true": true, "false": true, "nil": true, "null": true, "none": true,
}

// egressTraceRe is the sidecar's traceRe: a correlation id in a line is
// kept, not templated, because templating would scatter it into holes and it
// identifies nobody.
var egressTraceRe = regexp.MustCompile(`\bv1/([A-Za-z0-9_-]{1,64})/([A-Za-z0-9_-]{1,64})/(\d{1,3})\b`)

// strictRedact is the sidecar's base redaction in strict mode followed by
// the name heuristic: template, then shapes, then prose.
func strictRedact(body string) string {
	return scrubProse(scrubText(templateText(body)))
}

// egressLine is Sidecar.forEgress for one line in strict mode with no
// learned rules: a trace in the line is lifted out and put back in front.
// With rules, see egressLineWith (rules.go).
func egressLine(line string) string {
	return egressLineWith(nil, "", line)
}

// egressTemplate is how the sidecar prepares a bundle's template text:
// strict redaction, then shapes once more regardless of mode.
func egressTemplate(text string) string {
	return scrubText(strictRedact(text))
}
