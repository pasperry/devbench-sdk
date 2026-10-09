package devbench

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// The control-socket reporter: the sidecar transport, used when no DSN is
// configured (SERVER_SDK_SPEC "Control socket — v1"; direct.go is the other).
//
// In this mode the SDK sends nothing over the network. It writes
// line-delimited JSON to the sidecar's unix socket and the sidecar does the
// rest — aggregation, fingerprinting, scrubbing and egress.
//
// Everything here runs on, or is fed from, the customer's request path, so the
// rules are absolute:
//
//   - the caller never blocks: messages go into a bounded buffered channel and
//     are dropped (and counted) when it is full;
//   - one background goroutine, started on the first message, does all I/O;
//   - every dial and write has a short deadline, and every failure is
//     swallowed — a missing or wedged sidecar costs reports, never latency.
//
// Apps do not need to call anything to start or stop it. Close exists for
// graceful shutdown and for tests.

// DefaultSocket is where the sidecar listens unless ADT_SIDECAR_SOCKET says
// otherwise. The same default as the Ruby gem.
const DefaultSocket = "/tmp/adt-sidecar.sock"

// SocketEnv names the environment variable that overrides DefaultSocket.
const SocketEnv = "ADT_SIDECAR_SOCKET"

const (
	// queueSize bounds memory: at most this many reports wait for the
	// writer. A burst beyond it is dropped rather than buffered.
	queueSize = 1024

	// batchMax bounds how many queued reports share one connection.
	batchMax = 256

	// dialTimeout and writeTimeout (below) bound how long a wedged sidecar
	// can hold the writer. They never touch the caller, but a short bound
	// keeps the queue moving so a recovered sidecar sees fresh reports
	// quickly.
	dialTimeout = 100 * time.Millisecond

	// After a failed dial the writer waits before trying again, doubling up
	// to backoffMax, so an absent sidecar does not become a busy loop.
	backoffMin = 10 * time.Millisecond
	backoffMax = time.Second

	// maxLine is the sidecar's per-line limit; a larger line would be
	// rejected there, so it is not sent at all.
	maxLine = 256 << 10

	// maxWrite bounds the bytes written under one deadline. A batch of 256
	// large reports is megabytes; written whole against one 250ms deadline,
	// a sidecar that is merely slow to read times the write out partway,
	// truncating one line and losing the rest of the batch. Chunked, each
	// piece gets its own deadline, so a slow reader costs the background
	// writer time rather than costing reports — while a sidecar that has
	// stopped reading still fails within one deadline. Small enough that a
	// reader managing even ~150 KB/s keeps up.
	maxWrite = 32 << 10
)

// writeTimeout is the deadline for each maxWrite bytes written. A variable
// only so tests can widen it: with a deadline of seconds, a caller that
// waited on a wedged sidecar takes seconds, which no amount of scheduler
// noise imitates. Never changed outside tests, and only while no writer runs.
var writeTimeout = 250 * time.Millisecond

// message is one control-socket line, before encoding.
//
// Encoding happens on the writer goroutine, not the caller's: the caller pays
// for copying a struct into the channel and nothing else.
type message struct {
	V       int     `json:"v"`
	Kind    string  `json:"kind"`
	Context string  `json:"context,omitempty"`
	Handled *bool   `json:"handled,omitempty"`
	Error   string  `json:"error,omitempty"`
	Message string  `json:"message,omitempty"`
	Symbol  string  `json:"symbol,omitempty"`
	Reason  string  `json:"reason,omitempty"`
	Frames  []Frame `json:"frames,omitempty"`
	Trace   string  `json:"trace,omitempty"`
	User    *User   `json:"user,omitempty"`
}

// ReporterStats counts what happened to reports since the process started.
type ReporterStats struct {
	// Sent reports were written to the sidecar's socket.
	Sent uint64
	// Dropped reports never left the process because the queue was full or
	// the reporter was closing.
	Dropped uint64
	// Failed reports were lost to a dial or write error: no sidecar, a
	// wedged sidecar, or a line too large to send.
	Failed uint64
}

// counters is one set of report counts.
type counters struct {
	sent, dropped, failed atomic.Uint64
}

// stats is the process-wide set behind Stats(), shared by both transports.
var stats counters

// Stats returns the reporter's counters. Cheap; safe to call from anywhere.
func Stats() ReporterStats {
	return ReporterStats{
		Sent:    stats.sent.Load(),
		Dropped: stats.dropped.Load(),
		Failed:  stats.failed.Load(),
	}
}

// reporter owns one socket path, one queue and one writer goroutine.
type reporter struct {
	path    string
	queue   chan message
	start   sync.Once
	stop    chan struct{} // closed by Close: drain and exit
	stopped chan struct{} // closed by the writer on exit
	closing sync.Once

	// own counts this reporter's reports alone (each is also counted in
	// stats), so a caller can tell what happened to its own reports when
	// other reporters and the direct transport share the process.
	own counters
}

// The reporter's count helpers: each count goes to the reporter and to the
// process-wide Stats().
func (r *reporter) sent(n uint64)    { r.own.sent.Add(n); stats.sent.Add(n) }
func (r *reporter) dropped(n uint64) { r.own.dropped.Add(n); stats.dropped.Add(n) }
func (r *reporter) failed(n uint64)  { r.own.failed.Add(n); stats.failed.Add(n) }

// counts returns this reporter's own counters.
func (r *reporter) counts() ReporterStats {
	return ReporterStats{Sent: r.own.sent.Load(), Dropped: r.own.dropped.Load(), Failed: r.own.failed.Load()}
}

// current is the process's reporter. Close swaps it out, so the next report
// lazily creates a fresh one (and re-reads ADT_SIDECAR_SOCKET).
var current atomic.Pointer[reporter]

