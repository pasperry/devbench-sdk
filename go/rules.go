package devbench

import (
	"regexp"
	"strings"
)

// Learned redaction rules (SERVER_SDK_SPEC "In-process log capture": "applied
// to delivered lines exactly as the sidecar applies them").
//
// Ingest sends the tenant's full current rule set on every flush response to
// a server key (`redaction`). A copy of internal/sidecar/rules.go and of the
// rule handling in Sidecar.forEgress / redactBody, in strict mode — the only
// mode the SDK has. This module cannot import the backend; change nothing
// here without changing the sidecar in lockstep.
//
// The authority boundary is the sidecar's: a rule can stop a shape's content
// ('none'), lift the name heuristic for a shape ('full'), or add masking
// (field and term rules). No rule can lift base redaction — templating and
// the shape scrub always run.

// egressRule is one learned rule as ingest sends it (store.EgressRule). An
// entry with no kind is a shape rule.
type egressRule struct {
	FP     string `json:"fp,omitempty"`
	Egress string `json:"egress,omitempty"`
	Kind   string `json:"kind,omitempty"`
	Target string `json:"target,omitempty"`
}

const (
	ruleField = "field"
	ruleTerm  = "term"

	egressMasked = "masked"
	egressNone   = "none"
	egressFull   = "full"
)

// ruleFieldNameRe mirrors store.NormalizeRule; a field name that fails it is
// ignored, so a malformed rule never becomes an odd pattern in the host.
var ruleFieldNameRe = regexp.MustCompile(`^[a-z0-9_.\-]{1,64}$`)

// ruleMinTermLen: a shorter term would mask fragments of ordinary words.
const ruleMinTermLen = 3

// egressPolicy is one compiled rule set, immutable once built.
type egressPolicy struct {
	shapes map[string]string
	fields *regexp.Regexp // nil when there are no field rules
	terms  []string
}

func compilePolicy(rules []egressRule) *egressPolicy {
	p := &egressPolicy{shapes: make(map[string]string, len(rules))}

	var fields []string
	seenField := map[string]bool{}
	seenTerm := map[string]bool{}

	for _, r := range rules {
		switch r.Kind {
		case "":
			if r.FP != "" {
				p.shapes[r.FP] = r.Egress
			}
		case ruleField:
			name := strings.ToLower(strings.TrimSpace(r.Target))
			if ruleFieldNameRe.MatchString(name) && !seenField[name] {
				seenField[name] = true
				fields = append(fields, regexp.QuoteMeta(name))
			}
		case ruleTerm:
			if len(r.Target) >= ruleMinTermLen && !seenTerm[r.Target] {
				seenTerm[r.Target] = true
				p.terms = append(p.terms, r.Target)
			}
		}
		// Unknown kinds are skipped: a newer backend may send rules this SDK
		// cannot enforce, and the local passes still run.
	}

	if len(fields) > 0 {
		// The field name exactly, optionally quoted or a Ruby symbol, a
		// separator, then one value: a quoted string or a bare token.
		p.fields = regexp.MustCompile(
			`(?i)(^|[^a-z0-9_.\-])(["':]?)(` + strings.Join(fields, "|") + `)` +
				`(["']?\s*(?:=>|=|:)\s*)` +
				`("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|[^\s,;&)}\]]+)`)
	}
	return p
}

// mask applies field and term rules. Additive only.
func (p *egressPolicy) mask(s string) string {
	if p == nil {
		return s
	}
	if p.fields != nil {
		s = p.fields.ReplaceAllString(s, "${1}${2}${3}${4}<redacted:field>")
	}
	for _, term := range p.terms {
		s = strings.ReplaceAll(s, term, "<redacted:term>")
	}
	return s
}

// shapeRule returns the shape rule for a trace-stripped line, and the shape
// fingerprint it was looked up by — the sidecar's shapeFP, the identity the
// backend stores shape rules under.
func (p *egressPolicy) shapeRule(service, body string) (fp, rule string) {
	if p == nil || len(p.shapes) == 0 {
		return "", ""
	}
	fp, _, err := computeFingerprint(fpSignal{
		Kind: "log_template", Source: "sidecar", Service: service, Message: body,
	})
	if err != nil {
		return "", "" // cannot identify the shape; the local passes still run
	}
	return fp, p.shapes[fp]
}

// withheld stands in for a line whose shape is marked 'none' (the sidecar's
// Withheld): the content never leaves, the fact that a line was there does.
func withheld(fp string) string {
	if len(fp) > 12 {
		fp = fp[:12]
	}
	return "<withheld:shape:" + fp + ">"
}

// redactBody is the sidecar's redactBody in strict mode. ok is false when a
// 'none' rule stops this shape.
func (p *egressPolicy) redactBody(body, rule string) (string, bool) {
	if rule == egressNone {
		return "", false
	}
	// Base redaction — the floor no rule can lift.
	redacted := scrubText(templateText(body))
	// The name heuristic — the one pass a rule may move.
	if rule != egressFull {
		redacted = scrubProse(redacted)
	}
	// Field and term rules: additive, after everything else.
	return p.mask(redacted), true
}

// egressLineWith is Sidecar.forEgress for one line in strict mode under the
// learned rules p (nil: none). The trace is lifted out before redaction and
// put back in front.
func egressLineWith(p *egressPolicy, service, line string) string {
	raw := egressTraceRe.FindString(line)
	body := line
	if raw != "" {
		body = egressTraceRe.ReplaceAllString(line, "<trace>")
	}
	fp, rule := p.shapeRule(service, body)
	redacted, ok := p.redactBody(body, rule)
	if !ok {
		redacted = withheld(fp)
	}
	if raw == "" {
		return redacted
	}
	return "[" + raw + "] " + redacted
}
