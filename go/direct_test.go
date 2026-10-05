package devbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The vector "go-error-with-frames" in testdata/fingerprint_vectors.json is
// exactly a Go exception as the sidecar fingerprints it. Reporting the same
// thing in direct mode must produce the same hash, or moving a service off
// the sidecar would fork every issue it has.
const goVectorFP = "928369bb9094e4e29255bd2f58c390e9d471e5e76aa161906ad1e9b1e88c6912"

func goVectorException() message {
	return message{
		V: 1, Kind: "exception", Context: "request", Handled: new(bool),
		Error:   "*errors.errorString",
		Message: "failed to persist invoice 40912",
		Symbol:  "PUT /invoices/{id}",
		Frames: []Frame{
			{Function: "billing.(*Invoicer).Persist", File: "/home/dev/src/acme/services/billing/invoice.go", Line: 214},
		},
	}
}

func TestDirect_Phase1BodyCarriesTheSidecarsFingerprints(t *testing.T) {
	f := newFakeIngest(t)
	resetSDK(t)
	if err := Init(Options{DSN: f.dsn("key-abc"), Service: "go-billing", Release: "r42"}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	send(goVectorException())
	ctx := WithUser(context.Background(), User{Email: "  Pat@Example.com ", Account: "acct-1182"})
	ReportHandled(ctx, errors.New("duplicate key 9912"), "billing.Invoicer.Persist")
	flushNow(t)

	_, flushes, _ := f.snapshot()
	if len(flushes) != 1 {
		t.Fatalf("got %d flushes, want 1", len(flushes))
	}
	fl := flushes[0]
	if fl.Key != "key-abc" {
		t.Errorf("X-ADT-Key = %q, want the key from the DSN", fl.Key)
	}
	if fl.ContentType != "application/json" {
		t.Errorf("content-type = %q", fl.ContentType)
	}
	b := fl.Body
	if b.V != 1 || b.Source != "server" || b.Service != "go-billing" || b.Release != "r42" {
		t.Errorf("envelope = v%d source=%q service=%q release=%q", b.V, b.Source, b.Service, b.Release)
	}
	if b.SensorID == "" || b.SensorID != sensorID {
		t.Errorf("sensor_id = %q, want this process's %q", b.SensorID, sensorID)
	}

	// The handled failure: symbol and class only, as the sidecar builds it.
	handledFP, _, err := computeFingerprint(fpSignal{
		Kind: "handled_failure", Source: "server", Service: "go-billing",
		Type: "*errors.errorString", Message: "billing.Invoicer.Persist",
	})
	if err != nil {
		t.Fatal(err)
	}

	byFP := map[string]int{}
	for i, c := range b.Counts {
		byFP[c.FP] = i
	}
	ei, ok := byFP[goVectorFP]
	if !ok {
		t.Fatalf("the exception's fingerprint is not the shared vector's %s; counts: %+v", goVectorFP, b.Counts)
	}
	if c := b.Counts[ei]; c.Kind != "error" || c.N != 1 || c.First <= 0 || c.Last < c.First || len(c.Users) != 0 {
		t.Errorf("exception count = %+v", c)
	}
	hi, ok := byFP[handledFP]
	if !ok {
		t.Fatalf("no count for the handled failure's fingerprint %s; counts: %+v", handledFP, b.Counts)
	}
	hc := b.Counts[hi]
	if hc.Kind != "handled_failure" || hc.N != 1 {
		t.Errorf("handled count = %+v", hc)
	}
	if len(hc.Users) != 1 || hc.Users[0].Email != "pat@example.com" || hc.Users[0].Account != "acct-1182" {
		t.Errorf("users = %+v, want the normalized identity", hc.Users)
	}
}

func TestDirect_RepeatsAreCountedNotSent(t *testing.T) {
	f := newFakeIngest(t)
	useDirect(t, f)

	for i := 0; i < 250; i++ {
		ReportHandled(context.Background(), fmt.Errorf("conflict on row %d", i), "hot.Loop")
	}
	flushNow(t)

	_, flushes, _ := f.snapshot()
	if len(flushes) != 1 || len(flushes[0].Body.Counts) != 1 {
		t.Fatalf("flushes = %+v, want one flush with one count", flushes)
	}
	if n := flushes[0].Body.Counts[0].N; n != 250 {
		t.Errorf("n = %d, want 250", n)
	}

	// The window is reset: the next flush carries only what happened since.
	ReportHandled(context.Background(), errors.New("again"), "hot.Loop")
	flushNow(t)
	_, flushes, _ = f.snapshot()
	if len(flushes) != 2 || flushes[1].Body.Counts[0].N != 1 {
		t.Fatalf("second window = %+v, want n=1", flushes)
	}
}

func TestDirect_UsersAreDistinctAndCapped(t *testing.T) {
	f := newFakeIngest(t)
	useDirect(t, f)

	err := errors.New("boom")
	for i := 0; i < 25; i++ {
		ctx := WithUser(context.Background(), User{Email: fmt.Sprintf("u%d@x.com", i)})
		ReportHandled(ctx, err, "users.Cap")
		ReportHandled(ctx, err, "users.Cap") // the same user twice is one identity
	}
	ReportHandled(context.Background(), err, "users.Cap") // nobody named
	flushNow(t)

	_, flushes, _ := f.snapshot()
	c := flushes[0].Body.Counts[0]
	if c.N != 51 {
		t.Errorf("n = %d, want 51", c.N)
	}
	// Past the cap the list cannot tell a new user from a repeat, so every
	// further occurrence counts (5 users x 2) — exactly as the sidecar does.
	if len(c.Users) != 20 || c.UsersOverflow != 10 {
		t.Errorf("users = %d, overflow = %d; want 20 and 10", len(c.Users), c.UsersOverflow)
	}

	ReportHandled(context.Background(), err, "users.Anon")
	flushNow(t)
	_, flushes, _ = f.snapshot()
	if got := flushes[1].Body.Counts[0]; got.Users != nil || got.UsersOverflow != 0 {
		t.Errorf("a count with nobody named carried users %+v / %d", got.Users, got.UsersOverflow)
	}
}

func TestDirect_WindowIsBoundedAndOverflowIsCounted(t *testing.T) {
	f := newFakeIngest(t)
	useDirect(t, f)

	for i := 0; i < maxWindowFingerprints+88; i++ {
		ReportHandled(context.Background(), errors.New("x"), siteName(i))
	}
	flushNow(t)

	_, flushes, _ := f.snapshot()
	total, overflowed := 0, 0
	for _, fl := range flushes {
		total += len(fl.Body.Counts)
		overflowed += fl.Body.Overflowed
	}
	if total != maxWindowFingerprints || overflowed != 88 {
		t.Errorf("counts = %d, overflowed = %d; want %d and 88", total, overflowed, maxWindowFingerprints)
	}
}

// Counts carrying 20 identities each are big: 512 of them are ~1 MB, and
// ingest refuses an oversized body whole. Split, every count once.
func TestDirect_LargeWindowsAreSplitUnderTheBodyLimit(t *testing.T) {
	f := newFakeIngest(t)
	useDirect(t, f)

	long := strings.Repeat("a", 60)
	err := errors.New("x")
	for i := 0; i < maxWindowFingerprints; i++ {
		site := siteName(i)
		for u := 0; u < maxUsersPerCount; u++ {
			ctx := WithUser(context.Background(), User{Email: fmt.Sprintf("%s%d@example.com", long, u), Account: fmt.Sprintf("acct-%s-%d", long, u)})
			ReportHandled(ctx, err, site)
		}
		// 10,240 reports in a tight loop would overrun the bounded queue
		// (by design: drops, not waits). Pace the producer instead.
		for d := curDirect.Load(); d != nil && len(d.queue) > directQueueSize/2; {
			time.Sleep(time.Millisecond)
		}
	}
	flushNow(t)

	_, flushes, _ := f.snapshot()
	if len(flushes) < 2 {
		t.Fatalf("one request carried everything (%d flushes); it would exceed the limit", len(flushes))
	}
	seen := map[string]int{}
	for _, fl := range flushes {
		if fl.Size >= maxFlushBodyBytes {
			t.Errorf("a request was %d bytes, limit %d", fl.Size, maxFlushBodyBytes)
		}
		for _, c := range fl.Body.Counts {
			seen[c.FP]++
			if c.N != maxUsersPerCount || len(c.Users) != maxUsersPerCount {
				t.Errorf("count %s: n=%d users=%d", c.FP[:8], c.N, len(c.Users))
			}
		}
	}
	if len(seen) != maxWindowFingerprints {
		t.Errorf("%d distinct counts arrived, want %d", len(seen), maxWindowFingerprints)
	}
	for fp, n := range seen {
		if n != 1 {
			t.Errorf("%s arrived %d times", fp[:8], n)
		}
	}
}

func TestDirect_EmptyWindowSendsNothing(t *testing.T) {
	f := newFakeIngest(t)
	useDirect(t, f)

	flushNow(t)
	ReportHandled(context.Background(), errors.New("x"), "once")
	flushNow(t)
	flushNow(t)

	if attempts, _, _ := f.snapshot(); attempts != 1 {
		t.Errorf("%d requests, want 1: an empty window must send nothing", attempts)
	}
}

// Evidence: only when asked, as the sidecar's bundle, redacted in the SDK.
func TestDirect_EvidenceIsUploadedWhenAskedAndScrubbed(t *testing.T) {
	f := newFakeIngest(t)
	f.askEvidence = true
	useDirect(t, f)

	m := message{
		V: 1, Kind: "exception", Context: "explicit", Handled: &handledTrue,
		Error:   "*billing.ChargeError",
		Message: `charge 40912 failed for pat.secret@example.com card 4111 1111 1111 1111 "Alice Smith" from 203.0.113.42` + "\nsecond line",
		Symbol:  "POST /charges",
		Frames:  []Frame{{Function: "acme/billing.Charge", File: "billing/charge.go", Line: 88}},
		User:    &User{Email: "lee@example.com", Account: "acct-7"},
	}
	send(m)
	flushNow(t)

	_, flushes, puts := f.snapshot()
	if len(puts) != 1 {
		t.Fatalf("got %d evidence uploads, want 1 (flushes: %d)", len(puts), len(flushes))
	}
	p := puts[0]
	if p.Key != "" {
		t.Error("the ingest key was sent to the storage URL")
	}
	if p.ContentType != "application/json" {
		t.Errorf("content-type = %q", p.ContentType)
	}

	var bundle map[string]any
	if err := json.Unmarshal(p.Raw, &bundle); err != nil {
		t.Fatalf("bundle: %v\n%s", err, p.Raw)
	}
	fp := flushes[0].Body.Counts[0].FP
	if bundle["v"] != float64(1) || bundle["fp"] != fp || bundle["kind"] != "error" || bundle["service"] != "billing" {
		t.Errorf("bundle header = %v", bundle)
	}
	if got := bundle["template"]; got != "*billing.ChargeError at POST /charges" {
		t.Errorf("template = %q", got)
	}
	ex, _ := bundle["exemplars"].([]any)
	if len(ex) != 1 {
		t.Fatalf("exemplars = %v", bundle["exemplars"])
	}
	want := "*billing.ChargeError: charge <num> failed for <email> card <num> <num> <num> <num> <str> from <ip> second line [explicit, handled]"
	if ex[0] != want {
		t.Errorf("exemplar =\n  %q\nwant\n  %q", ex[0], want)
	}
	frames, _ := bundle["frames"].([]any)
	if len(frames) != 1 || frames[0].(map[string]any)["file"] != "billing/charge.go" {
		t.Errorf("frames = %v", bundle["frames"])
	}
	for _, leaked := range []string{"pat.secret", "4111", "Alice", "203.0.113", "lee@example.com", "acct-7", "40912"} {
		if strings.Contains(string(p.Raw), leaked) {
			t.Errorf("bundle leaked %q:\n%s", leaked, p.Raw)
		}
	}
	if _, has := bundle["user"]; has {
		t.Error("identity entered the bundle")
	}

	// Asked once per fingerprint; repeats do not upload again.
	send(m)
	flushNow(t)
	if _, _, puts = f.snapshot(); len(puts) != 1 {
		t.Errorf("%d uploads after a repeat, want still 1", len(puts))
	}
}

// Evidence asked for on a later flush, after the window that saw the
// fingerprint, is still answered: the first occurrence's detail outlives the
// window.
func TestDirect_EvidenceAskedLaterIsStillAnswered(t *testing.T) {
	f := newFakeIngest(t)
	useDirect(t, f)

	ReportHandled(context.Background(), errors.New("x"), "later.Site")
	flushNow(t)
	f.mu.Lock()
	f.askEvidence = true
	f.mu.Unlock()
	ReportHandled(context.Background(), errors.New("x"), "later.Site")
	flushNow(t)

	if _, _, puts := f.snapshot(); len(puts) != 1 {
		t.Fatalf("%d uploads, want 1", len(puts))
	}
}

// Close is how graceful shutdown gets the last window out.
func TestDirect_CloseCountsTheQueueAndFlushes(t *testing.T) {
	f := newFakeIngest(t)
	useDirect(t, f)

	for i := 0; i < 300; i++ {
		ReportHandled(context.Background(), errors.New("x"), "closing.Site")
	}
	closeNow(t)

	if got := f.counts(); len(got) != 1 {
		t.Fatalf("counts = %v, want one fingerprint", got)
	} else {
		for _, n := range got {
			if n != 300 {
				t.Errorf("n = %d, want all 300 reports counted by Close", n)
			}
		}
	}

	// Never a trap: reporting after Close starts again, same configuration.
	ReportHandled(context.Background(), errors.New("x"), "after.Close")
	closeNow(t)
	if len(f.counts()) != 2 {
		t.Errorf("a report after Close was lost: %v", f.counts())
	}
}

func TestDirect_CloseIsBoundedWhenIngestHangs(t *testing.T) {
	f := newFakeIngest(t)
	f.delay = time.Minute
	useDirect(t, f)

	ReportHandled(context.Background(), errors.New("x"), "hang")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := Close(ctx)
	if err == nil {
		t.Fatal("Close returned nil while ingest was hanging")
	}
	if el := time.Since(start); el > time.Second {
		t.Errorf("Close took %v with a 100ms context", el)
	}

	// With no deadline of its own, the final flush is still bounded (2 s).
	ReportHandled(context.Background(), errors.New("x"), "hang")
	start = time.Now()
	_ = Close(context.Background())
	if el := time.Since(start); el > exitFlushBound+time.Second {
		t.Errorf("Close without a deadline took %v; the exit flush is bounded to %v", el, exitFlushBound)
	}
}

func TestDirect_OneRetryThenSuccess(t *testing.T) {
	shortJitter(t)
	f := newFakeIngest(t)
	f.respond = func(n int) int {
		if n == 1 {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	}
	useDirect(t, f)

	ReportHandled(context.Background(), errors.New("x"), "retry.Site")
	flushNow(t)

	attempts, flushes, _ := f.snapshot()
	if attempts != 2 || len(flushes) != 1 || flushes[0].Body.Counts[0].N != 1 {
		t.Errorf("attempts = %d, accepted = %d; want 2 attempts and the count delivered once", attempts, len(flushes))
	}
}

func TestDirect_OneRetryThenDiscard(t *testing.T) {
	shortJitter(t)
	f := newFakeIngest(t)
	var failing atomic.Bool
	failing.Store(true)
	f.respond = func(int) int {
		if failing.Load() {
			return http.StatusBadGateway
		}
		return http.StatusOK
	}
	useDirect(t, f)

	before := Stats()
	ReportHandled(context.Background(), errors.New("x"), "lost.Site")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Flush(ctx); err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("Flush = %v, want the 502 reported", err)
	}
	if attempts, _, _ := f.snapshot(); attempts != 2 {
		t.Errorf("attempts = %d, want exactly 2 (one retry, then discard)", attempts)
	}
	if Stats().Failed-before.Failed != 1 {
		t.Errorf("failed reports = %d, want 1", Stats().Failed-before.Failed)
	}

	// Discarded, not buffered: the next window carries only new reports.
	failing.Store(false)
	ReportHandled(context.Background(), errors.New("x"), "next.Site")
	flushNow(t)
	_, flushes, _ := f.snapshot()
	if len(flushes) != 1 || len(flushes[0].Body.Counts) != 1 {
		t.Fatalf("after recovery: %+v, want one count", flushes)
	}
}

// A rejected key is not retried: a second identical request cannot succeed.
func TestDirect_RejectedKeyIsNotRetried(t *testing.T) {
	shortJitter(t)
	f := newFakeIngest(t)
	f.respond = func(int) int { return http.StatusUnauthorized }
	useDirect(t, f)

	ReportHandled(context.Background(), errors.New("x"), "x")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := Flush(ctx)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("Flush = %v, want the 401 reported", err)
	}
	if attempts, _, _ := f.snapshot(); attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
}

