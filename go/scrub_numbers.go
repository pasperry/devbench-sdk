package devbench

import (
	"regexp"
	"strings"
)

// The card and phone rules (#186): a copy of the backend's
// internal/scrub/numbers.go, held to the `numbers` section of
// testdata/redaction_vectors.json like the rest of scrub.go.
//
// Each is a candidate regex plus checks on the whole candidate, run where
// the match is replaced. A rejected candidate is left in place and matching
// carries on after it; it is never retried shorter. No lookaround (RE2), so
// every SDK matches the same way.
//
// A 10- or 13-digit run beginning with 1 is read as a Unix timestamp in
// seconds or milliseconds. No 10-digit North American number starts with 1,
// and no card network issues a 13-digit number starting with 1.

const (
	cardPlaceholder  = `<redacted:card>`
	phonePlaceholder = `<redacted:phone>`

	// E.164 caps a phone number at 15 digits; anything longer is an id.
	maxPhoneDigits = 15
	minPhoneDigits = 7
)

var (
	cardCandidate = regexp.MustCompile(`\b(?:\d[ \-]*?){13,19}\b`)

	// Group 1 is the character before the number (or the start of the
	// text); group 2 is the candidate. The leading boundary keeps a UUID's
	// tail (`-446655440000`) and a prefixed id (`ord_123456789`) out.
	phoneCandidate = regexp.MustCompile(`(^|[^A-Za-z0-9_\-])(\+?\d[\d\s().\-]{7,}\d)`)

	isoDatePrefix = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}(?:$|\D)`)
	decimalNumber = regexp.MustCompile(`^\d+\.\d+$`)
)

func redactCards(s string) string {
	return cardCandidate.ReplaceAllStringFunc(s, func(m string) string {
		if isCard(m) {
			return cardPlaceholder
		}
		return m
	})
}

// isCard: the digits pass the Luhn check, and the candidate is not a bare
// millisecond timestamp.
func isCard(candidate string) bool {
	digits := asciiDigitsOf(candidate)
	if digits == candidate && isUnixTimestamp(digits) {
		return false
	}
	return luhnValid(digits)
}

func redactPhones(s string) string {
	matches := phoneCandidate.FindAllStringSubmatchIndex(s, -1)
	if matches == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		start, end := m[4], m[5]
		if !isPhone(s[start:end], s[end:]) {
			continue
		}
		b.WriteString(s[last:start])
		b.WriteString(phonePlaceholder)
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}

// isPhone judges a candidate and the text after it. Not a phone number when
// a letter, digit or `_` follows it; it starts with an ISO date; it is a
// decimal; it has fewer than 7 or more than 15 digits; or it is a bare 10-
// or 13-digit run starting with 1 (a timestamp).
func isPhone(candidate, after string) bool {
	if after != "" && isASCIIWordByte(after[0]) {
		return false
	}
	if isoDatePrefix.MatchString(candidate) || decimalNumber.MatchString(candidate) {
		return false
	}
	digits := asciiDigitsOf(candidate)
	if n := len(digits); n < minPhoneDigits || n > maxPhoneDigits {
		return false
	}
	if digits == candidate && isUnixTimestamp(digits) {
		return false
	}
	return true
}

func isUnixTimestamp(digits string) bool {
	return (len(digits) == 10 || len(digits) == 13) && digits[0] == '1'
}

func luhnValid(digits string) bool {
	if digits == "" {
		return false
	}
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

func asciiDigitsOf(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func isASCIIWordByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
