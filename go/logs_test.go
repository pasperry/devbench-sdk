package devbench

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// In-process log capture, exercised through the real entry points: a real
// net/http handler behind Middleware logging with slog and r.Context(), and
// the standard log package through LogWriter. The store is read back with
// the same lookup delivery uses.

const capTrace = "v1/sessCap/actCap/0"

// useCapture configures direct mode against a stand-in ingest and starts
// with an empty line store.
func useCapture(t *testing.T) *fakeIngest {
	t.Helper()
	f := newFakeIngest(t)
	useDirect(t, f)
	captured.reset()
	t.Cleanup(captured.reset)
	return f
}

// tracedRequest is a request carrying the browser's trace header.
func tracedRequest(trace string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/deals/9912", nil)
	if trace != "" {
		r.Header.Set(Header, trace)
	}
	return r
}

func held(key string) []string { return captured.lookup(key, logNow()) }

func TestCapture_SlogWithTheRequestContextIsKeptUnderItsTrace(t *testing.T) {
	useCapture(t)

	var out bytes.Buffer
	logger := slog.New(NewLogHandler(slog.NewTextHandler(&out, nil)))
	app := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.InfoContext(r.Context(), "charging card", "deal", 9912, "note", "two words")
		logger.With("svc", "billing").WithGroup("req").WarnContext(r.Context(), "slow gateway", "ms", 812)
		logger.Info("no context, so no trace")
	}))
	app.ServeHTTP(httptest.NewRecorder(), tracedRequest(capTrace))
	app.ServeHTTP(httptest.NewRecorder(), tracedRequest("")) // no trace: nothing kept

	lines := held("sessCap/actCap")
	if len(lines) != 2 {
		t.Fatalf("held %d lines, want 2: %q", len(lines), lines)
	}
	for i, want := range []string{
		`INFO charging card deal=9912 note="two words" adt_trace=v1/sessCap/actCap/0`,
		`WARN slow gateway svc=billing req.ms=812 adt_trace=v1/sessCap/actCap/0`,
	} {
		if !strings.HasSuffix(lines[i], want) {
			t.Errorf("line %d = %q, want it to end %q", i, lines[i], want)
		}
	}
	if n, _ := captured.stats(); n != 2 {
		t.Errorf("store holds %d lines, want only the 2 traced ones", n)
	}
}

// The request's trace is what keys a line. Two concurrent requests with
// different traces never share lines.
func TestCapture_LinesStayWithTheirOwnTrace(t *testing.T) {
	useCapture(t)
	logger := slog.New(NewLogHandler(slog.NewTextHandler(io.Discard, nil)))
	app := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t, _ := FromContext(r.Context())
		logger.InfoContext(r.Context(), "handling "+t.Intent)
	}))
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			app.ServeHTTP(httptest.NewRecorder(), tracedRequest(fmt.Sprintf("v1/sessK/act%d/0", i)))
		}(i)
	}
	wg.Wait()
	for i := 0; i < 20; i++ {
		got := held(fmt.Sprintf("sessK/act%d", i))
		if len(got) != 1 || !strings.Contains(got[0], fmt.Sprintf("handling act%d ", i)) {
			t.Errorf("act%d holds %q", i, got)
		}
	}
}

// With a sidecar (no DSN) the sidecar holds the lines; the SDK keeps none.
func TestCapture_OffOutsideDirectMode(t *testing.T) {
	resetSDK(t)
	captured.reset()
	t.Cleanup(captured.reset)
	if err := Init(Options{}); err != nil {
		t.Fatal(err)
	}
	if Mode() != "sidecar" {
		t.Fatalf("Mode = %s", Mode())
	}
	logger := slog.New(NewLogHandler(slog.NewTextHandler(io.Discard, nil)))
	ctx := WithTrace(context.Background(), Trace{Session: "s", Intent: "i"})
	logger.InfoContext(ctx, "kept by the sidecar, not here")
	if n, _ := captured.stats(); n != 0 {
		t.Errorf("sidecar mode kept %d lines in process", n)
	}
}

