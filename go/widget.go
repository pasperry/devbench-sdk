package devbench

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The chat widget's signed identity (docs/server-sdk/chat-widget.md, "Capability 6 —
// Chat widget"). The client's server signs the signed-in user with the
// tenant's widget secret (DEVBENCH_WIDGET_SECRET); the page hands the token to
// the browser SDK, which shows the widget only to users the chat service
// verifies. Every signer must reproduce testdata/widget_identity_vectors.json
// byte for byte: a divergence is a widget that never renders.

// widgetPrefix names the token format. The MAC covers it.
const widgetPrefix = "dbw1"

// widgetCapabilities is the closed list of chat capabilities, in the
// token's canonical (byte) order. A new one is a contract change.
var widgetCapabilities = []string{"feature_requests", "handoff", "howto", "report", "status"}

// ErrWidgetIdentity is returned (wrapped, with the reason) when an identity
// cannot be signed: no secret, neither email nor account, invalid UTF-8 or a
// control character (U+0000–U+001F, U+007F) left in a field after trimming,
// a field over 255 characters, or an unknown chat capability. The
// signer refuses rather than repairs: truncating would sign a different
// person. No token means no widget for that user.
var ErrWidgetIdentity = errors.New("devbench: widget identity cannot be signed")

// WidgetOption adds the optional, signed parts of a widget identity.
type WidgetOption func(*widgetClaims)

type widgetClaims struct {
	role       string
	chatStated bool
	chat       []string
	err        error
}

// WidgetRole signs the client's own role name for this user (e.g. "admin"),
// which the tenant's settings map to chat capabilities. Trimmed of ASCII
// whitespace, case kept. "" states no role.
func WidgetRole(role string) WidgetOption {
	return func(c *widgetClaims) { c.role = role }
}

// WidgetChat signs the user's explicit chat permission: exactly these
// capabilities ("report", "status", "howto", "feature_requests", "handoff"),
// overriding the role's mapping. Duplicates are removed and the order does
// not matter. With no names it is an error, so an empty slice spread into it
// never silently denies; say WidgetNoChat() to deny.
func WidgetChat(capabilities ...string) WidgetOption {
	caps := append([]string(nil), capabilities...)
	return func(c *widgetClaims) {
		if len(caps) == 0 {
			c.err = fmt.Errorf("%w: WidgetChat needs at least one capability (WidgetNoChat denies)", ErrWidgetIdentity)
			return
		}
		c.chatStated, c.chat = true, caps
	}
}

// WidgetNoChat signs that this user may not chat at all.
func WidgetNoChat() WidgetOption {
	return func(c *widgetClaims) { c.chatStated, c.chat = true, nil }
}

// WidgetIdentity returns the signed widget identity for u, issued now:
//
//	token, err := devbench.WidgetIdentity(os.Getenv("DEVBENCH_WIDGET_SECRET"),
//	    devbench.User{Email: u.Email, Account: d.ID},
//	    devbench.WidgetRole(u.Role),            // optional
//	    devbench.WidgetChat("report", "howto")) // optional
//
// Render the token into the page (data-widget-identity on the script tag,
// or Devbench.setWidgetIdentity(token)); a page carrying one is per-user and
// must not be served from a shared cache. A non-nil error (wrapping
// ErrWidgetIdentity) means no token, so no widget for that user; the first
// refusal in the process is also logged, once. Options apply in order; the
// last role or chat option wins.
func WidgetIdentity(secret string, u User, opts ...WidgetOption) (string, error) {
	var c widgetClaims
	for _, opt := range opts {
		if opt != nil {
			opt(&c)
		}
	}
	if c.err != nil {
		warnWidgetRefused(c.err)
		return "", c.err
	}
	token, err := signWidget(secret, u.Email, u.Account, c.role, c.chatStated, c.chat, time.Now())
	if err != nil {
		warnWidgetRefused(err)
		return "", err
	}
	return token, nil
}

