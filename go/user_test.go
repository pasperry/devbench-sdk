package devbench

import (
	"context"
	"strings"
	"testing"
)

func TestWithUser_Normalizes(t *testing.T) {
	long := strings.Repeat("é", 300) // multi-byte: truncation must count characters

	cases := []struct {
		name string
		in   User
		want User
	}{
		{"email trimmed and lowercased", User{Email: "  Pat@Example.COM \n", Account: "acct-1182"}, User{Email: "pat@example.com", Account: "acct-1182"}},
		{"account kept verbatim", User{Account: "Acct 1182"}, User{Account: "Acct 1182"}},
		{"email truncated to 255 chars", User{Email: long}, User{Email: strings.Repeat("é", 255)}},
		{"account truncated to 255 chars", User{Account: long}, User{Account: strings.Repeat("é", 255)}},
		{"exactly 255 kept", User{Account: strings.Repeat("a", 255)}, User{Account: strings.Repeat("a", 255)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := UserFromContext(WithUser(context.Background(), tc.in))
			if !ok {
				t.Fatal("no user on the context")
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestWithUser_EmptyMeansNoUser(t *testing.T) {
	if _, ok := UserFromContext(context.Background()); ok {
		t.Error("a bare context has a user")
	}
	ctx := WithUser(context.Background(), User{Email: "pat@example.com"})
	ctx = WithUser(ctx, User{Email: "   "})
	if u, ok := UserFromContext(ctx); ok {
		t.Errorf("an all-blank user should clear identity, got %+v", u)
	}
}

// On the wire, identity is its own object and is omitted when unset.
func TestUser_OnTheWire(t *testing.T) {
	s := listenSidecar(t)

	ctx := WithUser(context.Background(), User{Email: "Pat@Example.com", Account: "acct-1182"})
	send(message{V: 1, Kind: "handled_failure", Symbol: "with", User: userField(ctx)})
	send(message{V: 1, Kind: "handled_failure", Symbol: "without", User: userField(context.Background())})

	m := s.next(t)
	u, ok := m["user"].(map[string]any)
	if !ok {
		t.Fatalf("user missing: %v", m)
	}
	if u["email"] != "pat@example.com" || u["account"] != "acct-1182" {
		t.Errorf("user = %v", u)
	}

	m = s.next(t)
	if _, present := m["user"]; present {
		t.Errorf("user present with no identity set: %v", m)
	}
}
