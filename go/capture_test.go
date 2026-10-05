package devbench

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
)

func TestCaptureException_SendsAnExplicitException(t *testing.T) {
	s := listenSidecar(t)

	ctx := WithUser(WithTrace(context.Background(), Trace{Session: "s1", Intent: "i1", Hop: 1}), User{Account: "acct-9"})
	_, _, line, _ := runtime.Caller(0)
	CaptureException(ctx, fmt.Errorf("reconcile: %w", &conflictError{key: "batch_7"}))

	m := s.next(t)
	checks := map[string]any{
		"v":       float64(1),
		"kind":    "exception",
		"context": "explicit",
		"handled": true,
		"error":   "*devbench.conflictError",
		"message": "reconcile: conflict on batch_7",
		"trace":   "v1/s1/i1/1",
	}
	for k, want := range checks {
		if m[k] != want {
			t.Errorf("%s = %#v, want %#v", k, m[k], want)
		}
	}
	if u, _ := m["user"].(map[string]any); u["account"] != "acct-9" {
		t.Errorf("user = %v", m["user"])
	}
	if _, ok := m["symbol"]; ok {
		t.Errorf("symbol = %v outside a request", m["symbol"])
	}

	// The caller's frame is innermost; CaptureException itself is not shown.
	top := frames(t, m)[0]
	if !strings.HasSuffix(top["function"].(string), "devbench-sdk/go.TestCaptureException_SendsAnExplicitException") {
		t.Errorf("innermost frame = %v, want the caller", top)
	}
	if top["file"] != "capture_test.go" || top["line"] != float64(line+1) {
		t.Errorf("frame location = %v:%v, want capture_test.go:%d", top["file"], top["line"], line+1)
	}
}

func TestCaptureException_NilIsIgnored(t *testing.T) {
	s := listenSidecar(t)
	CaptureException(context.Background(), nil)
	CaptureException(nil, nil) //nolint:staticcheck // a nil context must not panic either
	send(message{V: 1, Kind: "handled_failure", Symbol: "marker"})

	if m := s.next(t); m["symbol"] != "marker" {
		t.Errorf("a nil error was reported: %v", m)
	}
}

func TestCaptureException_InARequestUsesTheRoutePattern(t *testing.T) {
	s := listenSidecar(t)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /batches/{id}/reconcile", func(w http.ResponseWriter, r *http.Request) {
		CaptureException(r.Context(), &conflictError{key: r.PathValue("id")})
		w.WriteHeader(http.StatusAccepted)
	})
	srv, _ := panicServer(t, mux)

	res, err := http.Post(srv.URL+"/batches/77/reconcile", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	m := s.next(t)
	if m["symbol"] != wantSymbol("POST /batches/{id}/reconcile") || m["context"] != "explicit" {
		t.Errorf("got %v", m)
	}
}

// Captured, then panicked with: one error, one report.
func TestCaptureException_ThenPanicReportsOnce(t *testing.T) {
	s := listenSidecar(t)
	srv, caught := panicServer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		err := &conflictError{key: "once"}
		CaptureException(r.Context(), err)
		CaptureException(r.Context(), err)
		panic(err)
	}))

	get(t, srv.URL+"/x", "")
	if _, ok := caughtValue(t, caught).(*conflictError); !ok {
		t.Error("the panic was not re-raised")
	}

	if m := s.next(t); m["context"] != "explicit" {
		t.Fatalf("first report = %v", m)
	}
	send(message{V: 1, Kind: "handled_failure", Symbol: "marker"})
	if m := s.next(t); m["symbol"] != "marker" {
		t.Errorf("the same error was reported again: %v", m)
	}
}

// Dedupe is per request: a shared sentinel is a fresh failure each time.
func TestCaptureException_SentinelIsReportedInEachRequest(t *testing.T) {
	s := listenSidecar(t)
	srv, _ := panicServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		CaptureException(r.Context(), io.ErrUnexpectedEOF)
		w.WriteHeader(http.StatusOK)
	}))

	get(t, srv.URL+"/a", "")
	get(t, srv.URL+"/b", "")
	for i := 0; i < 2; i++ {
		if m := s.next(t); m["message"] != "unexpected EOF" {
			t.Fatalf("report %d = %v", i, m)
		}
	}
}
