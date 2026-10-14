package devbench

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// testdata/session_vectors.json (#181): the shared adts1 session-token
// vectors. Ingest verifies every token with this package's VerifySession,
// and the Ruby SDK (sdk/server/ruby/test/session_test.rb) reads the same
// file, so a token either SDK mints is the same bytes. Each row pins the
// session clock to its own `now`. Missing file: skipped in a vendored copy,
// a failure under ADT_REQUIRE_VECTORS=1 (CI, and the public SDK repo's CI).

type sessionVectorFile struct {
	Format            string `json:"format"`
	MaxSubjectBytes   int    `json:"max_subject_bytes"`
	DefaultTTLSeconds int64  `json:"default_ttl_seconds"`
	Mint              []struct {
		Name        string `json:"name"`
		Secret      string `json:"secret"`
		Tenant      string `json:"tenant"`
		Subject     string `json:"subject"`
		Now         int64  `json:"now"`
		TTLSeconds  int64  `json:"ttl_seconds"`
		Token       string `json:"token"`
		SubjectKept string `json:"subject_kept"`
	} `json:"mint"`
	Refuse []struct {
		Name    string `json:"name"`
		Secret  string `json:"secret"`
		Tenant  string `json:"tenant"`
		Subject string `json:"subject"`
	} `json:"refuse"`
	Verify []struct {
		Name   string `json:"name"`
		Secret string `json:"secret"`
		Token  string `json:"token"`
		Now    int64  `json:"now"`
		Result string `json:"result"`
		Claims *struct {
			Tenant  string `json:"tenant"`
			Subject string `json:"subject"`
			Expires int64  `json:"expires"`
		} `json:"claims"`
	} `json:"verify"`
}

func loadSessionVectors(t *testing.T) sessionVectorFile {
	t.Helper()
	var vf sessionVectorFile
	if err := json.Unmarshal(readVectors(t, "session_vectors.json"), &vf); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(vf.Mint) == 0 || len(vf.Refuse) == 0 || len(vf.Verify) == 0 {
		t.Fatal("vector file is missing a mint, refuse or verify section; this test would prove nothing")
	}
	return vf
}

func pinSessionClock(t *testing.T, unix int64) {
	t.Helper()
	sessionNow = func() time.Time { return time.Unix(unix, 0) }
	t.Cleanup(func() { sessionNow = time.Now })
}

func TestSessionVectors_Constants(t *testing.T) {
	vf := loadSessionVectors(t)
	if vf.MaxSubjectBytes != maxSubjectLen {
		t.Errorf("max_subject_bytes = %d, this SDK cuts at %d", vf.MaxSubjectBytes, maxSubjectLen)
	}
	if time.Duration(vf.DefaultTTLSeconds)*time.Second != DefaultSessionTTL {
		t.Errorf("default_ttl_seconds = %d, this SDK defaults to %s", vf.DefaultTTLSeconds, DefaultSessionTTL)
	}
}

func TestSessionVectors_MintIsByteExact(t *testing.T) {
	for _, tc := range loadSessionVectors(t).Mint {
		t.Run(tc.Name, func(t *testing.T) {
			pinSessionClock(t, tc.Now)
			got, err := MintSession(tc.Secret, tc.Tenant, tc.Subject, time.Duration(tc.TTLSeconds)*time.Second)
			if err != nil {
				t.Fatalf("MintSession: %v", err)
			}
			if got != tc.Token {
				t.Errorf("minted\n  %s\nwant\n  %s", got, tc.Token)
			}
			// The kept subject is what the token carries.
			claims, err := VerifySession(tc.Secret, got)
			if err != nil {
				t.Fatalf("VerifySession of the minted token: %v", err)
			}
			if claims.Subject != tc.SubjectKept {
				t.Errorf("subject kept %q, want %q", claims.Subject, tc.SubjectKept)
			}
		})
	}
}

func TestSessionVectors_RefuseCasesAreRefused(t *testing.T) {
	for _, tc := range loadSessionVectors(t).Refuse {
		t.Run(tc.Name, func(t *testing.T) {
			if got, err := MintSession(tc.Secret, tc.Tenant, tc.Subject, time.Hour); err == nil {
				t.Errorf("MintSession minted %q, want a refusal", got)
			}
		})
	}
}

func TestSessionVectors_Verify(t *testing.T) {
	for _, tc := range loadSessionVectors(t).Verify {
		t.Run(tc.Name, func(t *testing.T) {
			pinSessionClock(t, tc.Now)
			got, err := VerifySession(tc.Secret, tc.Token)
			switch tc.Result {
			case "valid":
				if err != nil {
					t.Fatalf("VerifySession: %v, want valid", err)
				}
				if tc.Claims == nil {
					t.Fatal("a valid row without claims proves nothing")
				}
				if got.Tenant != tc.Claims.Tenant || got.Subject != tc.Claims.Subject || got.Expires.Unix() != tc.Claims.Expires {
					t.Errorf("verified %q / %q / %d, want %q / %q / %d",
						got.Tenant, got.Subject, got.Expires.Unix(), tc.Claims.Tenant, tc.Claims.Subject, tc.Claims.Expires)
				}
			case "expired":
				if !errors.Is(err, ErrSessionExpired) {
					t.Errorf("VerifySession: %v, want ErrSessionExpired", err)
				}
			case "invalid":
				if !errors.Is(err, ErrSessionInvalid) {
					t.Errorf("VerifySession: %v, want ErrSessionInvalid", err)
				}
			default:
				t.Fatalf("unknown result %q", tc.Result)
			}
		})
	}
}
