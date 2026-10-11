package devbench

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// testdata/widget_identity_vectors.json (Capability 6): this signer must
// produce every `sign` token byte for byte and refuse every `refuse` input.
// Missing file: skipped in a vendored copy, a failure under
// ADT_REQUIRE_VECTORS=1 (CI, and the public SDK repo's CI).

type widgetVector struct {
	Name     string    `json:"name"`
	Secret   string    `json:"secret"`
	Email    string    `json:"email"`
	Account  string    `json:"account"`
	Role     string    `json:"role"`
	Chat     *[]string `json:"chat"` // null: not stated; []: may not chat
	IssuedAt int64     `json:"issued_at"`
	Token    string    `json:"token"`
}

type widgetVectorFile struct {
	Format       string         `json:"format"`
	Capabilities []string       `json:"capabilities"`
	Sign         []widgetVector `json:"sign"`
	Refuse       []widgetVector `json:"refuse"`
}

func loadWidgetVectors(t *testing.T) widgetVectorFile {
	t.Helper()
	var vf widgetVectorFile
	if err := json.Unmarshal(readVectors(t, "widget_identity_vectors.json"), &vf); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(vf.Sign) == 0 || len(vf.Refuse) == 0 {
		t.Fatal("vector file has no sign or refuse cases; this test would prove nothing")
	}
	if !strings.HasPrefix(vf.Format, widgetPrefix+".") {
		t.Fatalf("vector format %q is not %s: a new prefix needs a new signer", vf.Format, widgetPrefix)
	}
	return vf
}

func (v widgetVector) chat() (bool, []string) {
	if v.Chat == nil {
		return false, nil
	}
	return true, *v.Chat
}

func TestWidgetVectors_SignReproducedExactly(t *testing.T) {
	vf := loadWidgetVectors(t)
	for _, v := range vf.Sign {
		stated, chat := v.chat()
		got, err := signWidget(v.Secret, v.Email, v.Account, v.Role, stated, chat, time.Unix(v.IssuedAt, 0))
		if err != nil {
			t.Errorf("%s: refused (%v); want %s", v.Name, err, v.Token)
			continue
		}
		if got != v.Token {
			t.Errorf("%s:\n  got  %s\n  want %s\n  the chat service would reject this user's widget", v.Name, got, v.Token)
		}
	}
}

func TestWidgetVectors_RefuseCasesAreRefused(t *testing.T) {
	vf := loadWidgetVectors(t)
	for _, v := range vf.Refuse {
		stated, chat := v.chat()
		got, err := signWidget(v.Secret, v.Email, v.Account, v.Role, stated, chat, time.Unix(1791460740, 0))
		if err == nil {
			t.Errorf("%s: signed %s; must refuse", v.Name, got)
			continue
		}
		if !errors.Is(err, ErrWidgetIdentity) || got != "" {
			t.Errorf("%s: got (%q, %v); want (\"\", ErrWidgetIdentity)", v.Name, got, err)
		}
	}
}

// The closed capability list is the contract's, not a copy that drifts.
func TestWidgetVectors_CapabilitiesMatchTheContract(t *testing.T) {
	vf := loadWidgetVectors(t)
	if strings.Join(vf.Capabilities, ",") != strings.Join(widgetCapabilities, ",") {
		t.Errorf("capabilities %v, contract says %v", widgetCapabilities, vf.Capabilities)
	}
}

// decodeWidgetToken checks the MAC and returns the five payload fields. The
// tests use it to read what the public API signed.
func decodeWidgetToken(t *testing.T, secret, token string) []string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != widgetPrefix {
		t.Fatalf("token %q is not dbw1.<payload>.<mac>", token)
	}
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(widgetPrefix + "." + parts[1]))
	if !hmac.Equal([]byte(parts[2]), []byte(base64.RawURLEncoding.EncodeToString(h.Sum(nil)))) {
		t.Fatalf("token %q: MAC does not verify with the secret", token)
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	fields := strings.Split(string(raw), "\n")
	if len(fields) != 5 {
		t.Fatalf("payload has %d fields, want 5: %q", len(fields), raw)
	}
	return fields
}

const testWidgetSecret = "dbws_k7x2m4q9r3t6v8w1y5z0a2b4c6d8e0f1g3h5j7k9m2n4p6q8"