// What the wrapped handler writes must not depend on capture: the same
// calls produce the same bytes with capture on (direct) and off (sidecar).
func TestCapture_WrappedHandlerOutputIsUnchanged(t *testing.T) {
	run := func() string {
		var out bytes.Buffer
		noTime := &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		}}
		logger := slog.New(NewLogHandler(slog.NewJSONHandler(&out, noTime)))
		ctx := WithTrace(context.Background(), Trace{Session: "sessU", Intent: "actU", Hop: 1})
		logger.InfoContext(ctx, "charge declined for pat@example.com", "card", "4111 1111 1111 1111")
		logger.With("a", 1).WithGroup("g").DebugContext(ctx, "below the level")
		logger.With("a", 1).WithGroup("g").ErrorContext(ctx, "boom", "k", "v")
		logger.Info("untraced")
		return out.String()
	}

	useCapture(t)
	direct := run()
	if n, _ := captured.stats(); n != 2 {
		t.Fatalf("precondition: capture kept %d lines, want 2 (the debug record is below the wrapped handler's level)", n)
	}

	resetSDK(t)
	if err := Init(Options{}); err != nil {
		t.Fatal(err)
	}
	sidecar := run()
	if direct != sidecar {
		t.Errorf("capture changed the wrapped handler's output\ndirect:\n%s\nsidecar:\n%s", direct, sidecar)
	}
	if !strings.Contains(direct, `"adt_trace":"v1/sessU/actU/1"`) || !strings.Contains(direct, "pat@example.com") {
		t.Errorf("the application's own output lost something:\n%s", direct)
	}
}

