package devbench

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// In-process log capture (SERVER_SDK_SPEC "In-process log capture (direct
// mode)", DECISIONS #161).
//
// In direct mode there is no sidecar reading the application's log stream,
// so the SDK keeps recent lines itself, indexed by trace, and sends the
// lines for a trace only when triage asks for them (need_logs on a flush
// response). Two ways in:
//
//   - slog: NewLogHandler(next). Every record handled with a context that
//     carries a trace (the request context Middleware binds) is kept under
//     that trace, as one text line. The wrapped handler sees exactly what it
//     saw before.
//   - the standard log package: LogWriter(w). The log package has no context,
//     and Go has no goroutine-local storage to stand in for one, so a line is
//     keyed only by a trace id written in the line itself (v1/<s>/<i>/<hop>)
//     — the rule the sidecar uses on a log stream. Logger(ctx) returns a
//     *log.Logger that writes that id for you. A plain log.Printf with no id
//     in it is passed through and not kept.
//
// Bounds (spec): per process, the most recent 10,000 lines or 4 MiB,
// whichever is smaller; nothing older than 15 minutes; each line at most
// 4 KiB. Oldest evicted first. Capturing never blocks on I/O, never panics
// into the logging call, and only happens in direct mode: with a sidecar
// the sidecar holds the lines, and keeping a second copy would cost the
// host memory for nothing.

const (
	logMaxLines     = 10000
	logMaxBytes     = 4 << 20
	logMaxLineBytes = 4 << 10
)

// Variables so tests can shorten them.
var (
	logMaxAge = 15 * time.Minute
	logNow    = time.Now
)

type capturedLine struct {
	at   int64 // unix nanoseconds
	key  string
	text string
}

// lineRing holds captured lines oldest first. A fixed ring, so a burst of
// logging allocates nothing beyond the line itself.
type lineRing struct {
	mu    sync.Mutex
	buf   []capturedLine // len logMaxLines once the first line arrives
	head  int            // index of the oldest line
	n     int
	bytes int
}

// captured is the process's line store.
var captured lineRing

func lineCost(l capturedLine) int { return len(l.text) + len(l.key) }

// add keeps one line under key, evicting the oldest as the bounds require.
func (r *lineRing) add(key, text string, now time.Time) {
	text = truncateBytes(text, logMaxLineBytes)
	l := capturedLine{at: now.UnixNano(), key: key, text: text}
	cost := lineCost(l)

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.buf == nil {
		r.buf = make([]capturedLine, logMaxLines)
	}
	r.expire(now)
	for r.n > 0 && (r.n >= len(r.buf) || r.bytes+cost > logMaxBytes) {
		r.popOldest()
	}
	r.buf[(r.head+r.n)%len(r.buf)] = l
	r.n++
	r.bytes += cost
}

// expire drops lines older than logMaxAge. Caller holds mu.
func (r *lineRing) expire(now time.Time) {
	cutoff := now.Add(-logMaxAge).UnixNano()
	for r.n > 0 && r.buf[r.head].at < cutoff {
		r.popOldest()
	}
}

func (r *lineRing) popOldest() {
	r.bytes -= lineCost(r.buf[r.head])
	r.buf[r.head] = capturedLine{} // release the strings
	r.head = (r.head + 1) % len(r.buf)
	r.n--
}

// lookup returns the lines held for key, oldest first.
func (r *lineRing) lookup(key string, now time.Time) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == 0 {
		return nil
	}
	r.expire(now)
	var out []string
	for i := 0; i < r.n; i++ {
		l := r.buf[(r.head+i)%len(r.buf)]
		if l.key == key {
			out = append(out, l.text)
		}
	}
	return out
}

// holding reports whether any line younger than logMaxAge is held — whether
// this process should poll for log requests.
func (r *lineRing) holding(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == 0 {
		return false
	}
	r.expire(now)
	return r.n > 0
}