// signWidget is the signer the vectors exercise: raw inputs, explicit time.
func signWidget(secret, email, account, role string, chatStated bool, chat []string, issuedAt time.Time) (string, error) {
	if secret == "" {
		return "", fmt.Errorf("%w: no widget secret (DEVBENCH_WIDGET_SECRET)", ErrWidgetIdentity)
	}
	// Validity first: strings.ToLower would quietly turn a bad byte into
	// U+FFFD, signing a repaired (different) email.
	for _, f := range []struct{ name, value string }{{"email", email}, {"account", account}, {"role", role}} {
		if !utf8.ValidString(f.value) {
			return "", fmt.Errorf("%w: %s is not valid UTF-8", ErrWidgetIdentity, f.name)
		}
	}
	email = strings.ToLower(trimASCII(email))
	account = trimASCII(account)
	role = trimASCII(role)
	if email == "" && account == "" {
		return "", fmt.Errorf("%w: neither email nor account", ErrWidgetIdentity)
	}
	for _, f := range []struct{ name, value string }{{"email", email}, {"account", account}, {"role", role}} {
		switch {
		case hasControl(f.value):
			return "", fmt.Errorf("%w: control character in %s", ErrWidgetIdentity, f.name)
		case utf8.RuneCountInString(f.value) > maxUserField:
			return "", fmt.Errorf("%w: %s over %d characters", ErrWidgetIdentity, f.name, maxUserField)
		}
	}
	chatField, err := encodeWidgetChat(chatStated, chat)
	if err != nil {
		return "", err
	}
	ts := issuedAt.Unix()
	if ts < 0 {
		return "", fmt.Errorf("%w: issued before 1970", ErrWidgetIdentity)
	}

	raw := strconv.FormatInt(ts, 10) + "\n" + email + "\n" + account + "\n" + role + "\n" + chatField
	payload := base64.RawURLEncoding.EncodeToString([]byte(raw))
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(widgetPrefix + "." + payload))
	return widgetPrefix + "." + payload + "." + base64.RawURLEncoding.EncodeToString(h.Sum(nil)), nil
}

// encodeWidgetChat is the token's chat field: "" not stated, "none" may not
// chat, else known capabilities, de-duplicated, sorted, joined by ",".
func encodeWidgetChat(stated bool, chat []string) (string, error) {
	if !stated {
		return "", nil
	}
	set := map[string]bool{}
	for _, c := range chat {
		if !isWidgetCapability(c) {
			return "", fmt.Errorf("%w: unknown chat capability %q", ErrWidgetIdentity, c)
		}
		set[c] = true
	}
	if len(set) == 0 {
		return "none", nil
	}
	caps := make([]string, 0, len(set))
	for c := range set {
		caps = append(caps, c)
	}
	sort.Strings(caps)
	return strings.Join(caps, ","), nil
}

func isWidgetCapability(c string) bool {
	for _, known := range widgetCapabilities {
		if c == known {
			return true
		}
	}
	return false
}

// hasControl reports a C0 control character (U+0000–U+001F) or DEL (U+007F).
// Checked after trimming: the trimmed whitespace (TAB, LF, VT, FF, CR) is
// itself C0 and is fine at the ends; anything left inside, or any other
// control at an end (NUL included), is refused. Identities are compared and
// displayed, and invisible bytes could make two of them look alike.
func hasControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if b := s[i]; b < 0x20 || b == 0x7f {
			return true
		}
	}
	return false
}

// trimASCII removes ASCII whitespace only, as the contract names it.
// strings.TrimSpace would also remove Unicode spaces (U+00A0, U+2003), which
// Ruby's strip keeps: the two signers would sign different people.
func trimASCII(s string) string {
	return strings.Trim(s, " \t\n\r\f\v")
}

// widgetWarned: one refusal warning per process (the spec), whatever the
// reason. The message names the reason, never the identity.
var widgetWarned sync.Once

// widgetLogf is where the warning goes; a test seam.
var widgetLogf = log.Printf

func warnWidgetRefused(err error) {
	widgetWarned.Do(func() {
		widgetLogf("%v; no chat widget for that user (further refusals are not logged)", err)
	})
}