func TestCapture_LogWriterKeepsTracedLinesAndWritesEverything(t *testing.T) {
	useCapture(t)
	var out bytes.Buffer
	w := LogWriter(&out)

	std := log.New(w, "", log.LstdFlags)
	std.Printf("[%s] charging card for deal 9912", capTrace)
	std.Printf("no trace in this one")
	log.New(w, "", 0).Printf("[%s] from Logger-style prefix", "v1/sessCap/actCap/3")

	// The bytes written through are exactly the bytes received.
	raw := "multi v1/sessW/actW/0 one\nmulti two v1/sessW/actW/1\r\n\nplain\n"
	n, err := w.Write([]byte(raw))
	if err != nil || n != len(raw) {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if !strings.HasSuffix(out.String(), raw) {
		t.Errorf("LogWriter changed the output:\n%q", out.String())
	}

	if got := held("sessCap/actCap"); len(got) != 2 ||
		!strings.Contains(got[0], "charging card for deal 9912") || !strings.Contains(got[1], "Logger-style") {
		t.Errorf("sessCap/actCap holds %q", got)
	}
	// "plain" follows a traced line in the same Write: it is that message's
	// continuation line (#557).
	if got := held("sessW/actW"); len(got) != 3 || strings.HasSuffix(got[1], "\r") ||
		got[2] != "[v1/sessW/actW/1] plain" {
		t.Errorf("sessW/actW holds %q, want both traced lines without their line endings, then plain", got)
	}
	if n, _ := captured.stats(); n != 5 {
		t.Errorf("store holds %d lines, want the 4 traced ones and 1 continuation", n)
	}
}

// A multi-line slog message is held as one entry per line (#134, #557):
// docs/server-sdk/configuration.md "Multi-line messages".
func TestCapture_SlogMultiLineMessageIsOneEntryPerLine(t *testing.T) {
	useCapture(t)
	logger := slog.New(NewLogHandler(slog.NewTextHandler(io.Discard, nil)))
	ctx := WithTrace(context.Background(), Trace{Session: "sessM", Intent: "actM", Hop: 4})

	logger.ErrorContext(ctx, "boom: nil map\r\n  at charge (billing.go:12)\n\n   \n  at main (main.go:3)\n", "deal", 9912)

	got := held("sessM/actM")
	want := []string{
		"ERROR boom: nil map deal=9912 adt_trace=v1/sessM/actM/4",
		"ERROR   at charge (billing.go:12) adt_trace=v1/sessM/actM/4",
		"ERROR   at main (main.go:3) adt_trace=v1/sessM/actM/4",
	}
	if len(got) != len(want) {
		t.Fatalf("held %d entries, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if strings.Contains(got[i], "\n") || !strings.HasSuffix(got[i], want[i]) {
			t.Errorf("entry %d = %q, want it to end %q", i, got[i], want[i])
		}
		// Every entry carries the record's time prefix, not only the first.
		if !strings.HasPrefix(got[i], "20") {
			t.Errorf("entry %d = %q, want the record's time first", i, got[i])
		}
	}

	// Only blank lines: nothing held. One line with no newline is held as
	// before, even when empty.
	captured.reset()
	logger.InfoContext(ctx, "\n  \n\r\n")
	if got := held("sessM/actM"); len(got) != 0 {
		t.Errorf("an all-blank message held %q", got)
	}
	logger.InfoContext(ctx, "single line\n")
	if got := held("sessM/actM"); len(got) != 1 || !strings.HasSuffix(got[0], "INFO single line adt_trace=v1/sessM/actM/4") {
		t.Errorf("a message with only a trailing newline held %q", got)
	}
}

func TestCapture_SlogMultiLineMessageIsCappedAt50(t *testing.T) {
	useCapture(t)
	logger := slog.New(NewLogHandler(slog.NewTextHandler(io.Discard, nil)))
	ctx := WithTrace(context.Background(), Trace{Session: "sessC", Intent: "actC", Hop: 0})

	var msg strings.Builder
	for i := 1; i <= 120; i++ {
		fmt.Fprintf(&msg, "frame %d\n", i)
	}
	logger.ErrorContext(ctx, msg.String())

	got := held("sessC/actC")
	if len(got) != 50 {
		t.Fatalf("held %d entries, want 50", len(got))
	}
	if !strings.HasSuffix(got[48], "ERROR frame 49 adt_trace=v1/sessC/actC/0") {
		t.Errorf("entry 49 = %q", got[48])
	}
	if !strings.HasSuffix(got[49], "ERROR ... (71 more lines) adt_trace=v1/sessC/actC/0") {
		t.Errorf("entry 50 = %q, want the omitted count", got[49])
	}

	// Exactly 50 lines: all kept, no marker.
	captured.reset()
	msg.Reset()
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&msg, "frame %d\n", i)
	}
	logger.ErrorContext(ctx, msg.String())
	if got := held("sessC/actC"); len(got) != 50 || !strings.HasSuffix(got[49], "ERROR frame 50 adt_trace=v1/sessC/actC/0") {
		t.Errorf("a 50-line message held %d entries, last %q", len(got), got[len(got)-1])
	}
}

// A traced multi-line log.Print (an error and its stack): continuation lines
// are held under the trace of the line that starts the message, blank lines
// dropped, at most 50 (#557).
func TestCapture_LogWriterHoldsContinuationLines(t *testing.T) {
	useCapture(t)
	var out bytes.Buffer
	prevW, prevF := log.Writer(), log.Flags()
	log.SetOutput(LogWriter(&out))
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevW); log.SetFlags(prevF) })

	ctx := WithTrace(context.Background(), Trace{Session: "sessS", Intent: "actS", Hop: 1})
	Logger(ctx).Printf("charge failed: timeout\n\tat charge()\r\n\n\tat main()")
	log.Printf("untraced first line\nand its continuation")

	want := []string{
		"[v1/sessS/actS/1] charge failed: timeout",
		"[v1/sessS/actS/1] \tat charge()",
		"[v1/sessS/actS/1] \tat main()",
	}
	got := held("sessS/actS")
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("held %q, want %q", got, want)
	}
	if n, _ := captured.stats(); n != 3 {
		t.Errorf("store holds %d lines, want 3 (an untraced message is not kept)", n)
	}

	captured.reset()
	var msg strings.Builder
	for i := 1; i <= 120; i++ {
		fmt.Fprintf(&msg, "\nframe %d", i)
	}
	Logger(ctx).Print("head" + msg.String())
	got = held("sessS/actS")
	if len(got) != 50 || got[0] != "[v1/sessS/actS/1] head" ||
		got[48] != "[v1/sessS/actS/1] frame 48" || got[49] != "[v1/sessS/actS/1] ... (72 more lines)" {
		t.Errorf("a 121-line message held %d lines: first %q, 49th %q, last %q",
			len(got), got[0], got[min(48, len(got)-1)], got[len(got)-1])
	}
}

