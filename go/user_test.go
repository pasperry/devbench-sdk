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

	// Matched by symbol, not by order: the writer may put the two reports on
	// two connections, and nothing orders across connections (#549).
	got := map[any]map[string]any{}
	for i := 0; i < 2; i++ {
		m := s.next(t)
		got[m["symbol"]] = m
	}

	with, ok := got["with"]
	if !ok {
		t.Fatalf("the report with a user never arrived: %v", got)
	}
	u, ok := with["user"].(map[string]any)
	if !ok {
		t.Fatalf("user missing: %v", with)
	}
	if u["email"] != "pat@example.com" || u["account"] != "acct-1182" {
		t.Errorf("user = %v", u)
	}

	without, ok := got["without"]
	if !ok {
		t.Fatalf("the report without a user never arrived: %v", got)
	}
	if _, present := without["user"]; present {
		t.Errorf("user present with no identity set: %v", without)
	}
}
