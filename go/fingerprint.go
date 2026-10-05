package devbench

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
)

// In-process fingerprinting for direct mode.
//
// A copy of the backend's internal/fingerprint, which this module cannot
// import: it is published on its own, stdlib-only, and must never pull the
// backend into a customer's build. A copy can drift, and a drifted copy splits
// every issue in two — one fingerprint from the sidecar, another from direct
// mode — so it is held to the same shared vectors
// (testdata/fingerprint_vectors.json) as every other implementation. Change
// nothing here without changing internal/fingerprint in lockstep.

const fpMaxFrames = 5

type fpSignal struct {
	Kind    string
	Source  string
	Service string
	Type    string
	Message string
	Frames  []Frame
}

type fpNormalized struct {
	Kind     string   `json:"kind"`
	Source   string   `json:"source"`
	Service  string   `json:"service"`
	Type     string   `json:"type"`
	Template string   `json:"template"`
	Frames   []string `json:"frames"`
}

var fpDependencyPathFragments = []string{
	"/node_modules/",
	"/vendor/",
	"/gems/",
	"/ruby/gems/",
	"/usr/local/go/src/",
	"/go/pkg/mod/",
	"/.bundle/",
	"<anonymous>",
}

// fpTraits lets a rule be skipped when the line cannot possibly match. An
// optimization only; it must never change output.
type fpTraits struct {
	digit, dash, at, dot, colon, dquote, squote, slashes, hexRun bool
}

func fpScan(s string) fpTraits {
	var t fpTraits
	run := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			t.digit = true
		case c == '-':
			t.dash = true
		case c == '@':
			t.at = true
		case c == '.':
			t.dot = true
		case c == ':':
			t.colon = true
			if i+2 < len(s) && s[i+1] == '/' && s[i+2] == '/' {
				t.slashes = true
			}
		case c == '"':
			t.dquote = true
		case c == '\'':
			t.squote = true
		}
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			run++
			if run >= 8 {
				t.hexRun = true
			}
		} else {
			run = 0
		}
	}
	return t
}

// Ordered: specific before general, or a UUID becomes <num> fragments.
var fpTemplateRules = []struct {
	re    *regexp.Regexp
	with  string
	needs func(fpTraits) bool
}{
	{regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`), "<uuid>",
		func(t fpTraits) bool { return t.dash && t.hexRun }},
	{regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?`), "<ts>",
		func(t fpTraits) bool { return t.digit && t.dash && t.colon }},
	{regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`), "<date>",
		func(t fpTraits) bool { return t.digit && t.dash }},
	{regexp.MustCompile(`\b[^\s<>@]+@[^\s<>@]+\.[A-Za-z]{2,}\b`), "<email>",
		func(t fpTraits) bool { return t.at && t.dot }},
	{regexp.MustCompile(`\b[a-z][a-z0-9+.-]*://\S+`), "<url>",
		func(t fpTraits) bool { return t.slashes }},
	{regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`), "<ip>",
		func(t fpTraits) bool { return t.digit && t.dot }},
	{regexp.MustCompile(`\b(?:0x)?[0-9a-fA-F]{8,}\b`), "<hex>",
		func(t fpTraits) bool { return t.hexRun }},
	{regexp.MustCompile(`"[^"]*"`), "<str>",
		func(t fpTraits) bool { return t.dquote }},
	{regexp.MustCompile(`'[^']*'`), "<str>",
		func(t fpTraits) bool { return t.squote }},
	{regexp.MustCompile(`\b\d+(?:\.\d+)?\s?(?:ms|s|us|ns|µs)\b`), "<dur>",
		func(t fpTraits) bool { return t.digit }},
	{regexp.MustCompile(`\b\d+(?:\.\d+)?\b`), "<num>",
		func(t fpTraits) bool { return t.digit }},
}

var fpWhitespace = regexp.MustCompile(`\s+`)