func TestCapture_LoggerPrefixesTheTrace(t *testing.T) {
	useCapture(t)
	var out bytes.Buffer
	prevW, prevF := log.Writer(), log.Flags()
	log.SetOutput(LogWriter(&out))
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevW); log.SetFlags(prevF) })

	ctx := WithTrace(context.Background(), Trace{Session: "sessL", Intent: "actL", Hop: 2})
	Logger(ctx).Printf("charging card")
	Logger(context.Background()).Printf("no trace")

	if got := held("sessL/actL"); len(got) != 1 || got[0] != "[v1/sessL/actL/2] charging card" {
		t.Errorf("held %q", got)
	}
	if !strings.Contains(out.String(), "[v1/sessL/actL/2] charging card\nno trace\n") {
		t.Errorf("output = %q", out.String())
	}
}

// A context that hands back the request as Value(0) — gin.Context — is
// keyed by the request's trace.
type requestValueCtx struct {
	context.Context
	r *http.Request
}

func (c requestValueCtx) Value(key any) any {
	if key == 0 {
		return c.r
	}
	return c.Context.Value(key)
}

func TestCapture_ContextThatCarriesTheRequest(t *testing.T) {
	useCapture(t)
	logger := slog.New(NewLogHandler(slog.NewTextHandler(io.Discard, nil)))
	r := tracedRequest(capTrace)
	r = r.WithContext(WithTrace(r.Context(), Trace{Session: "sessCap", Intent: "actCap"}))
	logger.InfoContext(requestValueCtx{Context: context.Background(), r: r}, "via gin-style context")
	if got := held("sessCap/actCap"); len(got) != 1 {
		t.Errorf("held %q", got)
	}
}

type panicky struct{}

func (panicky) String() string { panic("String exploded") }

type panickyValuer struct{}

func (panickyValuer) LogValue() slog.Value { panic("LogValue exploded") }

// Rendering a hostile value costs the captured line, never the log call,
// and never what the wrapped handler does with the record.
func TestCapture_NeverPanicsIntoTheLoggingCall(t *testing.T) {
	useCapture(t)
	var out bytes.Buffer
	logger := slog.New(NewLogHandler(slog.NewTextHandler(&out, nil)))
	ctx := WithTrace(context.Background(), Trace{Session: "sessP", Intent: "actP"})

	logger.InfoContext(ctx, "valuer", "v", panickyValuer{})
	logger.With("bad", panicky{}).InfoContext(ctx, "stringer", "also", panicky{})

	if got := held("sessP/actP"); len(got) != 2 || !strings.Contains(got[1], "stringer") {
		t.Errorf("held %q, want both lines", got)
	}
	if strings.Count(out.String(), "\n") != 2 {
		t.Errorf("the wrapped handler did not get both records:\n%s", out.String())
	}
}

