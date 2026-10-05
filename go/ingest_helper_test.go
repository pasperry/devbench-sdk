package devbench

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A real HTTP server standing in for ingest. Nothing in the SDK is replaced:
// the direct transport makes real requests over real sockets, and the tests
// assert on the bytes that actually arrived. It decodes flushes with the
// same strictness as the real handler (unknown fields rejected), so a body
// the real ingest would refuse fails here too; the end-to-end test in
// internal/ingest runs the real one.

type gotFlush struct {
	Key         string
	ContentType string
	Size        int
	Body        wireFlush
}

type gotPut struct {
	Path        string
	Key         string // must stay empty: the ingest key never goes to storage
	ContentType string
	Raw         []byte
}

// wireFlush mirrors ingest's flushRequest exactly.
type wireFlush struct {
	V        int    `json:"v"`
	Tenant   string `json:"tenant"`
	Source   string `json:"source"`
	Service  string `json:"service"`
	Release  string `json:"release"`
	SensorID string `json:"sensor_id"`
	Counts   []struct {
		FP     string         `json:"fp"`
		N      int64          `json:"n"`
		First  int64          `json:"first"`
		Last   int64          `json:"last"`
		Kind   string         `json:"kind"`
		Sample map[string]any `json:"sample"`
		Users  []struct {
			Email   string `json:"email"`
			Account string `json:"account"`
		} `json:"users"`
		UsersOverflow int64 `json:"users_overflow"`
	} `json:"counts"`
	Overflowed int `json:"overflowed"`
	Dropped    int `json:"dropped"`
}

type fakeIngest struct {
	srv *httptest.Server

	mu       sync.Mutex
	attempts int // every request to /v1/flush, accepted or not
	flushes  []gotFlush
	puts     []gotPut
	asked    map[string]bool

	// respond decides the status of the n-th flush attempt (1-based).
	// nil means 200.
	respond func(n int) int
	// askEvidence makes the first flush of each fingerprint ask for evidence.
	askEvidence bool
	// delay holds every flush this long before answering.
	delay time.Duration
}

func newFakeIngest(t *testing.T) *fakeIngest {
	t.Helper()
	f := &fakeIngest{asked: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/flush", f.flush)
	mux.HandleFunc("/evidence/", f.put)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// dsn is a DSN pointing at this server with key k.
func (f *fakeIngest) dsn(k string) string {
	return strings.Replace(f.srv.URL, "://", "://"+k+"@", 1)
}

func (f *fakeIngest) flush(w http.ResponseWriter, r *http.Request) {
	// Read first: net/http only notices a client hanging up (and cancels
	// r.Context) once the body has been consumed.
	raw, _ := io.ReadAll(r.Body)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-r.Context().Done():
			return
		}
	}

	f.mu.Lock()
	f.attempts++
	n := f.attempts
	respond := f.respond
	f.mu.Unlock()

	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	if respond != nil {
		if status := respond(n); status != http.StatusOK {
			http.Error(w, "nope", status)
			return
		}
	}

	var body wireFlush
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		http.Error(w, "malformed request", http.StatusBadRequest)
		return
	}
	if len(body.Counts) == 0 {
		http.Error(w, "no counts", http.StatusBadRequest) // a server key may not poll
		return
	}

	type ask struct {
		FP      string `json:"fp"`
		Claim   string `json:"claim"`
		URL     string `json:"url"`
		Expires int64  `json:"expires"`
	}
	var reply struct {
		NeedEvidence []ask `json:"need_evidence,omitempty"`
	}

	f.mu.Lock()
	f.flushes = append(f.flushes, gotFlush{
		Key: r.Header.Get("X-ADT-Key"), ContentType: r.Header.Get("content-type"),
		Size: len(raw), Body: body,
	})
	if f.askEvidence {
		for _, c := range body.Counts {
			if f.asked[c.FP] {
				continue
			}
			f.asked[c.FP] = true
			reply.NeedEvidence = append(reply.NeedEvidence, ask{
				FP: c.FP, Claim: "c_" + c.FP[:6],
				URL:     f.srv.URL + "/evidence/" + c.FP + "?X-Signature=presigned-secret",
				Expires: time.Now().Add(5 * time.Minute).Unix(),
			})
		}
	}
	f.mu.Unlock()

	w.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(reply)
}

func (f *fakeIngest) put(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	if r.Method != http.MethodPut {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	f.mu.Lock()
	f.puts = append(f.puts, gotPut{
		Path: r.URL.Path, Key: r.Header.Get("X-ADT-Key"),
		ContentType: r.Header.Get("content-type"), Raw: raw,
	})
	f.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (f *fakeIngest) snapshot() (attempts int, flushes []gotFlush, puts []gotPut) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts, append([]gotFlush(nil), f.flushes...), append([]gotPut(nil), f.puts...)
}

// counts flattens every accepted count by fingerprint.
func (f *fakeIngest) counts() map[string]int64 {
	_, flushes, _ := f.snapshot()
	out := map[string]int64{}
	for _, fl := range flushes {
		for _, c := range fl.Body.Counts {
			out[c.FP] += c.N
		}
	}
	return out
}

// useDirect configures the SDK for direct mode against f for this test and
// tears it down afterwards.
func useDirect(t *testing.T, f *fakeIngest) {
	t.Helper()
	resetSDK(t)
	if err := Init(Options{DSN: f.dsn("key-direct-test"), Service: "billing", Release: "r42"}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if got := Mode(); got != "direct" {
		t.Fatalf("Mode = %q, want direct", got)
	}
}

// resetSDK stops every transport, forgets the configuration, and clears the
// environment the SDK reads, now and again when the test ends.
func resetSDK(t *testing.T) {
	t.Helper()
	stopAll := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = Close(ctx)
		resetConfig()
	}
	stopAll()
	for _, k := range []string{EnvDSN, envLegacyDSN, EnvService, EnvRelease, EnvEnabled, SocketEnv} {
		t.Setenv(k, "")
	}
	t.Cleanup(stopAll)
}

func flushNow(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
}

func closeNow(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
