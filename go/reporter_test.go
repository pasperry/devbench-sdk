package devbench

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReporter_WritesLineDelimitedJSONToTheSocket(t *testing.T) {
	s := listenSidecar(t)

	for i := 0; i < 3; i++ {
		send(message{V: 1, Kind: "handled_failure", Symbol: fmt.Sprintf("site.%d", i)})
	}

	// Every report arrives, once, as its own line. Not in order: the writer
	// may split a burst across connections, and nothing orders across
	// connections — the sidecar counts. Asserting order flaked on Linux.
	got := map[any]int{}
	for i := 0; i < 3; i++ {
		m := s.next(t)
		if m["v"] != float64(1) || m["kind"] != "handled_failure" {
			t.Fatalf("line %d = %v, want v=1 kind=handled_failure", i, m)
		}
		got[m["symbol"]]++
	}
	for i := 0; i < 3; i++ {
		if want := fmt.Sprintf("site.%d", i); got[want] != 1 {
			t.Errorf("%s arrived %d times, want once (got %v)", want, got[want], got)
		}
	}
}

func TestReporter_DefaultsToTheDocumentedSocketPath(t *testing.T) {
	closeReporter(t)
	t.Setenv(SocketEnv, "")
	t.Cleanup(func() { closeReporter(t) })

	if got := getReporter().path; got != "/tmp/adt-sidecar.sock" {
		t.Errorf("default socket = %q, want /tmp/adt-sidecar.sock (shared with the Ruby gem)", got)
	}
}