// The public API: issued now, options mapped to the role and chat fields.
func TestWidgetIdentity_SignsNowWithOptions(t *testing.T) {
	cases := []struct {
		name     string
		opts     []WidgetOption
		role     string
		chat     string
		wantFail bool
	}{
		{name: "no options", chat: ""},
		{name: "role", opts: []WidgetOption{WidgetRole(" dealer_staff ")}, role: "dealer_staff"},
		{name: "chat", opts: []WidgetOption{WidgetChat("report", "howto", "report")}, chat: "howto,report"},
		{name: "no chat", opts: []WidgetOption{WidgetNoChat()}, chat: "none"},
		{name: "role and chat", opts: []WidgetOption{WidgetRole("admin"), WidgetChat("handoff")}, role: "admin", chat: "handoff"},
		{name: "last chat option wins", opts: []WidgetOption{WidgetChat("handoff"), WidgetNoChat()}, chat: "none"},
		{name: "nil option ignored", opts: []WidgetOption{nil, WidgetRole("viewer")}, role: "viewer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := time.Now().Unix()
			token, err := WidgetIdentity(testWidgetSecret, User{Email: " Pat@Dealer.com ", Account: "dealer-1182"}, tc.opts...)
			after := time.Now().Unix()
			if err != nil {
				t.Fatalf("WidgetIdentity: %v", err)
			}
			f := decodeWidgetToken(t, testWidgetSecret, token)
			ts, err := strconv.ParseInt(f[0], 10, 64)
			if err != nil || ts < before || ts > after {
				t.Errorf("issued_at %q, want between %d and %d", f[0], before, after)
			}
			want := []string{f[0], "pat@dealer.com", "dealer-1182", tc.role, tc.chat}
			if strings.Join(f, "|") != strings.Join(want, "|") {
				t.Errorf("payload %q, want %q", f, want)
			}
		})
	}
}

func TestWidgetIdentity_RefusesWithAnError(t *testing.T) {
	cases := []struct {
		name   string
		secret string
		user   User
		opts   []WidgetOption
	}{
		{"no secret", "", User{Email: "pat@dealer.com"}, nil},
		{"no identity", testWidgetSecret, User{}, []WidgetOption{WidgetRole("admin")}},
		{"WidgetChat with no names", testWidgetSecret, User{Email: "pat@dealer.com"}, []WidgetOption{WidgetChat()}},
		{"empty slice spread into WidgetChat", testWidgetSecret, User{Email: "pat@dealer.com"}, []WidgetOption{WidgetChat([]string{}...)}},
		{"unknown capability", testWidgetSecret, User{Email: "pat@dealer.com"}, []WidgetOption{WidgetChat("admin")}},
		{"line break in role", testWidgetSecret, User{Email: "pat@dealer.com"}, []WidgetOption{WidgetRole("a\nb")}},
		{"NUL at the end of the account", testWidgetSecret, User{Account: "acct\x00"}, nil},
		// Not repaired to U+FFFD (strings.ToLower would): refused.
		{"invalid UTF-8 in the email", testWidgetSecret, User{Email: "pat\xff@dealer.com"}, nil},
		{"invalid UTF-8 in the account", testWidgetSecret, User{Account: "acct\xff"}, nil},
		// The sensor identity truncates at 255; the widget must not sign the
		// truncated (different) person.
		{"long email", testWidgetSecret, User{Email: strings.Repeat("a", 251) + "@x.io"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token, err := WidgetIdentity(tc.secret, tc.user, tc.opts...)
			if err == nil || !errors.Is(err, ErrWidgetIdentity) || token != "" {
				t.Fatalf("got (%q, %v); want (\"\", ErrWidgetIdentity)", token, err)
			}
		})
	}
}

