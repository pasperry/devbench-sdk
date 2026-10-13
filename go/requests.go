package devbench

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// How Middleware observes request signals (reqsignal.go has the rules).
//
// Once per request, after the handler has returned without panicking:
// classify it (failed / slow / nothing), feed the process's 429 spike
// detector, and hand any signal to the active transport like every other
// report — a `request` control message in sidecar mode, a count in direct
// mode. All of it on the request's goroutine but none of it blocking: a
// classification, one mutex-guarded map update, at most a channel send.

// requestClock is the clock Middleware reads at entry and at return: wall
// time for the spike window, and (through time.Time's monotonic reading)
// the duration. A variable only so tests can pin a duration to the
// millisecond; never changed outside tests, and only while no server runs.
var requestClock = time.Now

// requestSpikes is the process's 429 spike detector: per process, in both
// transports (docs/server-sdk/failed-and-slow-requests.md, "The 429 spike rule").
var requestSpikes = &spikeDetector{}

// statusWriter records the status the application answered with. It never
// changes the response: every call passes straight through, and Flush,
// Hijack, ReadFrom and Unwrap reach the writer below, so streaming,
// websocket and sendfile paths behave exactly as without Dev Bench.
type statusWriter struct {
	http.ResponseWriter
	status   int  // the first final status (or 101); 0 while none
	hijacked bool // the handler took the connection
}

func (w *statusWriter) WriteHeader(code int) {
	// 1xx informational headers (103 Early Hints) do not decide the
	// response; 101 does: the connection is now something else.
	if w.status == 0 && (code >= 200 || code == http.StatusSwitchingProtocols) {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK // net/http's implicit WriteHeader(200)
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusWriter) Flush() { _ = w.FlushError() }

// FlushError is what http.ResponseController.Flush calls. A flush before
// any WriteHeader sends the headers with 200.
func (w *statusWriter) FlushError() error {
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if err == nil && w.status == 0 {
		w.status = http.StatusOK
	}
	return err
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, brw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.hijacked = true
	}
	return conn, brw, err
}

// ReadFrom keeps net/http's sendfile path (http.ServeFile, io.Copy into
// the writer) when the writer below has one.
func (w *statusWriter) ReadFrom(r io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(writerOnly{w.ResponseWriter}, r)
}

// writerOnly hides any ReadFrom, so io.Copy cannot loop back into ours.
type writerOnly struct{ io.Writer }

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// frameworkStatus is a framework's own response writer that knows the
// status it will send (gin's, negroni's). It may differ from the first
// WriteHeader that passed through us: gin keeps a status pending until it
// writes, lets a later c.Status replace it, and commits a body-less
// response after the middleware chain has returned.
type frameworkStatus interface {
	Status() int
	Written() bool
}

// finalStatus is the status the application handed back: the framework's
// when the writer below knows it, else the first final WriteHeader, else
// 200 (net/http answers a handler that wrote nothing with 200).
func (w *statusWriter) finalStatus() int {
	if fs, ok := w.ResponseWriter.(frameworkStatus); ok {
		if s := fs.Status(); s >= 100 && s <= 599 && w.status != http.StatusSwitchingProtocols {
			return s
		}
	}
	if w.status != 0 {
		return w.status
	}
	return http.StatusOK
}

// streaming: a 101 upgrade, a hijacked connection (a websocket answers its
// 101 on the raw connection) or a text/event-stream response. Its duration
// is the connection's lifetime, so it is never slow.
func (w *statusWriter) streaming() bool {
	if w.hijacked || w.status == http.StatusSwitchingProtocols {
		return true
	}
	ct := strings.ToLower(strings.TrimSpace(w.Header().Get("Content-Type")))
	return strings.HasPrefix(ct, "text/event-stream")
}

// observeRequest classifies a finished request, reports its signal, and
// feeds the spike detector. Never panics into the application.
func observeRequest(ctx context.Context, scope *requestScope, w *statusWriter, r *http.Request, start, end time.Time) {
	defer func() { _ = recover() }()

	status := w.finalStatus()
	method := normalizeMethod(r.Method)
	route := normalizeRoute(scope.route())
	durationMs := end.Sub(start).Milliseconds()

	sig := classifyRequest(requestOutcome{
		status:            status,
		durationMs:        durationMs,
		exceptionReported: scope.exception.Load(),
		streaming:         w.streaming(),
	})
	if sig != "" {
		send(message{
			V: ProtocolVersion, Kind: "request",
			Signal: sig, Method: method, Route: route,
			Status: status, DurationMs: durationMs,
			Trace: traceField(ctx), User: userField(ctx),
		})
	}

	if status >= 100 && status <= 599 {
		sendSpikes(requestSpikes.observe(end.UnixMilli(), method, route, status))
	}
	if status == http.StatusTooManyRequests {
		startFlusher()
	}
}

// startFlusher makes sure the direct transport's flush loop runs once a 429
// is tallied: a process whose only traffic is a burst of 429s has queued no
// report, and without the loop its spike would wait for the next request or
// Close. (Sidecar mode has no flush loop: there a window closes at the next
// request or at Close.)
func startFlusher() {
	if c := activeConfig(); c.mode == modeDirect {
		d := getDirect(c)
		d.start.Do(d.launch)
	}
}

// sendSpikes reports each spike: an aggregate of many requests, so no trace
// and no user.
func sendSpikes(spikes []spike) {
	for _, s := range spikes {
		send(spikeMessage(s))
	}
}

func spikeMessage(s spike) message {
	return message{
		V: ProtocolVersion, Kind: "request", Signal: signalRateLimitSpike,
		Method: s.Method, Route: s.Route, Status: http.StatusTooManyRequests,
		Count: s.Count, Total: s.Total,
	}
}

// tickSpikes closes the spike window if the clock has left it (at a flush).
func tickSpikes() {
	defer func() { _ = recover() }()
	sendSpikes(requestSpikes.tick(requestClock().UnixMilli()))
}

// endSpikes evaluates the open window as it stands (process exit).
func endSpikes() {
	defer func() { _ = recover() }()
	sendSpikes(requestSpikes.end())
}