func TestCapture_Bounds(t *testing.T) {
	t.Run("10,000 lines, oldest evicted first", func(t *testing.T) {
		captured.reset()
		t.Cleanup(captured.reset)
		now := time.Now()
		for i := 0; i < logMaxLines+50; i++ {
			captured.add("s/i", fmt.Sprintf("line %05d", i), now)
		}
		got := captured.lookup("s/i", now)
		if len(got) != logMaxLines {
			t.Fatalf("held %d lines, want %d", len(got), logMaxLines)
		}
		if got[0] != "line 00050" || got[len(got)-1] != fmt.Sprintf("line %05d", logMaxLines+49) {
			t.Errorf("held %q .. %q, want the newest 10,000", got[0], got[len(got)-1])
		}
	})

	t.Run("4 MiB, and 4 KiB per line", func(t *testing.T) {
		captured.reset()
		t.Cleanup(captured.reset)
		now := time.Now()
		long := strings.Repeat("é", 3000) // 6000 bytes; a cut must not split a rune
		for i := 0; i < 2000; i++ {
			captured.add("s/i", fmt.Sprintf("%04d %s", i, long), now)
		}
		lines, size := captured.stats()
		if size > logMaxBytes {
			t.Errorf("store holds %d bytes, cap is %d", size, logMaxBytes)
		}
		got := captured.lookup("s/i", now)
		if len(got) != lines || len(got) >= 2000 {
			t.Fatalf("held %d lines; the byte cap evicted nothing", len(got))
		}
		for _, l := range got {
			if len(l) > logMaxLineBytes || !utf8Valid(l) {
				t.Fatalf("line of %d bytes (valid utf-8: %v), cap is %d", len(l), utf8Valid(l), logMaxLineBytes)
			}
		}
		if !strings.HasPrefix(got[len(got)-1], "1999 ") {
			t.Errorf("newest line lost: %q", got[len(got)-1][:10])
		}
	})

	t.Run("nothing older than 15 minutes", func(t *testing.T) {
		captured.reset()
		t.Cleanup(captured.reset)
		start := time.Now()
		captured.add("s/i", "old", start)
		captured.add("s/i", "recent", start.Add(10*time.Minute))
		if got := captured.lookup("s/i", start.Add(16*time.Minute)); len(got) != 1 || got[0] != "recent" {
			t.Errorf("held %q, want only the line under 15 minutes old", got)
		}
		if n, _ := captured.stats(); n != 1 {
			t.Errorf("aged-out line still counted against the bounds: %d", n)
		}
	})
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "�") == s }

// Many goroutines logging at once (run with -race): every line lands under
// its own trace, the bounds hold, and lookups during logging are safe.
func TestCapture_ConcurrentLoggers(t *testing.T) {
	useCapture(t)
	logger := slog.New(NewLogHandler(slog.NewTextHandler(io.Discard, nil)))
	w := LogWriter(io.Discard)
	const workers, each = 16, 800 // 12,800 slog lines + as many std ones: past the line cap

	var wg sync.WaitGroup
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				_ = held("sess0/act0")
			}
		}
	}()
	for g := 0; g < workers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			ctx := WithTrace(context.Background(), Trace{Session: fmt.Sprintf("sess%d", g), Intent: "act0"})
			for i := 0; i < each; i++ {
				logger.InfoContext(ctx, "work", "i", i)
				fmt.Fprintf(w, "[v1/sess%d/act0/0] std %d\n", g, i)
			}
		}(g)
	}
	wg.Wait()
	close(stop)

	lines, size := captured.stats()
	if lines != logMaxLines || size > logMaxBytes {
		t.Errorf("after %d lines: holding %d lines / %d bytes, want exactly %d lines within %d bytes",
			2*workers*each, lines, size, logMaxLines, logMaxBytes)
	}
	for g := 0; g < workers; g++ {
		for _, l := range held(fmt.Sprintf("sess%d/act0", g)) {
			if !strings.Contains(l, fmt.Sprintf("sess%d/act0", g)) {
				t.Fatalf("sess%d holds another trace's line: %q", g, l)
			}
		}
	}
}
