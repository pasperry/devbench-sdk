package devbench

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Known-answer session tokens. Ingest verifies every token with this
// package's VerifySession, so a token the Ruby SDK mints must be these exact
// bytes too: sdk/server/ruby/test/session_test.rb pins the same table. There
// are no shared testdata/ vectors for session tokens yet (#146); until there
// are, change a row here only together with its Ruby twin.
//
// Fixed inputs: secret katSecret, tenant "acme", clock 1700000000, ttl one
// hour, so every payload's expiry is 1700003600.
const (
	katSecret = "a-tenant-session-signing-secret-32b"
	katNow    = 1700000000
	katExpiry = 1700003600
)

var sessionKAT = []struct {
	name        string
	subject     string
	keptSubject string // after truncation to at most maxSubjectLen bytes
	token       string
}{
	{"plain subject", "user-42", "user-42",
		"adts1.YWNtZXwxNzAwMDAzNjAwfHVzZXItNDI.HPdOJfrw2tnU5pQmLcQ5847z3r0yZUFj5sWFUpAU0Zg"},
	{"no subject", "", "",
		"adts1.YWNtZXwxNzAwMDAzNjAwfA.fyO1jBICsrQrfXC_hDEAyieu2os3fSH2ZqwFUfnNSJY"},
	{"ascii over the bound keeps 128 bytes", strings.Repeat("x", 200), strings.Repeat("x", 128),
		"adts1.YWNtZXwxNzAwMDAzNjAwfHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4.c9UIiw5LEm3XVeOQOK19XVznTEAUXWbaC5SaYHfGSRg"},
	{"two-byte characters ending on the bound", strings.Repeat("é", 100), strings.Repeat("é", 64),
		"adts1.YWNtZXwxNzAwMDAzNjAwfMOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOp.4veV5h1Pe1wXkPuIvSTClWySEroW3o3rMhmYb2W5HZA"},
	{"two-byte character straddling the bound", "a" + strings.Repeat("é", 100), "a" + strings.Repeat("é", 63),
		"adts1.YWNtZXwxNzAwMDAzNjAwfGHDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6nDqcOpw6k.kEZibMkUk7hrvzUdgOM9ZxPMY4kDXyP_FI_5BvWZjl0"},
	{"three-byte character straddling the bound", strings.Repeat("€", 60), strings.Repeat("€", 42),
		"adts1.YWNtZXwxNzAwMDAzNjAwfOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrOKCrA.WVG9xXu5n2KEON0WELOMrGslRZxlUPdIF6XFODlCQ-g"},
	{"four-byte character straddling the bound", "a" + strings.Repeat("😀", 40), "a" + strings.Repeat("😀", 31),
		"adts1.YWNtZXwxNzAwMDAzNjAwfGHwn5iA8J-YgPCfmIDwn5iA8J-YgPCfmIDwn5iA8J-YgPCfmIDwn5iA8J-YgPCfmIDwn5iA8J-YgPCfmIDwn5iA8J-YgPCfmIDwn5iA8J-YgPCfmIDwn5iA8J-YgPCfmIDwn5iA8J-YgPCfmIDwn5iA8J-YgPCfmIDwn5iA.3v9yN5wgqPCZhiE0zVleLmrlcUKAPlh_waGoCy4PuWU"},
}

func pinSessionClock(t *testing.T, unix int64) {
	t.Helper()
	sessionNow = func() time.Time { return time.Unix(unix, 0) }
	t.Cleanup(func() { sessionNow = time.Now })
}

func TestSessionKAT_MintIsByteExact(t *testing.T) {
	pinSessionClock(t, katNow)
	for _, tc := range sessionKAT {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MintSession(katSecret, "acme", tc.subject, time.Hour)
			if err != nil {
				t.Fatalf("MintSession: %v", err)
			}
			if got != tc.token {
				t.Errorf("minted\n  %s\nwant\n  %s", got, tc.token)
			}
		})
	}
}

func TestSessionKAT_VerifyReadsBackTheFields(t *testing.T) {
	pinSessionClock(t, katNow)
	for _, tc := range sessionKAT {
		t.Run(tc.name, func(t *testing.T) {
			got, err := VerifySession(katSecret, tc.token)
			if err != nil {
				t.Fatalf("VerifySession: %v", err)
			}
			if got.Tenant != "acme" || got.Subject != tc.keptSubject || got.Expires.Unix() != katExpiry {
				t.Errorf("verified %q / %q / %d, want acme / %q / %d",
					got.Tenant, got.Subject, got.Expires.Unix(), tc.keptSubject, katExpiry)
			}
		})
	}
}

// The expiry second itself still verifies; the next second does not. The Ruby
// SDK's verifier draws the line in the same place.
func TestSessionKAT_ExpiryBoundary(t *testing.T) {
	token := sessionKAT[0].token

	pinSessionClock(t, katExpiry)
	if _, err := VerifySession(katSecret, token); err != nil {
		t.Errorf("at the expiry second: %v", err)
	}

	pinSessionClock(t, katExpiry+1)
	if _, err := VerifySession(katSecret, token); !errors.Is(err, ErrSessionExpired) {
		t.Errorf("one second past expiry: %v, want ErrSessionExpired", err)
	}
}