// Every C0 control character and DEL is refused inside every field, and at
// the ends unless trimming removes it (#172; the reference signer,
// internal/widgetid, and Capability 6). NUL at an end is the case a
// strip-like trim (Ruby's String#strip) would wrongly remove and sign.
func TestWidgetIdentity_RefusesControlCharacters(t *testing.T) {
	quietWidgetLog(t)
	controls := []rune{0x7f}
	for r := rune(0); r < 0x20; r++ {
		controls = append(controls, r)
	}
	type input struct {
		user User
		role string
	}
	for _, r := range controls {
		c := string(r)
		for field, in := range map[string]input{
			"email":   {User{Email: "pat" + c + "@dealer.com", Account: "dealer-1182"}, ""},
			"account": {User{Email: "pat@dealer.com", Account: "dealer" + c + "1182"}, ""},
			"role":    {User{Email: "pat@dealer.com", Account: "dealer-1182"}, "admin" + c + "owner"},
		} {
			token, err := WidgetIdentity(testWidgetSecret, in.user, WidgetRole(in.role))
			if !errors.Is(err, ErrWidgetIdentity) || token != "" {
				t.Errorf("U+%04X inside %s: got (%q, %v); want refused", r, field, token, err)
			}
		}
	}
	for _, end := range []string{"\x00", "\x1f", "\x7f", "\x1b"} {
		for field, in := range map[string]input{
			"email":   {User{Email: "pat@dealer.com" + end}, ""},
			"account": {User{Account: end + "dealer-1182"}, ""},
			"role":    {User{Account: "dealer-1182"}, "admin" + end},
		} {
			token, err := WidgetIdentity(testWidgetSecret, in.user, WidgetRole(in.role))
			if !errors.Is(err, ErrWidgetIdentity) || token != "" {
				t.Errorf("%q at an end of %s: got (%q, %v); want refused", end, field, token, err)
			}
		}
	}
}

// The trimmed whitespace (TAB, LF, VT, FF, CR, space) is C0 too: at the ends
// it is trimmed, never refused, and the token is the clean identity's.
func TestWidgetIdentity_TrimmedWhitespaceStillSigns(t *testing.T) {
	at := time.Unix(1_700_000_000, 0)
	want, err := signWidget(testWidgetSecret, "pat@dealer.com", "dealer-1182", "admin", false, nil, at)
	if err != nil {
		t.Fatal(err)
	}
	for _, ws := range []string{" ", "\t", "\n", "\v", "\f", "\r", " \t\n\v\f\r"} {
		got, err := signWidget(testWidgetSecret, ws+"Pat@Dealer.com"+ws, ws+"dealer-1182"+ws, ws+"admin"+ws, false, nil, at)
		if err != nil {
			t.Errorf("%q around every field: %v", ws, err)
			continue
		}
		if got != want {
			t.Errorf("%q around every field: token differs from the trimmed identity's", ws)
		}
	}
}

// quietWidgetLog keeps the once-per-process refusal warning out of the test
// output and leaves it armed for the test that checks it.
func quietWidgetLog(t *testing.T) {
	t.Helper()
	prev := widgetLogf
	widgetLogf = func(string, ...any) {}
	t.Cleanup(func() { widgetLogf = prev; widgetWarned = sync.Once{} })
}

// Unicode whitespace is data, not trimmed: Ruby's strip keeps U+00A0, and
// the contract names ASCII whitespace only.
func TestWidgetIdentity_TrimsASCIIWhitespaceOnly(t *testing.T) {
	token, err := WidgetIdentity(testWidgetSecret, User{Account: "\u00a0acct\t"})
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeWidgetToken(t, testWidgetSecret, token)[2]; got != "\u00a0acct" {
		t.Errorf("account %q, want %q", got, "\u00a0acct")
	}
}

// One warning per process, whatever the reason, and never the identity.
func TestWidgetIdentity_WarnsOncePerProcess(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	prevLog := widgetLogf
	widgetLogf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	widgetWarned = sync.Once{}
	t.Cleanup(func() { widgetLogf = prevLog })

	for i := 0; i < 3; i++ {
		if _, err := WidgetIdentity("", User{Email: "secret.person@dealer.com"}); err == nil {
			t.Fatal("signed without a secret")
		}
	}
	if _, err := WidgetIdentity(testWidgetSecret, User{Email: "secret.person@dealer.com"}, WidgetChat("nope")); err == nil {
		t.Fatal("signed an unknown capability")
	}
	if len(lines) != 1 {
		t.Fatalf("logged %d lines, want 1: %q", len(lines), lines)
	}
	if strings.Contains(lines[0], "secret.person") {
		t.Errorf("warning names the user: %q", lines[0])
	}
	if !strings.Contains(lines[0], "widget identity") {
		t.Errorf("warning does not say what failed: %q", lines[0])
	}
}