// computeFingerprint is internal/fingerprint.Compute.
func computeFingerprint(sig fpSignal) (string, fpNormalized, error) {
	if sig.Kind == "" {
		return "", fpNormalized{}, errors.New("fingerprint: kind is required")
	}
	if sig.Source == "" {
		return "", fpNormalized{}, errors.New("fingerprint: source is required")
	}
	if sig.Type == "" && sig.Message == "" && len(sig.Frames) == 0 {
		return "", fpNormalized{}, errors.New("fingerprint: signal has no type, message, or frames")
	}

	norm := fpNormalized{
		Kind:     sig.Kind,
		Source:   sig.Source,
		Service:  sig.Service,
		Type:     strings.TrimSpace(sig.Type),
		Template: templateText(sig.Message),
		Frames:   fpNormalizeFrames(sig.Frames),
	}
	sum := sha256.Sum256([]byte(fpCanonical(norm)))
	return hex.EncodeToString(sum[:]), norm, nil
}

// templateText is internal/fingerprint.Template: values that vary between
// occurrences become holes.
func templateText(msg string) string {
	out := strings.TrimSpace(msg)
	if out == "" {
		return ""
	}
	// Re-scanned after each substitution, exactly as the original does.
	for _, rule := range fpTemplateRules {
		if !rule.needs(fpScan(out)) {
			continue
		}
		out = rule.re.ReplaceAllString(out, rule.with)
	}
	out = fpWhitespace.ReplaceAllString(out, " ")
	return strings.TrimSpace(out)
}

func fpNormalizeFrames(frames []Frame) []string {
	out := make([]string, 0, fpMaxFrames)
	for _, f := range frames {
		if fpIsDependencyFrame(f) {
			continue
		}
		fn := strings.TrimSpace(f.Function)
		file := fpNormalizePath(f.File)
		if fn == "" && file == "" {
			continue
		}
		out = append(out, fn+"@"+file)
		if len(out) == fpMaxFrames {
			break
		}
	}
	return out
}

func fpIsDependencyFrame(f Frame) bool {
	hay := f.File + " " + f.Function
	for _, frag := range fpDependencyPathFragments {
		if strings.Contains(hay, frag) {
			return true
		}
	}
	return false
}

func fpNormalizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if i := strings.Index(p, "://"); i >= 0 {
		rest := p[i+3:]
		if j := strings.Index(rest, "/"); j >= 0 {
			p = rest[j:]
		} else {
			p = rest
		}
	}
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	p = strings.ReplaceAll(p, "\\", "/")

	segments := strings.Split(strings.Trim(p, "/"), "/")
	const keep = 3
	if len(segments) > keep {
		segments = segments[len(segments)-keep:]
	}
	segments[len(segments)-1] = fpStripAssetDigest(segments[len(segments)-1])
	return strings.Join(segments, "/")
}

var fpAssetDigest = regexp.MustCompile(`^(.+)[-.]([A-Za-z0-9_]{8,})((?:\.chunk|\.bundle)?\.(?:js|mjs|cjs|css)(?:\.map)?)$`)

func fpStripAssetDigest(name string) string {
	m := fpAssetDigest.FindStringSubmatch(name)
	if m == nil || !fpLooksLikeDigest(m[2]) {
		return name
	}
	return m[1] + m[3]
}

func fpLooksLikeDigest(tok string) bool {
	hasDigit, hasUpper, hexOnly := false, false, true
	for _, r := range tok {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'Z':
			hasUpper = true
			hexOnly = false
		default:
			hexOnly = false
		}
	}
	if hexOnly && hasDigit && len(tok) >= 8 {
		return true
	}
	return len(tok) == 8 && hasDigit && hasUpper
}

func fpCanonical(n fpNormalized) string {
	var b strings.Builder
	b.WriteString("v1\x1e")
	b.WriteString(n.Kind)
	b.WriteString("\x1e")
	b.WriteString(n.Source)
	b.WriteString("\x1e")
	b.WriteString(n.Service)
	b.WriteString("\x1e")
	b.WriteString(n.Type)
	b.WriteString("\x1e")
	b.WriteString(n.Template)
	b.WriteString("\x1e")
	b.WriteString(strings.Join(n.Frames, "\x1f"))
	return b.String()
}
