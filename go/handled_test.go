package devbench_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	devbench "github.com/pasperry/devbench-sdk/go"
)

// The canonical silent failure, from the server's side: the handler caught a
// problem and answered success anyway.
func TestHandled_SuccessfulResponseThatSwallowedAFailureIsMarked(t *testing.T) {
	h := devbench.Handled(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		devbench.ReportHandled(r.Context(), errors.New("duplicate email"), "customers.Update")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/customers/9912", nil))

	if got := rec.Header().Get(devbench.HandledHeader); got != "1" {
		t.Errorf("%s = %q, want 1 — the browser cannot otherwise tell", devbench.HandledHeader, got)
	}
}

// net/http writes an implicit 200 on the first Write, skipping WriteHeader.
// Headers set after that are discarded, so the count must be stamped on the
// way in — a detail that is easy to miss and silent when missed.
func TestHandled_ImplicitTwoHundredIsStillMarked(t *testing.T) {
	h := devbench.Handled(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		devbench.ReportHandled(r.Context(), errors.New("boom"), "x")
		_, _ = w.Write([]byte("ok")) // no WriteHeader call at all
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := rec.Header().Get(devbench.HandledHeader); got != "1" {
		t.Errorf("%s = %q on an implicit 200, want 1", devbench.HandledHeader, got)
	}
}

// A handler that writes nothing at all still produces a response.
func TestHandled_EmptyResponseIsStillMarked(t *testing.T) {
	h := devbench.Handled(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		devbench.ReportHandled(r.Context(), errors.New("boom"), "x")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := rec.Header().Get(devbench.HandledHeader); got != "1" {
		t.Errorf("%s = %q, want 1", devbench.HandledHeader, got)
	}
}

// A browser cannot read a custom header cross-origin unless the server lists
// it. Omitting this is silent: the detector never fires and the system looks
// like it is working.
func TestHandled_HeaderIsExposedForCrossOriginReaders(t *testing.T) {
	h := devbench.Handled(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		devbench.ReportHandled(r.Context(), errors.New("boom"), "x")
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	exposed := strings.ToLower(rec.Header().Get("Access-Control-Expose-Headers"))
	if !strings.Contains(exposed, devbench.HandledHeader) {
		t.Errorf("Access-Control-Expose-Headers = %q, must list %s", exposed, devbench.HandledHeader)
	}
}

func TestHandled_ApplicationsOwnExposedHeadersSurvive(t *testing.T) {
	h := devbench.Handled(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Expose-Headers", "x-request-id")
		devbench.ReportHandled(r.Context(), errors.New("boom"), "x")
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	exposed := rec.Header().Get("Access-Control-Expose-Headers")
	if !strings.Contains(exposed, "x-request-id") {
		t.Errorf("clobbered the application's own exposed headers: %q", exposed)
	}
	if !strings.Contains(strings.ToLower(exposed), devbench.HandledHeader) {
		t.Errorf("exposed = %q, missing %s", exposed, devbench.HandledHeader)
	}
}

// False positives spend a team's trust, and trust is spent once.
func TestHandled_CleanResponseCarriesNoHeader(t *testing.T) {
	h := devbench.Handled(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := rec.Header().Get(devbench.HandledHeader); got != "" {
		t.Errorf("%s = %q on a clean request", devbench.HandledHeader, got)
	}
}

// A nil error is not a handled failure. Reporting one would be a false
// positive manufactured by the SDK itself.
func TestHandled_NilErrorIsNotAFailure(t *testing.T) {
	h := devbench.Handled(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		devbench.ReportHandled(r.Context(), nil, "x")
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := rec.Header().Get(devbench.HandledHeader); got != "" {
		t.Errorf("%s = %q for a nil error", devbench.HandledHeader, got)
	}
}

// Called outside the middleware — from a background goroutine, a job, a test
// — it must do nothing rather than panic. This runs inside the handling of a
// failure that already happened.
func TestHandled_ReportingWithoutTheMiddlewareIsSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ReportHandled panicked outside a request: %v", r)
		}
	}()

	devbench.ReportHandled(context.Background(), errors.New("boom"), "background.Job")

	if n := devbench.HandledCount(context.Background()); n != 0 {
		t.Errorf("HandledCount = %d outside a request, want 0", n)
	}
}

// A Go handler fans out. The counter is shared across those goroutines, so it
// must be safe to report from all of them.
func TestHandled_ConcurrentReportsAreCountedExactly(t *testing.T) {
	const n = 200

	h := devbench.Handled(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				devbench.ReportHandled(r.Context(), errors.New("boom"), "fanout")
			}()
		}
		wg.Wait()
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := rec.Header().Get(devbench.HandledHeader); got != "200" {
		t.Errorf("%s = %q, want 200 — the counter raced", devbench.HandledHeader, got)
	}
}

// One request's count must not appear on another's response.
func TestHandled_CountsDoNotLeakBetweenRequests(t *testing.T) {
	h := devbench.Handled(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dirty" {
			devbench.ReportHandled(r.Context(), errors.New("boom"), "x")
		}
		w.WriteHeader(http.StatusOK)
	}))

	dirty := httptest.NewRecorder()
	h.ServeHTTP(dirty, httptest.NewRequest(http.MethodGet, "/dirty", nil))
	if dirty.Header().Get(devbench.HandledHeader) != "1" {
		t.Fatal("the dirty request was not marked")
	}

	clean := httptest.NewRecorder()
	h.ServeHTTP(clean, httptest.NewRequest(http.MethodGet, "/clean", nil))
	if got := clean.Header().Get(devbench.HandledHeader); got != "" {
		t.Errorf("%s = %q leaked into the next request", devbench.HandledHeader, got)
	}
}

// The detail stays on the customer's host. This header is readable by any
// script on the page.
func TestHandled_NoDetailReachesTheBrowser(t *testing.T) {
	h := devbench.Handled(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		devbench.ReportHandled(r.Context(),
			errors.New("duplicate key value violates unique constraint users_email_key"),
			"customers.Update")
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	var all strings.Builder
	for name, values := range rec.Header() {
		all.WriteString(name)
		all.WriteString(strings.Join(values, " "))
	}
	joined := all.String()

	for _, leak := range []string{"duplicate", "users_email_key", "customers.Update"} {
		if strings.Contains(joined, leak) {
			t.Errorf("response headers disclose %q: %s", leak, joined)
		}
	}
}
