package devbench

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Polling while holding lines (SERVER_SDK_SPEC "Poll while holding lines").
// need_logs only rides a flush response, so a process that holds traced
// lines and has nothing to report must still flush, empty, once per
// interval — or a request for lines only it has is never answered.

// shortInterval makes the background flusher tick every d for this test.
func shortInterval(t *testing.T, d time.Duration) {
	t.Helper()
	prev := flushInterval
	flushInterval = d
	t.Cleanup(func() { flushInterval = prev })
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(deadline time.Duration, cond func() bool) bool {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// A process that only logged — no panic, no ReportHandled, so no counts
// ever — answers a slice request within one interval. The stand-in hands
// out need_logs only on an empty flush, so only the poll can get it.
func TestPoll_AQuietProcessAnswersWithinOneInterval(t *testing.T) {
	const every = 100 * time.Millisecond
	shortInterval(t, every)
	f := newFakeIngest(t)
	f.mu.Lock()
	f.logsOnlyOnPoll = true
	f.mu.Unlock()
	useLogs(t, f)

	stdLog().Printf("[v1/sessQuiet/act1/0] charging card for deal 9912")
	f.askLogs("sessQuiet/act1")
	start := time.Now()

	if !waitFor(5*time.Second, func() bool {
		got, _ := f.delivered()
		return len(got["sessQuiet/act1"]) == 1
	}) {
		_, flushes, _ := f.snapshot()
		t.Fatalf("a quiet process never answered (flushes seen: %d)", len(flushes))
	}
	// One interval to the first tick, plus generous scheduling slack.
	if took := time.Since(start); took > every+time.Second {
		t.Errorf("answered after %v; want within about one interval (%v)", took, every)
	}

	_, flushes, _ := f.snapshot()
	if len(flushes) == 0 {
		t.Fatal("answered without any flush")
	}
	for _, fl := range flushes {
		if len(fl.Body.Counts) != 0 {
			t.Errorf("the poll carried counts: %+v", fl.Body.Counts)
		}
		if fl.Key != pairSecret {
			t.Errorf("the poll authenticated with %q, want the secret", fl.Key)
		}
	}
}

// A process holding no lines never polls: an empty window sends nothing,
// however long it runs.
func TestPoll_NoLinesNoPoll(t *testing.T) {
	const every = 50 * time.Millisecond
	shortInterval(t, every)
	f := newFakeIngest(t)
	useLogs(t, f)

	// One real report starts the flusher; after it is sent, nothing more
	// is counted and no line is held.
	ReportHandled(context.Background(), errors.New("x"), "poll.Once")
	if !waitFor(5*time.Second, func() bool { a, _, _ := f.snapshot(); return a >= 1 }) {
		t.Fatal("precondition: the report was never flushed, so the flusher is not running")
	}
	time.Sleep(6 * every)

	attempts, flushes, _ := f.snapshot()
	if attempts != 1 {
		t.Errorf("%d flush requests with no lines held, want only the one report", attempts)
	}
	for _, fl := range flushes {
		if len(fl.Body.Counts) == 0 {
			t.Error("sent an empty flush while holding no lines")
		}
	}
}

// Lines that have aged out are not "holding": the process stops polling.
func TestPoll_StopsWhenLinesAgeOut(t *testing.T) {
	captured.reset()
	t.Cleanup(captured.reset)
	now := time.Now()
	captured.add("s/i", "line", now)
	if !captured.holding(now) {
		t.Fatal("a fresh line is not held")
	}
	if captured.holding(now.Add(logMaxAge + time.Second)) {
		t.Error("a line older than 15 minutes still counts as held, so polling would never stop")
	}
}
