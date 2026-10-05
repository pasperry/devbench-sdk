package devbench_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	devbench "github.com/pasperry/devbench-sdk/go"
)

// The test for risk #8.
//
// A common stack: Angular -> Rails -> two Go backends. If the correlation id does
// not survive every hop, client evidence cannot be joined to server logs and
// both detectors degrade to guesswork — silently, with nothing in any log to
// say why. This chains three real HTTP services and asserts one user action
// stays one user action all the way down.
func TestTraceSurvivesAChainOfServices(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []devbench.Trace
	)
	record := func(tr devbench.Trace) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, tr)
	}

	// Deepest service: records what reached it.
	last := httptest.NewServer(devbench.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tr, ok := devbench.FromContext(r.Context())
		if !ok {
			t.Error("the last service received no trace")
		}
		record(tr)
		w.WriteHeader(http.StatusOK)
	})))
	defer last.Close()

	// Middle service: records, then calls the last one *with the request
	// context*, which is what actually carries the trace forward.
	middle := httptest.NewServer(devbench.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tr, _ := devbench.FromContext(r.Context())
		record(tr)

		client := &http.Client{Transport: devbench.WrapTransport(nil)}
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, last.URL, nil)
		res, err := client.Do(req)
		if err != nil {
			t.Errorf("middle -> last: %v", err)
			return
		}
		defer res.Body.Close()
		w.WriteHeader(http.StatusOK)
	})))
	defer middle.Close()

	// Edge service, standing in for Rails.
	edge := httptest.NewServer(devbench.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tr, _ := devbench.FromContext(r.Context())
		record(tr)

		client := &http.Client{Transport: devbench.WrapTransport(nil)}
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, middle.URL, nil)
		res, err := client.Do(req)
		if err != nil {
			t.Errorf("edge -> middle: %v", err)
			return
		}
		defer res.Body.Close()
		w.WriteHeader(http.StatusOK)
	})))
	defer edge.Close()

	// The browser starts the action at hop 0.
	req, _ := http.NewRequest(http.MethodGet, edge.URL, nil)
	req.Header.Set(devbench.Header, "v1/sessABC/actXYZ/0")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("browser -> edge: %v", err)
	}
	defer res.Body.Close()

	mu.Lock()
	defer mu.Unlock()

	if len(seen) != 3 {
		t.Fatalf("%d services saw the trace, want 3", len(seen))
	}

	for i, tr := range seen {
		if tr.Key() != "sessABC/actXYZ" {
			t.Errorf("hop %d saw key %q, want sessABC/actXYZ — the action was not preserved", i, tr.Key())
		}
		if tr.Hop != i {
			t.Errorf("service %d saw hop %d, want %d", i, tr.Hop, i)
		}
	}
}

// A request built without the parent context is the other common way
// propagation breaks, and it is invisible: the call succeeds, the trace is just
// gone.
func TestTraceIsLostWithoutTheParentContext(t *testing.T) {
	var got string
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(devbench.Header)
	}))
	defer downstream.Close()

	ctx := devbench.WithTrace(context.Background(), devbench.Trace{Session: "s", Intent: "i", Hop: 0})
	client := &http.Client{Transport: devbench.WrapTransport(nil)}

	// Background context, not the traced one.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, downstream.URL, nil)
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	res.Body.Close()

	if got != "" {
		t.Errorf("a trace appeared without a traced context: %q", got)
	}

	// With the traced context it is forwarded.
	req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, downstream.URL, nil)
	res2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	res2.Body.Close()

	if got != "v1/s/i/1" {
		t.Errorf("forwarded header = %q, want v1/s/i/1", got)
	}
}

// ADT must never fail a customer's request. A mangled header is ignored.
func TestMalformedTraceIsIgnoredNotRejected(t *testing.T) {
	bad := []string{
		"", "garbage", "v2/a/b/0", "v1/a/b", "v1/a/b/c", "v1//b/0",
		"v1/a/b/-1", "v1/a/b/1000", "v1/" + strings.Repeat("x", 65) + "/b/0",
		"v1/a b/c/0", "v1/a/b/0/extra",
	}

	for _, value := range bad {
		t.Run(value, func(t *testing.T) {
			if _, ok := devbench.ParseTrace(value); ok {
				t.Errorf("ParseTrace(%q) accepted a malformed trace", value)
			}
		})
	}

	// And the request still gets served.
	served := false
	h := devbench.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = true
		if _, ok := devbench.FromContext(r.Context()); ok {
			t.Error("a malformed header produced a trace")
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(devbench.Header, "total garbage")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !served || rec.Code != http.StatusOK {
		t.Errorf("a malformed trace changed the response: served=%v code=%d", served, rec.Code)
	}
}

func TestParseTrace_RoundTrips(t *testing.T) {
	original := devbench.Trace{Session: "sess-1_A", Intent: "act_2-B", Hop: 7}
	parsed, ok := devbench.ParseTrace(original.String())
	if !ok {
		t.Fatalf("ParseTrace(%q) failed", original.String())
	}
	if parsed != original {
		t.Errorf("round trip gave %+v, want %+v", parsed, original)
	}
}

// A routing loop must not be helped along.
func TestTraceStopsPropagatingAtTheHopLimit(t *testing.T) {
	var got string
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(devbench.Header)
	}))
	defer downstream.Close()

	ctx := devbench.WithTrace(context.Background(), devbench.Trace{Session: "s", Intent: "i", Hop: 99})
	client := &http.Client{Transport: devbench.WrapTransport(nil)}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, downstream.URL, nil)
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	res.Body.Close()

	if got != "" {
		t.Errorf("forwarded at the hop limit: %q", got)
	}
}

// The trace has to reach the log line, or the sidecar has nothing to index by.
func TestLogHandlerStampsTheTrace(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(devbench.NewLogHandler(slog.NewJSONHandler(&buf, nil)))

	ctx := devbench.WithTrace(context.Background(), devbench.Trace{Session: "sessQ", Intent: "actR", Hop: 2})
	logger.InfoContext(ctx, "Completed 200 OK")
	logger.Info("no trace on this one")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d log lines, want 2", len(lines))
	}

	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("parse log line: %v", err)
	}
	if first["adt_trace"] != "v1/sessQ/actR/2" {
		t.Errorf("adt_trace = %v, want v1/sessQ/actR/2", first["adt_trace"])
	}

	var second map[string]any
	_ = json.Unmarshal([]byte(lines[1]), &second)
	if _, present := second["adt_trace"]; present {
		t.Error("a log call with no trace in context was stamped anyway")
	}
}

// The RoundTripper must not mutate the caller's request.
func TestTransportDoesNotMutateTheCallersRequest(t *testing.T) {
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer downstream.Close()

	ctx := devbench.WithTrace(context.Background(), devbench.Trace{Session: "s", Intent: "i", Hop: 0})
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, downstream.URL, nil)

	client := &http.Client{Transport: devbench.WrapTransport(nil)}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	res.Body.Close()

	if got := req.Header.Get(devbench.Header); got != "" {
		t.Errorf("the caller's request was mutated: %q", got)
	}
}
