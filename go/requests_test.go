package devbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Capability 2b end to end inside the SDK: real HTTP into a real ServeMux
// under the README's wiring (Recover(Middleware(Handled(mux)))), and the
// real transports — a real unix-socket listener for sidecar mode, a real
// HTTP ingest stand-in for direct mode. Nothing in the SDK is replaced; the
// only seam is requestClock, so a duration can be pinned to the
// millisecond (3,000 ms exactly is not slow, 3,001 is).

// testClock is a wall clock the test moves by hand. Handlers advance it to
// give their request a duration; tests advance it to cross a minute.
type testClock struct {
	base time.Time
	off  atomic.Int64 // nanoseconds
}

func (c *testClock) now() time.Time          { return c.base.Add(time.Duration(c.off.Load())) }
func (c *testClock) advance(d time.Duration) { c.off.Add(int64(d)) }
func (c *testClock) advanceMs(ms int64)      { c.advance(time.Duration(ms) * time.Millisecond) }
func (c *testClock) unixMilli() int64        { return c.now().UnixMilli() }
func (c *testClock) minuteStart() time.Time  { return c.now().Truncate(time.Minute) }
func (c *testClock) toNextMinute(extra time.Duration) {
	c.advance(c.minuteStart().Add(time.Minute).Sub(c.now()) + extra)
}

// useClock pins requestClock for this test. Registered before any server,
// so cleanups (LIFO) stop the servers before the clock is restored.
func useClock(t *testing.T) *testClock {
	t.Helper()
	// 2025-09-25T12:13:05Z: five seconds into a minute.
	c := &testClock{base: time.UnixMilli(int64(29313333)*60000 + 5000)}
	old := requestClock
	requestClock = c.now
	t.Cleanup(func() { requestClock = old })
	return c
}

// readmeServer serves mux under the README wiring on a real listener.
// With a clock, each request first advances it by its X-Test-Ms header
// (inside Middleware, so that is the request's duration exactly).
func readmeServer(t *testing.T, mux *http.ServeMux, clock ...*testClock) *httptest.Server {
	t.Helper()
	app := Handled(mux)
	if len(clock) == 1 {
		c, inner := clock[0], app
		app = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var ms int64
			_, _ = fmt.Sscan(r.Header.Get("X-Test-Ms"), &ms)
			c.advanceMs(ms)
			inner.ServeHTTP(w, r)
		})
	}
	srv := httptest.NewUnstartedServer(Recover(Middleware(app)))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // Recover's panic log
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// call makes one real request and reads the whole response.
func call(t *testing.T, srv *httptest.Server, method, path, trace string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if trace != "" {
		req.Header.Set(Header, trace)
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, string(body)
}

// received closes the reporter (draining it to the listener) and returns
// every line the listener decoded, once it has been quiet for a moment.
func (s *sidecar) received(t *testing.T) []map[string]any {
	t.Helper()
	closeReporter(t)
	var out []map[string]any
	for {
		select {
		case m := <-s.lines:
			out = append(out, m)
		case <-time.After(300 * time.Millisecond):
			return out
		}
	}
}

func ofKind(lines []map[string]any, kind string) []map[string]any {
	var out []map[string]any
	for _, m := range lines {
		if m["kind"] == kind {
			out = append(out, m)
		}
	}
	return out
}

func num(m map[string]any, k string) int64 {
	f, _ := m[k].(float64)
	return int64(f)
}

// signalMux is an app with ServeMux patterns.
func signalMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/reservations", func(w http.ResponseWriter, r *http.Request) {
		WithUser(r.Context(), User{Email: "Pat@Dealer.com", Account: "dealer-1182"})
		w.WriteHeader(http.StatusServiceUnavailable) // no error raised
		_, _ = w.Write([]byte("warehouse unavailable"))
	})
	mux.HandleFunc("GET /v1/stock/{sku}", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("answer") {
		case "429":
			http.Error(w, "slow down", http.StatusTooManyRequests)
		default:
			_, _ = w.Write([]byte(`{"sku":"` + r.PathValue("sku") + `"}`))
		}
	})
	mux.HandleFunc("GET /captured", func(w http.ResponseWriter, r *http.Request) {
		CaptureException(r.Context(), errors.New("ledger mismatch 4411"))
		http.Error(w, "internal", http.StatusInternalServerError)
	})
	mux.HandleFunc("GET /captured-slow", func(w http.ResponseWriter, r *http.Request) {
		CaptureException(r.Context(), errors.New("cache refill failed"))
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /panics", func(w http.ResponseWriter, r *http.Request) {
		panic(errors.New("deal vanished"))
	})
	mux.HandleFunc("PUT /deals/{id}", func(w http.ResponseWriter, r *http.Request) {
		ReportHandled(r.Context(), errors.New("stale price"), "deals.Save")
		if r.URL.Query().Get("answer") == "503" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		if r.URL.Query().Get("answer") == "503" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_, _ = w.Write([]byte("data: tick\n\n"))
	})
	mux.HandleFunc("GET /socket", func(w http.ResponseWriter, r *http.Request) {
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer conn.Close()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = brw.Flush()
	})
	mux.HandleFunc("GET /quiet", func(w http.ResponseWriter, r *http.Request) {
		// writes nothing: net/http answers 200
	})
	return mux
}