// stats reports what is held, for tests.
func (r *lineRing) stats() (lines, bytes int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n, r.bytes
}

func (r *lineRing) reset() {
	r.mu.Lock()
	r.buf, r.head, r.n, r.bytes = nil, 0, 0, 0
	r.mu.Unlock()
}

// capturing reports whether lines should be kept: direct mode only.
//
// Reads the configuration without resolving it. Resolution takes a lock and
// may itself log (an invalid DSN), and that log line may come straight back
// here through LogWriter; a log call must never wait on, or re-enter, that.
// The constructors (NewLogHandler, LogWriter) resolve it up front.
func capturing() bool {
	c := cfg.Load()
	return c != nil && c.mode == modeDirect
}

// keep stores one line. Never panics.
func keep(key, text string) {
	defer func() { _ = recover() }()
	if key == "" || text == "" {
		return
	}
	captured.add(key, text, logNow())
	// Holding lines means polling for requests for them, so the background
	// flusher must run even if this process has reported nothing yet. A
	// sync.Once after the first line: an atomic load.
	if c := cfg.Load(); c != nil && c.mode == modeDirect {
		d := getDirect(c)
		d.start.Do(d.launch)
	}
}

// truncateBytes cuts s to at most n bytes without splitting a UTF-8
// sequence.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := n
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}

// TraceContextKey is the string key under which a framework adapter stores
// the request's Trace on a context type that looks values up by string —
// gin.Context, whose Value(key) reads c.Keys and, unless the engine sets
// ContextWithFallback, never consults the request's context. The gin
// adapter sets it, so slog.InfoContext(c, ...) is keyed like
// slog.InfoContext(c.Request.Context(), ...). Applications never need it.
const TraceContextKey = "github.com/pasperry/devbench-sdk/go.Trace"

// traceFrom finds the trace for a log call's context: one derived from the
// request's (r.Context()); one carrying it under TraceContextKey; or one
// that hands back the request as Value(0), as gin.Context did before 1.10.
func traceFrom(ctx context.Context) (Trace, bool) {
	if ctx == nil {
		return Trace{}, false
	}
	if t, ok := FromContext(ctx); ok {
		return t, true
	}
	if t, ok := ctx.Value(TraceContextKey).(Trace); ok {
		return t, true
	}
	if r, ok := ctx.Value(0).(*http.Request); ok && r != nil {
		return FromContext(r.Context())
	}
	return Trace{}, false
}

// ---- slog ----

// LogHandler wraps a slog.Handler. For every record handled under a trace it
//
//   - adds the attribute adt_trace=v1/<session>/<intent>/<hop>, so a sidecar
//     reading the output can index the line (ARCHITECTURE_PROPOSAL.md risk
//     #9), and
//   - in direct mode, keeps a text rendering of the record in memory under
//     that trace, for triage to ask for (see the package's log capture
//     notes in logs.go).
//
// Everything the wrapped handler receives is what it received before
// in-process capture existed: same records, same attributes, same Enabled
// decisions. Records the wrapped handler is not enabled for are not kept.
//
//	slog.SetDefault(slog.New(devbench.NewLogHandler(slog.Default().Handler())))
//
// Then log with the request's context:
//
//	slog.InfoContext(r.Context(), "charging card", "deal", id)
type LogHandler struct {
	slog.Handler

	// pre is the text of attributes added with WithAttrs, already
	// rendered; group is the dotted prefix WithGroup has opened.
	pre   string
	group string
}

// NewLogHandler wraps h.
//
// h may be slog.Default().Handler() — slog's built-in handler — as in the
// install line above. That handler writes through the log package, and
// slog.SetDefault then points the log package back at the new default
// handler, so wrapped as-is every slog call would re-enter itself and
// deadlock on the log package's mutex. It is therefore replaced by an
// equivalent that writes, in the same format and at the same level, to the
// writer the log package had when NewLogHandler was called.
func NewLogHandler(h slog.Handler) *LogHandler {
	activeConfig() // so capturing() knows the mode from the first record
	return &LogHandler{Handler: standalone(h)}
}

