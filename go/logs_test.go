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
	if got := held("sessW/actW"); len(got) != 2 || strings.HasSuffix(got[1], "\r") {
		t.Errorf("sessW/actW holds %q, want both lines without their line endings", got)
	}
	if n, _ := captured.stats(); n != 4 {
		t.Errorf("store holds %d lines, want the 4 traced ones", n)
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