func callMs(t *testing.T, srv *httptest.Server, method, path string, ms int64) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, nil)
	req.Header.Set("X-Test-Ms", fmt.Sprint(ms))
	req.Header.Set(Header, "v1/sessR/actR/0")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestRequests_A503WithoutRaisingIsReportedOnceAsFailed(t *testing.T) {
	clock := useClock(t)
	s := listenSidecar(t)
	srv := readmeServer(t, signalMux(), clock)

	status, body := callMs(t, srv, http.MethodPost, "/v1/reservations", 2004)
	if status != http.StatusServiceUnavailable || body != "warehouse unavailable" {
		t.Fatalf("the response changed: %d %q", status, body)
	}

	lines := s.received(t)
	if len(lines) != 1 {
		t.Fatalf("lines = %v, want exactly one request message", lines)
	}
	m := lines[0]
	want := map[string]any{
		"v": float64(ProtocolVersion), "kind": "request", "signal": "failed",
		"method": "POST", "route": "/v1/reservations", "status": float64(503),
		"duration_ms": float64(2004), "trace": "v1/sessR/actR/0",
		"user": map[string]any{"email": "pat@dealer.com", "account": "dealer-1182"},
	}
	if fmt.Sprint(m) != fmt.Sprint(want) {
		t.Errorf("message = %v\nwant      %v", m, want)
	}
}

// The exception is the report: a 5xx after CaptureException, or a panic
// answered 500 by Recover, produces nothing more. A slow 2xx after a
// captured exception is still slow.
func TestRequests_AnExceptionWinsOverAFailedRequest(t *testing.T) {
	clock := useClock(t)
	s := listenSidecar(t)
	srv := readmeServer(t, signalMux(), clock)

	if st, _ := callMs(t, srv, http.MethodGet, "/captured", 10); st != http.StatusInternalServerError {
		t.Fatalf("status = %d", st)
	}
	if st, _ := callMs(t, srv, http.MethodGet, "/panics", 10); st != http.StatusInternalServerError {
		t.Fatalf("status = %d, want Recover's 500", st)
	}
	if st, _ := callMs(t, srv, http.MethodGet, "/captured-slow", 3500); st != http.StatusOK {
		t.Fatalf("status = %d", st)
	}

	lines := s.received(t)
	if got := ofKind(lines, "exception"); len(got) != 3 {
		t.Errorf("exceptions = %v, want three (captured, panic, captured-slow)", got)
	}
	reqs := ofKind(lines, "request")
	if len(reqs) != 1 {
		t.Fatalf("request messages = %v, want only the slow one", reqs)
	}
	if reqs[0]["signal"] != "slow" || reqs[0]["route"] != "/captured-slow" || num(reqs[0], "status") != 200 {
		t.Errorf("request message = %v", reqs[0])
	}
}