// The background flusher sends without anyone calling Flush.
func TestDirect_BackgroundFlusherSends(t *testing.T) {
	old := flushInterval
	flushInterval = 50 * time.Millisecond
	t.Cleanup(func() { flushInterval = old })

	f := newFakeIngest(t)
	useDirect(t, f)
	ReportHandled(context.Background(), errors.New("x"), "ticker.Site")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(f.counts()) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the background flusher never sent the window")
}

// The queue is bounded: when counting falls behind, reports are dropped and
// counted, never buffered without limit, and the drop is reported to ingest.
func TestDirect_FullQueueDropsAndCounts(t *testing.T) {
	f := newFakeIngest(t)
	useDirect(t, f)

	ReportHandled(context.Background(), errors.New("x"), "warm")
	d := curDirect.Load()
	if d == nil {
		t.Fatal("no direct client")
	}
	flushNow(t)

	// Hold the counter's lock so it stalls on its next report.
	d.mu.Lock()
	before := Stats().Dropped
	start := time.Now()
	for i := 0; i < 3*directQueueSize; i++ {
		ReportHandled(context.Background(), errors.New("x"), "burst")
	}
	el := time.Since(start)
	d.mu.Unlock()

	dropped := Stats().Dropped - before
	if dropped == 0 {
		t.Fatalf("no drops after %d reports into a stalled queue of %d", 3*directQueueSize, directQueueSize)
	}
	if el > time.Second {
		t.Errorf("reporting into a full queue took %v; the caller must never wait", el)
	}

	flushNow(t)
	_, flushes, _ := f.snapshot()
	last := flushes[len(flushes)-1].Body
	if uint64(last.Dropped) != dropped {
		t.Errorf("flush reported dropped=%d, SDK dropped %d", last.Dropped, dropped)
	}
}

