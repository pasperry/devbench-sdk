package devbench_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	devbench "github.com/pasperry/devbench-sdk/go"
)

const testSecret = "a-tenant-session-signing-secret-32b"

func TestSession_RoundTrip(t *testing.T) {
	token, err := devbench.MintSession(testSecret, "acme", "user-42", time.Hour)
	if err != nil {
		t.Fatalf("MintSession: %v", err)
	}

	got, err := devbench.VerifySession(testSecret, token)
	if err != nil {
		t.Fatalf("VerifySession: %v", err)
	}
	if got.Tenant != "acme" || got.Subject != "user-42" {
		t.Errorf("verified %+v", got)
	}
	if time.Until(got.Expires) < 50*time.Minute {
		t.Errorf("expiry %v is sooner than the ttl asked for", got.Expires)
	}
}

// The whole point: a token minted by the customer's backend cannot be forged by
// someone who only has the public ingest key.
func TestSession_CannotBeForgedWithoutTheSecret(t *testing.T) {
	token, _ := devbench.MintSession(testSecret, "acme", "", time.Hour)

	if _, err := devbench.VerifySession("a-different-secret-entirely-0000000", token); !errors.Is(err, devbench.ErrSessionInvalid) {
		t.Fatalf("a token verified under the wrong secret: %v", err)
	}
}

func TestSession_TamperedTokenIsRejected(t *testing.T) {
	token, _ := devbench.MintSession(testSecret, "acme", "user-42", time.Hour)
	parts := strings.Split(token, ".")

	// Rewrite the payload to claim another tenant, keeping the old signature.
	forged, _ := devbench.MintSession("someone-elses-secret-000000000000", "globex", "", time.Hour)
	forgedParts := strings.Split(forged, ".")

	swapped := parts[0] + "." + forgedParts[1] + "." + parts[2]
	if _, err := devbench.VerifySession(testSecret, swapped); !errors.Is(err, devbench.ErrSessionInvalid) {
		t.Errorf("a token with a swapped payload verified: %v", err)
	}

	// Flip a byte in the signature.
	sig := []byte(parts[2])
	sig[len(sig)-1] = flipChar(sig[len(sig)-1])
	if _, err := devbench.VerifySession(testSecret, parts[0]+"."+parts[1]+"."+string(sig)); !errors.Is(err, devbench.ErrSessionInvalid) {
		t.Errorf("a token with a flipped signature verified: %v", err)
	}
}

// Expiry bounds the damage from a scraped token: an attacker who loads the
// customer's page gets a token, not a permanent credential.
func TestSession_ExpiresAndSaysSo(t *testing.T) {
	token, err := devbench.MintSession(testSecret, "acme", "", -time.Minute)
	if err != nil {
		t.Fatalf("MintSession: %v", err)
	}

	// A negative TTL falls back to the default, so mint one that is genuinely
	// expired by hand-rolling the payload through a very short TTL.
	short, _ := devbench.MintSession(testSecret, "acme", "", time.Nanosecond)
	time.Sleep(2 * time.Millisecond)

	if _, err := devbench.VerifySession(testSecret, short); !errors.Is(err, devbench.ErrSessionExpired) {
		t.Errorf("an expired token verified: %v", err)
	}

	// The non-expired one from the default TTL still works.
	if _, err := devbench.VerifySession(testSecret, token); err != nil {
		t.Errorf("a token with a negative ttl did not fall back to the default: %v", err)
	}
}

// A pipe in a field would let a crafted subject forge a different tenant,
// because the payload is pipe-delimited.
func TestSession_RejectsDelimiterInjection(t *testing.T) {
	if _, err := devbench.MintSession(testSecret, "acme", "evil|globex|9999999999|", time.Hour); err == nil {
		t.Error("a subject containing the payload delimiter was accepted")
	}
	if _, err := devbench.MintSession(testSecret, "acme|x", "", time.Hour); err == nil {
		t.Error("a tenant containing the payload delimiter was accepted")
	}
}

func TestSession_RequiresSecretAndTenant(t *testing.T) {
	if _, err := devbench.MintSession("", "acme", "", time.Hour); err == nil {
		t.Error("minted with no secret")
	}
	if _, err := devbench.MintSession(testSecret, "", "", time.Hour); err == nil {
		t.Error("minted with no tenant")
	}
}

func TestSession_MalformedTokensAreRejected(t *testing.T) {
	for _, bad := range []string{
		"", "garbage", "adts1", "adts1.only-two", "adts2.abc.def",
		"adts1..", "adts1.!!!.xyz",
	} {
		if _, err := devbench.VerifySession(testSecret, bad); err == nil {
			t.Errorf("VerifySession(%q) accepted a malformed token", bad)
		}
	}
}

// Automatic injection is the point: the customer already installs the
// middleware, and threading a token through every template would be real work
// in their codebase for something we can do here.
func TestWithSession_InjectsIntoHTML(t *testing.T) {
	handler := devbench.WithSession(devbench.SessionOptions{Secret: testSecret, Tenant: "acme"})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<!doctype html><html><head><title>App</title></head><body>hi</body></html>"))
		}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	if !strings.Contains(body, `name="adt-session"`) {
		t.Fatalf("no session meta tag injected:\n%s", body)
	}
	// It must land inside head, not after the document.
	if strings.Index(body, "adt-session") > strings.Index(body, "</head>") {
		t.Error("the meta tag was injected outside <head>")
	}
	if !strings.Contains(body, "<title>App</title>") {
		t.Error("the original document was damaged")
	}

	token := between(body, `content="`, `"`)
	if _, err := devbench.VerifySession(testSecret, token); err != nil {
		t.Errorf("the injected token does not verify: %v", err)
	}
}

// Buffering someone's entire JSON or download response to add a meta tag they
// will never read would be a poor trade in a library running inside their app.
func TestWithSession_LeavesNonHTMLAlone(t *testing.T) {
	const payload = `{"ok":true}`

	handler := devbench.WithSession(devbench.SessionOptions{Secret: testSecret, Tenant: "acme"})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(payload))
		}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/thing", nil))

	if rec.Body.String() != payload {
		t.Errorf("a JSON response was modified: %q", rec.Body.String())
	}
}

// A stale Content-Length truncates the page in the browser.
func TestWithSession_ClearsContentLength(t *testing.T) {
	handler := devbench.WithSession(devbench.SessionOptions{Secret: testSecret, Tenant: "acme"})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Content-Length", "52")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("<html><head></head><body>hello there</body></html>"))
		}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := rec.Header().Get("Content-Length"); got != "" {
		t.Errorf("Content-Length = %q after injection; the browser would truncate the page", got)
	}
}

// Misconfiguration must never break the customer's site.
func TestWithSession_PassesThroughWithoutConfiguration(t *testing.T) {
	const page = "<html><head></head><body>fine</body></html>"

	handler := devbench.WithSession(devbench.SessionOptions{})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(page))
		}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Body.String() != page {
		t.Errorf("an unconfigured injector altered the page: %q", rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func between(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func flipChar(c byte) byte {
	if c == 'A' {
		return 'B'
	}
	return 'A'
}