// report_handled changes nothing here: a 5xx after it is still failed, a
// slow request with one is still slow, and the header is unchanged.
func TestRequests_ReportHandledChangesNothing(t *testing.T) {
	clock := useClock(t)
	s := listenSidecar(t)
	srv := readmeServer(t, signalMux(), clock)

	for _, c := range []struct {
		path string
		ms   int64
		code int
	}{{"/deals/42?answer=503", 5, 503}, {"/deals/43", 4000, 200}} {
		req, _ := http.NewRequest(http.MethodPut, srv.URL+c.path, nil)
		req.Header.Set("X-Test-Ms", fmt.Sprint(c.ms))
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != c.code || res.Header.Get(HandledHeader) != "1" {
			t.Errorf("%s: %d %s=%q", c.path, res.StatusCode, HandledHeader, res.Header.Get(HandledHeader))
		}
	}

	lines := s.received(t)
	if got := ofKind(lines, "handled_failure"); len(got) != 2 {
		t.Errorf("handled failures = %v, want two", got)
	}
	reqs := ofKind(lines, "request")
	if len(reqs) != 2 {
		t.Fatalf("request messages = %v, want failed and slow", reqs)
	}
	bySignal := map[string]map[string]any{}
	for _, m := range reqs {
		bySignal[m["signal"].(string)] = m
	}
	if m := bySignal["failed"]; m == nil || m["route"] != "/deals/{id}" || num(m, "status") != 503 || m["method"] != "PUT" {
		t.Errorf("failed = %v", m)
	}
	if m := bySignal["slow"]; m == nil || m["route"] != "/deals/{id}" || num(m, "status") != 200 || num(m, "duration_ms") != 4000 {
		t.Errorf("slow = %v", m)
	}
}

// "More than 3 s" is duration_ms > 3000; streaming responses are never slow
// (but a streaming 5xx is still failed); a handler that writes nothing is a
// 200.
func TestRequests_SlowThresholdAndStreaming(t *testing.T) {
	clock := useClock(t)
	s := listenSidecar(t)
	srv := readmeServer(t, signalMux(), clock)

	callMs(t, srv, http.MethodGet, "/v1/stock/ABC-123", 3000) // exactly 3 s: not slow
	callMs(t, srv, http.MethodGet, "/v1/stock/ABC-124", 3001) // slow
	callMs(t, srv, http.MethodGet, "/quiet", 3001)            // slow, implicit 200
	callMs(t, srv, http.MethodGet, "/events", 60000)          // SSE: never slow
	callMs(t, srv, http.MethodGet, "/events?answer=503", 10)  // SSE 503: failed
	callMs(t, srv, http.MethodGet, "/nowhere/77", 3001)       // no route: "*"

	// A hijacked connection answering 101 itself (a websocket): never slow.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/socket", nil)
	req.Header.Set("X-Test-Ms", "60000")
	if res, err := srv.Client().Do(req); err == nil {
		if res.StatusCode != http.StatusSwitchingProtocols {
			t.Errorf("socket status = %d", res.StatusCode)
		}
		res.Body.Close()
	}

	lines := ofKind(s.received(t), "request")
	got := map[string]bool{}
	for _, m := range lines {
		got[fmt.Sprintf("%s %s %s %d %d", m["signal"], m["method"], m["route"], num(m, "status"), num(m, "duration_ms"))] = true
		raw, _ := json.Marshal(m)
		if strings.Contains(string(raw), "ABC-12") || strings.Contains(string(raw), "/nowhere") {
			t.Errorf("a raw path reached a report: %s", raw)
		}
	}
	want := map[string]bool{
		"slow GET /v1/stock/{sku} 200 3001": true,
		"slow GET /quiet 200 3001":          true,
		"failed GET /events 503 10":         true,
		"slow GET * 404 3001":               true,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("request signals = %v\nwant              %v", got, want)
	}
}