// standalone returns h, or — for slog's built-in default handler — a
// handler that formats as it does without going through the log package.
func standalone(h slog.Handler) slog.Handler {
	if h == nil || fmt.Sprintf("%T", h) != "*slog.defaultHandler" {
		return h
	}
	return &stdLogHandler{
		level: h, // its Enabled reads slog.SetLogLoggerLevel; it never logs
		out:   log.New(log.Writer(), log.Prefix(), log.Flags()),
	}
}

// stdLogHandler writes "LEVEL message k=v ..." through its own *log.Logger,
// as slog's default handler does through the log package's.
type stdLogHandler struct {
	level slog.Handler
	out   *log.Logger
	pre   string
	group string
}

func (h *stdLogHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.level.Enabled(ctx, l)
}

func (h *stdLogHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Level.String())
	b.WriteByte(' ')
	b.WriteString(r.Message)
	b.WriteString(h.pre)
	r.Attrs(func(a slog.Attr) bool {
		appendAttr(&b, h.group, a)
		return true
	})
	return h.out.Output(0, b.String())
}

func (h *stdLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	var b strings.Builder
	for _, a := range attrs {
		appendAttr(&b, h.group, a)
	}
	return &stdLogHandler{level: h.level, out: h.out, pre: h.pre + b.String(), group: h.group}
}

func (h *stdLogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &stdLogHandler{level: h.level, out: h.out, pre: h.pre, group: h.group + name + "."}
}

// Handle captures r (direct mode, under a trace) and passes it on.
func (h *LogHandler) Handle(ctx context.Context, r slog.Record) error {
	t, ok := traceFrom(ctx)
	if !ok {
		return h.Handler.Handle(ctx, r)
	}
	if capturing() {
		h.capture(t, r)
	}
	r.AddAttrs(slog.String("adt_trace", t.String()))
	return h.Handler.Handle(ctx, r)
}

// capture renders r as one line and keeps it. A panic in rendering (a
// String method that panics, say) costs the line, never the log call.
func (h *LogHandler) capture(t Trace, r slog.Record) {
	defer func() { _ = recover() }()
	keep(t.Key(), h.render(t, r))
}

// render is the captured line:
//
//	<time> <LEVEL> <message> <k=v ...> adt_trace=v1/<s>/<i>/<hop>
//
// The message is written bare, so egress keeps its words (and scrubs what
// has a sensitive shape); attribute values are quoted when they contain
// spaces, as slog's TextHandler does, so egress treats a free-text value as
// one value. The trace goes last; egress lifts it to the front of the line,
// as the sidecar does.
func (h *LogHandler) render(t Trace, r slog.Record) string {
	var b strings.Builder
	if !r.Time.IsZero() {
		b.WriteString(r.Time.UTC().Format("2006-01-02T15:04:05.000Z07:00"))
		b.WriteByte(' ')
	}
	b.WriteString(r.Level.String())
	b.WriteByte(' ')
	b.WriteString(strings.ReplaceAll(r.Message, "\n", " "))
	b.WriteString(h.pre)
	r.Attrs(func(a slog.Attr) bool {
		appendAttr(&b, h.group, a)
		return b.Len() < logMaxLineBytes
	})
	// Bounded here rather than by add, so the trace survives a long line.
	suffix := " adt_trace=" + t.String()
	return truncateBytes(b.String(), logMaxLineBytes-len(suffix)) + suffix
}

func appendAttr(b *strings.Builder, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		attrs := a.Value.Group()
		if len(attrs) == 0 {
			return
		}
		if a.Key != "" {
			prefix += a.Key + "."
		}
		for _, ga := range attrs {
			appendAttr(b, prefix, ga)
		}
		return
	}
	b.WriteByte(' ')
	b.WriteString(prefix)
	b.WriteString(a.Key)
	b.WriteByte('=')
	var v string
	if a.Value.Kind() == slog.KindTime {
		v = a.Value.Time().UTC().Format(time.RFC3339Nano)
	} else {
		v = a.Value.String()
	}
	if needsQuoting(v) {
		v = strconv.Quote(v)
	}
	b.WriteString(v)
}