func TestReporter_EmptyFieldsAreOmitted(t *testing.T) {
	s := listenSidecar(t)
	send(message{V: 1, Kind: "handled_failure", Symbol: "x"})

	select {
	case line := <-s.raw:
		if line != `{"v":1,"kind":"handled_failure","symbol":"x"}` {
			t.Errorf("line = %s", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing arrived")
	}
}

// Close is how graceful shutdown (and every test) gets queued reports out.
func TestClose_FlushesEverythingQueued(t *testing.T) {
	s := listenSidecar(t)

	// Large enough that the writer is still busy when Close is called, so
	// most of these are still queued at that moment.
	const n = 500
	body := strings.Repeat("z", 16<<10)
	for i := 0; i < n; i++ {
		send(message{V: 1, Kind: "handled_failure", Symbol: "flush", Message: body})
	}
	closeReporter(t)

	for i := 0; i < n; i++ {
		if m := s.next(t); m["symbol"] != "flush" {
			t.Fatalf("message %d = %v", i, m)
		}
	}
}

// The same, deterministically: everything is still queued when shutdown
// begins, so nothing reaches the sidecar unless Close drains it.
func TestClose_DrainsAQueueTheWriterHasNotReached(t *testing.T) {
	s := listenSidecar(t)

	r := getReporter()
	for i := 0; i < 300; i++ {
		r.queue <- message{V: 1, Kind: "handled_failure", Symbol: "queued"}
	}
	r.closing.Do(func() { close(r.stop) })
	r.start.Do(func() { go r.run() })
	closeReporter(t)

	for i := 0; i < 300; i++ {
		if m := s.next(t); m["symbol"] != "queued" {
			t.Fatalf("message %d = %v", i, m)
		}
	}
}

func TestClose_WithoutAnyReportIsANoOp(t *testing.T) {
	useSocket(t, socketPath(t))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // even an expired context: there is nothing to wait for
	if err := Close(ctx); err != nil {
		t.Fatalf("Close on an idle reporter = %v, want nil", err)
	}
	_ = getReporter() // created but never started
	if err := Close(ctx); err != nil {
		t.Fatalf("Close on an unstarted reporter = %v, want nil", err)
	}
}

// A report made after Close must still work: Close is never a trap.
func TestClose_ReportsAfterCloseStartANewWriter(t *testing.T) {
	s := listenSidecar(t)
	send(message{V: 1, Kind: "handled_failure", Symbol: "before"})
	closeReporter(t)
	send(message{V: 1, Kind: "handled_failure", Symbol: "after"})

	// Both must arrive. Not in order: they travel on two connections, and
	// neither the sidecar nor anything downstream orders across connections
	// (it counts) — asserting an order here flaked on CI.
	got := map[any]bool{s.next(t)["symbol"]: true, s.next(t)["symbol"]: true}
	if !got["before"] || !got["after"] {
		t.Fatalf("received %v, want both the report before Close and the one after", got)
	}
}

// Graceful shutdown must not hang on a wedged sidecar.
func TestClose_IsBoundedByTheContext(t *testing.T) {
	useSocket(t, stalledSidecar(t))

	big := strings.Repeat("x", 100<<10)
	for i := 0; i < 200; i++ {
		send(message{V: 1, Kind: "handled_failure", Message: big})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := Close(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v, want DeadlineExceeded while the sidecar is wedged", err)
	}
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Errorf("Close took %v with a 20ms context", el)
	}
}

func TestReporter_AbsentSidecarCountsFailuresAndNeverBlocks(t *testing.T) {
	useSocket(t, socketPath(t)) // nothing listens there

	before := Stats()
	start := time.Now()
	for i := 0; i < 5000; i++ {
		send(message{V: 1, Kind: "handled_failure", Symbol: "nobody-home"})
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("5000 sends took %v with no sidecar — the caller is paying for I/O", el)
	}
	closeReporter(t)

	after := Stats()
	lost := (after.Failed - before.Failed) + (after.Dropped - before.Dropped)
	if lost == 0 {
		t.Fatalf("stats did not move: before %+v after %+v", before, after)
	}
	if after.Sent != before.Sent {
		t.Errorf("Sent moved with no sidecar: before %+v after %+v", before, after)
	}
}

// The queue is bounded: a burst the writer cannot keep up with is dropped
// and counted, never buffered without limit.
func TestReporter_FullQueueDropsAndCounts(t *testing.T) {
	useSocket(t, stalledSidecar(t))

	before := Stats()
	for i := 0; i < 3*queueSize; i++ {
		send(message{V: 1, Kind: "handled_failure", Message: strings.Repeat("y", 4096)})
	}
	if d := Stats().Dropped - before.Dropped; d == 0 {
		t.Fatalf("no drops after %d sends into a wedged sidecar with a queue of %d", 3*queueSize, queueSize)
	}
}

// The sidecar starting after the app (or restarting) must not need an app
// restart: the writer keeps trying.
func TestReporter_ReconnectsWhenTheSidecarAppears(t *testing.T) {
	path := socketPath(t)
	useSocket(t, path)

	send(message{V: 1, Kind: "handled_failure", Symbol: "lost"})
	time.Sleep(50 * time.Millisecond) // let the first dial fail

	s := startSidecarAt(t, path)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		send(message{V: 1, Kind: "handled_failure", Symbol: "found"})
		select {
		case m := <-s.lines:
			if m["symbol"] != "found" {
				t.Fatalf("got %v", m)
			}
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("the reporter never reached a sidecar that started late")
}

// Heavy concurrency against each sidecar state. Run under -race; the point is
// both that nothing races and that no caller waits on I/O.
func TestReporter_ConcurrentSendsNeverBlock(t *testing.T) {
	for _, state := range []string{"absent", "stalled", "present"} {
		t.Run(state, func(t *testing.T) {
			var s *sidecar
			switch state {
			case "absent":
				useSocket(t, socketPath(t))
			case "stalled":
				useSocket(t, stalledSidecar(t))
			case "present":
				s = listenSidecar(t)
			}

			worst := concurrentSends(t, 1000, 5, func(g, i int) {
				send(message{V: 1, Kind: "handled_failure", Symbol: fmt.Sprintf("g%d", g)})
			})
			// Generous for -race on a loaded CI box; a dial or write is
			// 100ms+ by construction, so anything doing I/O blows through.
			if worst > 50*time.Millisecond {
				t.Errorf("slowest send took %v with the sidecar %s", worst, state)
			}

			if s != nil {
				closeReporter(t)
				if m := s.next(t); m["kind"] != "handled_failure" {
					t.Errorf("present sidecar received %v", m)
				}
			}
		})
	}
}

// concurrentSends runs fn from g goroutines, n times each, all released at
// once, and returns the slowest single call.
func concurrentSends(t *testing.T, g, n int, fn func(g, i int)) time.Duration {
	t.Helper()
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		worst time.Duration
		gate  = make(chan struct{})
	)
	for j := 0; j < g; j++ {
		wg.Add(1)
		go func(j int) {
			defer wg.Done()
			<-gate
			local := time.Duration(0)
			for i := 0; i < n; i++ {
				start := time.Now()
				fn(j, i)
				if el := time.Since(start); el > local {
					local = el
				}
			}
			mu.Lock()
			if local > worst {
				worst = local
			}
			mu.Unlock()
		}(j)
	}
	close(gate)
	wg.Wait()
	return worst
}