// Real time, no clock seam: a handler that sleeps past 3 s is slow.
func TestRequests_ARealSlowRequestIsSlow(t *testing.T) {
	s := listenSidecar(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/stock/{sku}", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3100 * time.Millisecond)
		_, _ = w.Write([]byte("ok"))
	})
	srv := readmeServer(t, mux)
	if st, _, body := call(t, srv, http.MethodGet, "/v1/stock/9", ""); st != 200 || body != "ok" {
		t.Fatalf("response = %d %q", st, body)
	}
	lines := s.received(t)
	if len(lines) != 1 || lines[0]["signal"] != "slow" || lines[0]["route"] != "/v1/stock/{sku}" ||
		num(lines[0], "duration_ms") < 3100 || num(lines[0], "duration_ms") > 10000 {
		t.Fatalf("lines = %v, want one slow request of ~3100 ms", lines)
	}
}

// The spike rule over real requests: at least ten 429s and at least 10% of
// the route's requests in one unix minute, reported once when the window
// closes (the next minute's first request, a flush, or Close), carrying
// neither trace nor user.
func TestRequests_A429SpikeIsReportedOncePerWindow(t *testing.T) {
	clock := useClock(t)
	s := listenSidecar(t)
	srv := readmeServer(t, signalMux(), clock)

	for i := 0; i < 12; i++ {
		callMs(t, srv, http.MethodGet, fmt.Sprintf("/v1/stock/S%d?answer=429", i), 1)
	}
	for i := 0; i < 8; i++ {
		callMs(t, srv, http.MethodGet, fmt.Sprintf("/v1/stock/S%d", i), 1)
	}
	// Nine 429s on another route: not a spike.
	for i := 0; i < 9; i++ {
		callMs(t, srv, http.MethodPost, "/v1/reservations", 1)
	}
	// The next minute's first request closes the window.
	clock.toNextMinute(time.Second)
	callMs(t, srv, http.MethodGet, "/quiet", 1)

	// 429s in the new minute, then a flush past it closes that window too.
	for i := 0; i < 10; i++ {
		callMs(t, srv, http.MethodGet, "/v1/stock/T?answer=429", 1)
	}
	clock.toNextMinute(0)
	if err := Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Both spikes so far have reached the sidecar by now: the second was
	// closed by the flush, not by any later request.
	var spikes []map[string]any
	deadline := time.After(5 * time.Second)
	for len(spikes) < 2 {
		select {
		case m := <-s.lines:
			if m["signal"] == "rate_limit_spike" {
				spikes = append(spikes, m)
			}
		case <-deadline:
			t.Fatalf("spikes before the exit = %v, want two (the next request's, the flush's)", spikes)
		}
	}
	// 429s that no later request or flush closes are evaluated at exit.
	for i := 0; i < 10; i++ {
		callMs(t, srv, http.MethodGet, "/v1/stock/U?answer=429", 1)
	}

	for _, m := range ofKind(s.received(t), "request") { // received closes: process exit
		if m["signal"] == "rate_limit_spike" {
			spikes = append(spikes, m)
		}
	}
	if len(spikes) != 3 {
		t.Fatalf("spikes = %v, want three (one per window)", spikes)
	}
	for i, wantTotal := range []int64{20, 10, 10} {
		m := spikes[i]
		if m["method"] != "GET" || m["route"] != "/v1/stock/{sku}" || num(m, "status") != 429 || num(m, "total") != wantTotal {
			t.Errorf("spike %d = %v, want GET /v1/stock/{sku} total %d", i, m, wantTotal)
		}
		if _, ok := m["trace"]; ok {
			t.Errorf("spike %d carries a trace: %v", i, m)
		}
		if _, ok := m["user"]; ok {
			t.Errorf("spike %d carries a user: %v", i, m)
		}
	}
	if num(spikes[0], "count") != 12 || num(spikes[1], "count") != 10 || num(spikes[2], "count") != 10 {
		t.Errorf("spike counts = %d %d %d, want 12 10 10", num(spikes[0], "count"), num(spikes[1], "count"), num(spikes[2], "count"))
	}
}