// needsQuoting is slog's TextHandler rule, simplified: empty, or anything
// other than printable non-space characters without '=' or '"'.
func needsQuoting(s string) bool {
	if s == "" {
		return true
	}
	for _, r := range s {
		if r == ' ' || r == '=' || r == '"' || r < 0x21 || r == utf8.RuneError || r == 0x7f {
			return true
		}
	}
	return false
}

// WithAttrs implements slog.Handler.
func (h *LogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := &LogHandler{Handler: h.Handler.WithAttrs(attrs), pre: h.pre, group: h.group}
	func() {
		defer func() { _ = recover() }()
		var b strings.Builder
		for _, a := range attrs {
			appendAttr(&b, h.group, a)
		}
		next.pre = h.pre + truncateBytes(b.String(), logMaxLineBytes)
	}()
	return next
}

// WithGroup implements slog.Handler.
func (h *LogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &LogHandler{Handler: h.Handler.WithGroup(name), pre: h.pre, group: h.group + name + "."}
}

// ---- the standard log package ----

// LogWriter returns an io.Writer that writes everything to w unchanged and,
// in direct mode, keeps each line that carries a trace id
// (v1/<session>/<intent>/<hop>) under that trace:
//
//	log.SetOutput(devbench.LogWriter(os.Stderr))
//
// The log package passes no context, and a goroutine has no identity Go
// lets a library read, so the trace must be in the text. Logger(ctx) writes
// it for you; lines without one are written to w and not kept. Prefer slog
// with the request's context (NewLogHandler), which needs nothing in the
// text.
//
// Each Write is treated as whole lines, which is how the log package writes.
// A nil w discards (lines are still captured).
func LogWriter(w io.Writer) io.Writer {
	activeConfig()
	if w == nil {
		w = io.Discard
	}
	return &logWriter{w: w}
}

type logWriter struct{ w io.Writer }

func (lw *logWriter) Write(p []byte) (int, error) {
	if capturing() {
		captureWritten(p)
	}
	return lw.w.Write(p)
}

var traceMarker = []byte("v1/")

// captureWritten keeps the lines of p that carry a trace id.
func captureWritten(p []byte) {
	defer func() { _ = recover() }()
	if !bytes.Contains(p, traceMarker) {
		return
	}
	for len(p) > 0 {
		var line []byte
		if i := bytes.IndexByte(p, '\n'); i >= 0 {
			line, p = p[:i], p[i+1:]
		} else {
			line, p = p, nil
		}
		line = bytes.TrimRight(line, "\r")
		if len(line) == 0 || !bytes.Contains(line, traceMarker) {
			continue
		}
		if len(line) > logMaxLineBytes {
			// The trace must be found in what is kept.
			line = []byte(truncateBytes(string(line), logMaxLineBytes))
		}
		m := egressTraceRe.FindSubmatch(line)
		if m == nil {
			continue
		}
		keep(string(m[1])+"/"+string(m[2]), string(line))
	}
}

// Logger returns a standard-library logger for ctx: it writes to the
// standard logger's output with its flags, and puts "[v1/<s>/<i>/<hop>] "
// in front of each message, so a LogWriter (and a sidecar) can key the
// line. Without a trace in ctx it has no prefix and its lines are not kept.
//
//	devbench.Logger(r.Context()).Printf("charging card for deal %s", id)
func Logger(ctx context.Context) *log.Logger {
	prefix := ""
	if t, ok := traceFrom(ctx); ok {
		prefix = "[" + t.String() + "] "
	}
	return log.New(log.Writer(), prefix, log.Flags()|log.Lmsgprefix)
}