// Heavy concurrency against each ingest state, under -race: nothing races
// and no caller waits on the network.
func TestDirect_ConcurrentReportsNeverBlock(t *testing.T) {
	for _, state := range []string{"dead", "slow", "present"} {
		t.Run(state, func(t *testing.T) {
			f := newFakeIngest(t)
			var dsn string
			switch state {
			case "dead":
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				addr := ln.Addr().String()
				_ = ln.Close() // nothing listens there now
				dsn = "http://key@" + addr
			case "slow":
				f.delay = 3 * time.Second
				dsn = f.dsn("key")
			default:
				dsn = f.dsn("key")
			}

			old := flushInterval
			flushInterval = 20 * time.Millisecond // flushes in flight while reports arrive
			t.Cleanup(func() { flushInterval = old })
			resetSDK(t)
			if err := Init(Options{DSN: dsn, Service: "billing"}); err != nil {
				t.Fatal(err)
			}

			worst := concurrentSends(t, 1000, 5, func(g, i int) {
				ctx := WithUser(context.Background(), User{Email: fmt.Sprintf("u%d@x.com", g%30)})
				ReportHandled(ctx, errors.New("x"), fmt.Sprintf("g%d", g%40))
				CaptureException(ctx, fmt.Errorf("e%d", i))
			})
			// Each call is two reports plus a stack capture, from 1000
			// goroutines under -race: scheduling alone reaches ~60ms on a
			// small container. Waiting on this ingest costs seconds (3 s
			// delay; a dead host's 5 s timeout), so 250ms still separates
			// "never blocks" from "blocks".
			if worst > 250*time.Millisecond {
				t.Errorf("slowest report took %v with ingest %s", worst, state)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = Close(ctx)
			if state == "present" && len(f.counts()) == 0 {
				t.Error("nothing reached a present ingest")
			}
		})
	}
}

// With a DSN, nothing is written to the sidecar's socket: never both.
func TestDirect_NeverAlsoWritesToTheSidecar(t *testing.T) {
	path := socketPath(t)
	s := startSidecarAt(t, path)
	f := newFakeIngest(t)
	useDirect(t, f)
	t.Setenv(SocketEnv, path)

	ReportHandled(context.Background(), errors.New("x"), "one.Way")
	closeNow(t)

	if len(f.counts()) != 1 {
		t.Fatalf("direct mode did not deliver: %v", f.counts())
	}
	s.expectNone(t, 200*time.Millisecond)
}

// The self-test checks the key and records nothing: a synthetic exception
// would become a real issue on a new customer's punch list.
func TestTest_ChecksTheKeyAndRecordsNothing(t *testing.T) {
	f := newFakeIngest(t)
	f.askEvidence = true
	useDirect(t, f)

	if err := Test(context.Background()); err != nil {
		t.Fatalf("Test = %v", err)
	}
	_, flushes, puts := f.snapshot()
	f.mu.Lock()
	checks := append([]string(nil), f.checks...)
	f.mu.Unlock()
	if len(checks) != 1 || checks[0] == "" {
		t.Fatalf("checks = %v, want one with the key", checks)
	}
	if len(flushes) != 0 || len(puts) != 0 {
		t.Errorf("a self-test created %d flush(es) and %d upload(s); it must create nothing", len(flushes), len(puts))
	}
}

func TestTest_ReportsWhatIsWrong(t *testing.T) {
	t.Run("no DSN", func(t *testing.T) {
		resetSDK(t)
		err := Test(context.Background())
		if err == nil || !strings.Contains(err.Error(), EnvDSN) {
			t.Errorf("Test = %v, want it to name %s", err, EnvDSN)
		}
	})
	t.Run("rejected key", func(t *testing.T) {
		f := newFakeIngest(t)
		f.respond = func(int) int { return http.StatusUnauthorized }
		useDirect(t, f)
		err := Test(context.Background())
		if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "key") {
			t.Errorf("Test = %v, want a rejected-key error with the status", err)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		shortJitter(t)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		resetSDK(t)
		if err := Init(Options{DSN: "http://secret-key-123@" + addr}); err != nil {
			t.Fatal(err)
		}
		err = Test(context.Background())
		if err == nil || !strings.Contains(err.Error(), "unreachable") {
			t.Errorf("Test = %v, want unreachable", err)
		}
		if err != nil && strings.Contains(err.Error(), "secret-key-123") {
			t.Errorf("the error quotes the key: %v", err)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		resetSDK(t)
		t.Setenv(EnvEnabled, "false")
		t.Setenv(EnvDSN, "https://k@example.com")
		if err := Test(context.Background()); err == nil || !strings.Contains(err.Error(), "disabled") {
			t.Errorf("Test = %v, want disabled", err)
		}
	})
}

// A panicking handler under Middleware with a user, end to end over HTTP,
// arrives in direct mode with the route as its symbol in evidence.
func TestDirect_MiddlewarePanicIsReportedDirectly(t *testing.T) {
	f := newFakeIngest(t)
	f.askEvidence = true
	useDirect(t, f)

	mux := http.NewServeMux()
	mux.HandleFunc("/deals/", func(w http.ResponseWriter, r *http.Request) {
		WithUser(r.Context(), User{Email: "Lee@Example.com"})
		panic(errors.New("deal 9912 vanished"))
	})
	srv := httptest.NewServer(Middleware(mux))
	srv.Config.ErrorLog = discardLogger()
	t.Cleanup(srv.Close)

	res, err := http.Get(srv.URL + "/deals/9912")
	if err == nil {
		res.Body.Close()
	}
	closeNow(t)

	_, flushes, puts := f.snapshot()
	if len(flushes) != 1 || len(flushes[0].Body.Counts) != 1 {
		t.Fatalf("flushes = %+v", flushes)
	}
	c := flushes[0].Body.Counts[0]
	if c.Kind != "error" || len(c.Users) != 1 || c.Users[0].Email != "lee@example.com" {
		t.Errorf("count = %+v", c)
	}
	if len(puts) != 1 || !strings.Contains(evidenceText(puts[0].Raw), `deal <num> vanished [request]`) {
		t.Errorf("evidence = %d uploads %q", len(puts), puts)
	}
}

// siteName is a distinct symbol per i made of letters only: digits would be
// templated into <num> and every site would share one fingerprint.
func siteName(i int) string {
	s := ""
	for {
		s = string(rune('a'+i%26)) + s
		i /= 26
		if i == 0 {
			return "site." + s
		}
	}
}

// evidenceText is a bundle as text, with JSON's HTML escaping undone.
func evidenceText(raw []byte) string {
	return strings.NewReplacer("\\u003c", "<", "\\u003e", ">", "\\u0026", "&").Replace(string(raw))
}

func shortJitter(t *testing.T) {
	t.Helper()
	oldMin, oldMax := retryJitterMin, retryJitterMax
	retryJitterMin, retryJitterMax = 5*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { retryJitterMin, retryJitterMax = oldMin, oldMax })
}