// Many goroutines finishing requests at once, flushes ticking alongside:
// every request lands in exactly one tally, and the window spikes once.
// Run under -race (CI does).
func TestRequests_SpikeDetectorIsRaceSafe(t *testing.T) {
	useClock(t) // never advanced: the whole burst falls in one window
	s := listenSidecar(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/stock/{sku}", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("answer") == "429" {
			w.WriteHeader(http.StatusTooManyRequests)
		}
	})
	srv := readmeServer(t, mux)

	const workers, each = 16, 25
	var wg sync.WaitGroup
	var n429 atomic.Int64
	stopTicks := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopTicks:
				return
			default:
				_ = Flush(context.Background())
			}
		}
	}()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				path := "/v1/stock/x"
				if (w+i)%4 == 0 {
					path += "?answer=429"
				}
				res, err := srv.Client().Get(srv.URL + path)
				if err != nil {
					t.Error(err)
					return
				}
				if res.StatusCode == 429 {
					n429.Add(1)
				}
				res.Body.Close()
			}
		}(w)
	}
	wg.Wait()
	close(stopTicks)

	var spikes []map[string]any
	for _, m := range s.received(t) {
		if m["signal"] == "rate_limit_spike" {
			spikes = append(spikes, m)
		}
	}
	if len(spikes) != 1 {
		t.Fatalf("spikes = %v, want exactly one", spikes)
	}
	if num(spikes[0], "count") != n429.Load() || num(spikes[0], "total") != workers*each {
		t.Errorf("spike = %v, want count %d total %d", spikes[0], n429.Load(), workers*each)
	}
}

// Direct mode: the flush carries the vectors' fingerprints with the right
// kind, users on failed/slow but none on a spike, and the evidence bundle
// carries the vectors' template text.
func TestRequests_DirectModeCountsTheVectorFingerprints(t *testing.T) {
	clock := useClock(t)
	f := newFakeIngest(t)
	f.askEvidence = true
	resetSDK(t)
	if err := Init(Options{DSN: f.dsn("key-direct"), Service: "inventory"}); err != nil {
		t.Fatal(err)
	}
	srv := readmeServer(t, signalMux(), clock)

	callMs(t, srv, http.MethodPost, "/v1/reservations", 2004)    // demo G2
	callMs(t, srv, http.MethodPost, "/v1/reservations", 30)      // counted, same fp
	callMs(t, srv, http.MethodGet, "/v1/stock/S1", 4120)         // slow
	callMs(t, srv, http.MethodGet, "/v1/stock/S2?answer=429", 1) // spike…
	for i := 0; i < 9; i++ {
		callMs(t, srv, http.MethodGet, "/v1/stock/S3?answer=429", 1)
	}
	clock.toNextMinute(0)
	flushNow(t) // …closed by the flush

	vec := map[string]string{}
	for _, c := range loadRequestVectors(t).Build {
		vec[c.Name] = c.ExpectedFP
	}
	failedFP, spikeFP := vec["go failed 503 (demo G2)"], vec["429 spike"]
	slowFP, _, _ := computeFingerprint(fpSignal{Kind: "degradation", Source: "server", Service: "inventory",
		Type: "slow_request", Message: "GET /v1/stock/{sku}"})

	_, flushes, puts := f.snapshot()
	type got struct {
		n     int64
		kind  string
		users int
	}
	counts := map[string]got{}
	for _, fl := range flushes {
		for _, c := range fl.Body.Counts {
			g := counts[c.FP]
			g.n += c.N
			g.kind = c.Kind
			g.users += len(c.Users)
			counts[c.FP] = g
		}
	}
	want := map[string]got{
		failedFP: {2, "error", 1},
		slowFP:   {1, "degradation", 0},
		spikeFP:  {1, "error", 0},
	}
	if fmt.Sprint(counts) != fmt.Sprint(want) {
		t.Fatalf("counts = %v\nwant     %v", counts, want)
	}

	templates := map[string]string{}
	for _, p := range puts {
		var b evidenceBundle
		if err := json.Unmarshal(p.Raw, &b); err != nil {
			t.Fatal(err)
		}
		templates[b.FP] = b.Template
		if len(b.Exemplars) != 1 || b.Exemplars[0] == "" {
			t.Errorf("bundle %s exemplars = %v", b.FP, b.Exemplars)
		}
	}
	if templates[failedFP] != "http_503 at POST /v1/reservations" || templates[slowFP] != "slow_request at GET /v1/stock/{sku}" ||
		templates[spikeFP] != "http_429_spike at GET /v1/stock/{sku}" {
		t.Errorf("templates = %v", templates)
	}
}