func newReporter() *reporter {
	path := os.Getenv(SocketEnv)
	if path == "" {
		path = DefaultSocket
	}
	return &reporter{
		path:    path,
		queue:   make(chan message, queueSize),
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
}

func getReporter() *reporter {
	for {
		if r := current.Load(); r != nil {
			return r
		}
		r := newReporter()
		if current.CompareAndSwap(nil, r) {
			return r
		}
	}
}

// send hands m to the active transport: direct to ingest when a DSN is
// configured, else the sidecar's socket — never both, or every report would
// be counted twice. Never blocks, never panics.
func send(m message) {
	defer func() { _ = recover() }()

	switch c := activeConfig(); c.mode {
	case modeOff:
		return
	case modeDirect:
		getDirect(c).enqueue(m)
		return
	}
	sendSocket(m)
}

// sendSocket queues m for the sidecar.
func sendSocket(m message) {
	r := getReporter()
	r.start.Do(func() { go r.run() })

	select {
	case <-r.stop:
		r.dropped(1)
		return
	default:
	}

	select {
	case r.queue <- m:
	default:
		r.dropped(1)
	}
}

// closeSocket drains queued reports to the sidecar and stops the writer,
// waiting until it has finished or ctx is done, whichever is first. A report
// made after it starts a new writer.
func closeSocket(ctx context.Context) error {
	r := current.Swap(nil)
	if r == nil {
		return nil
	}
	r.closing.Do(func() { close(r.stop) })

	// A reporter whose writer never started has nothing to wait for. Marking
	// start done here also stops a racing send from starting one now.
	r.start.Do(func() { close(r.stopped) })

	// Checked alone first: select picks randomly among ready cases, and an
	// already-finished writer must not be reported as a timeout.
	select {
	case <-r.stopped:
		return nil
	default:
	}

	select {
	case <-r.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run is the single writer. It takes one report, gathers whatever else is
// already queued, writes them on one connection, and closes it — the spec's
// "connect, write, close", amortised over a burst.
func (r *reporter) run() {
	defer close(r.stopped)
	defer func() { _ = recover() }()

	backoff := time.Duration(0)
	batch := make([]message, 0, batchMax)

	for {
		batch = batch[:0]

		// Stopping takes priority over a non-empty queue, so shutdown goes
		// straight to drain — which sends the queue anyway, as fast as it can.
		select {
		case <-r.stop:
			r.drain(batch)
			return
		default:
		}

		select {
		case m := <-r.queue:
			batch = append(batch, m)
		case <-r.stop:
			r.drain(batch)
			return
		}
		batch = r.gather(batch)

		if r.write(batch) {
			backoff = 0
			continue
		}

		if backoff == 0 {
			backoff = backoffMin
		} else if backoff *= 2; backoff > backoffMax {
			backoff = backoffMax
		}
		select {
		case <-time.After(backoff):
		case <-r.stop:
			r.drain(batch[:0])
			return
		}
	}
}

// gather appends already-queued reports without waiting for more.
func (r *reporter) gather(batch []message) []message {
	for len(batch) < batchMax {
		select {
		case m := <-r.queue:
			batch = append(batch, m)
		default:
			return batch
		}
	}
	return batch
}

// drain sends everything still queued, once, on shutdown.
func (r *reporter) drain(batch []message) {
	for {
		batch = r.gather(batch[:0])
		if len(batch) == 0 {
			return
		}
		if !r.write(batch) {
			// The sidecar is gone; the rest would fail the same way.
			n := uint64(len(r.queue))
			r.failed(n)
			return
		}
	}
}

// write sends one batch on one connection. It reports whether the sidecar
// was reachable; individual reports that fail are counted either way.
func (r *reporter) write(batch []message) (ok bool) {
	// The writer must outlive anything odd in one batch; if it died, every
	// later report would sit in a queue nobody reads.
	defer func() {
		if recover() != nil {
			r.failed(uint64(len(batch)))
			ok = false
		}
	}()

	lines := make([][]byte, 0, len(batch))
	for i := range batch {
		line, err := json.Marshal(&batch[i])
		if err != nil || len(line)+1 > maxLine {
			r.failed(1)
			continue
		}
		lines = append(lines, append(line, '\n'))
	}
	if len(lines) == 0 {
		return true
	}

	conn, err := net.DialTimeout("unix", r.path, dialTimeout)
	if err != nil {
		r.failed(uint64(len(lines)))
		return false
	}
	defer conn.Close()

	// Whole lines per chunk, so a timeout can only ever cut the chunk being
	// written — never leave a half line followed by more lines.
	sent := 0
	for sent < len(lines) {
		end, size := sent, 0
		for end < len(lines) && (end == sent || size+len(lines[end]) <= maxWrite) {
			size += len(lines[end])
			end++
		}
		chunk := make([]byte, 0, size)
		for _, l := range lines[sent:end] {
			chunk = append(chunk, l...)
		}

		// One deadline per maxWrite bytes: a single line larger than a chunk
		// (up to maxLine) gets proportionally longer rather than failing at
		// a rate an ordinary chunk would pass.
		deadline := writeTimeout * time.Duration((size+maxWrite-1)/maxWrite)
		_ = conn.SetWriteDeadline(time.Now().Add(deadline))
		if _, err := conn.Write(chunk); err != nil {
			// A partial write may have delivered some lines of this chunk;
			// without a reply there is no telling which, so the chunk and
			// everything after it count as failed. Earlier chunks arrived.
			r.sent(uint64(sent))
			r.failed(uint64(len(lines) - sent))
			return false
		}
		sent = end
	}
	r.sent(uint64(sent))
	return true
}