// panickyStatus is a framework writer whose Status() panics: the
// instrumentation must swallow it, and the app's response must stand.
type panickyStatus struct{ http.ResponseWriter }

func (panickyStatus) Status() int   { panic("framework bug") }
func (panickyStatus) Written() bool { return false }

func TestRequests_InstrumentationNeverHarmsTheHost(t *testing.T) {
	s := listenSidecar(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /x", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-App", "kept")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("body"))
	})
	h := Middleware(mux)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(panickyStatus{w}, r)
	}))
	t.Cleanup(srv.Close)

	st, hdr, body := call(t, srv, http.MethodGet, "/x", "")
	if st != http.StatusAccepted || hdr.Get("X-App") != "kept" || body != "body" {
		t.Fatalf("response = %d %v %q", st, hdr, body)
	}
	if lines := s.received(t); len(lines) != 0 {
		t.Errorf("lines = %v", lines)
	}
}

// With reporting off, Middleware observes nothing and the handler gets the
// server's own writer.
func TestRequests_OffModeDoesNotWrapTheWriter(t *testing.T) {
	resetSDK(t)
	t.Setenv(EnvEnabled, "false")
	resetConfig()
	var wrapped atomic.Bool
	srv := httptest.NewServer(Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, isOurs := w.(*statusWriter)
		wrapped.Store(isOurs)
		w.WriteHeader(http.StatusServiceUnavailable)
	})))
	t.Cleanup(srv.Close)
	call(t, srv, http.MethodGet, "/", "")
	if wrapped.Load() {
		t.Error("the writer was wrapped with reporting off")
	}
}

// A Middleware inside another (the gin adapter under an outer one) shares
// the outer request: one observation, and an exception captured inside
// still wins.
func TestRequests_NestedMiddlewareObservesOnce(t *testing.T) {
	s := listenSidecar(t)
	inner := http.NewServeMux()
	inner.HandleFunc("GET /fail", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	inner.HandleFunc("GET /captured", func(w http.ResponseWriter, r *http.Request) {
		CaptureException(r.Context(), errors.New("upstream reset"))
		w.WriteHeader(http.StatusBadGateway)
	})
	srv := httptest.NewServer(Middleware(Middleware(Handled(inner))))
	t.Cleanup(srv.Close)
	call(t, srv, http.MethodGet, "/fail", "")
	call(t, srv, http.MethodGet, "/captured", "")

	reqs := ofKind(s.received(t), "request")
	if len(reqs) != 1 || reqs[0]["route"] != "/fail" || num(reqs[0], "status") != 502 {
		t.Errorf("request messages = %v, want one failed /fail", reqs)
	}
}

// The minute-ly background flush, not only Flush or Close, closes a spike
// window the clock has left: a burst followed by silence is reported within
// one flush interval.
func TestRequests_BackgroundFlushClosesTheSpikeWindow(t *testing.T) {
	clock := useClock(t)
	old := flushInterval
	flushInterval = 50 * time.Millisecond
	t.Cleanup(func() { flushInterval = old })
	f := newFakeIngest(t)
	resetSDK(t)
	if err := Init(Options{DSN: f.dsn("key-direct"), Service: "inventory"}); err != nil {
		t.Fatal(err)
	}
	srv := readmeServer(t, signalMux(), clock)
	for i := 0; i < 10; i++ {
		callMs(t, srv, http.MethodGet, "/v1/stock/S?answer=429", 1)
	}
	clock.toNextMinute(0) // silence: no request, no Flush, no Close

	var spikeFP string
	for _, c := range loadRequestVectors(t).Build {
		if c.Name == "429 spike" {
			spikeFP = c.ExpectedFP
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for f.counts()[spikeFP] != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("no spike reached ingest from the background flush: %v", f.counts())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
