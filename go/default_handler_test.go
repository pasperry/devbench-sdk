package devbench

import (
	"bytes"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// The README's install line, verbatim:
//
//	slog.SetDefault(slog.New(devbench.NewLogHandler(slog.Default().Handler())))
//
// slog's built-in default handler writes through the log package, and
// slog.SetDefault points the log package back at the new default — so a
// handler wrapping slog.Default().Handler() re-entered itself through the
// log package's mutex and the first slog call deadlocked the request
// forever. Found by tools/installtest on a fresh `go mod init` app; every
// other test wrapped a TextHandler.
func TestNewLogHandler_WrappingTheDefaultHandlerDoesNotDeadlock(t *testing.T) {
	useCapture(t)

	// The process-wide state SetDefault changes, restored afterwards.
	origLogger := slog.Default()
	origOut, origFlags, origPrefix := log.Writer(), log.Flags(), log.Prefix()
	var out syncBuffer
	log.SetOutput(&out)
	deadlocked := false
	t.Cleanup(func() {
		if deadlocked {
			return // the stuck goroutine holds the log package's mutex
		}
		slog.SetDefault(origLogger)
		log.SetOutput(origOut)
		log.SetFlags(origFlags)
		log.SetPrefix(origPrefix)
	})

	slog.SetDefault(slog.New(NewLogHandler(slog.Default().Handler())))

	done := make(chan struct{})
	go func() {
		defer close(done)
		app := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			slog.InfoContext(r.Context(), "charging card", "deal", 9912)
			slog.Info("no context")
			log.Printf("from the log package")
		}))
		app.ServeHTTP(httptest.NewRecorder(), tracedRequest(capTrace))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		deadlocked = true
		t.Fatal("a slog call through NewLogHandler(slog.Default().Handler()) never returned: deadlock")
	}

	// What the application wrote looks as it did with slog's default
	// handler: the log package's date prefix, then LEVEL message attrs.
	text := out.String()
	date := `\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `
	for _, want := range []string{
		date + `INFO charging card deal=9912 adt_trace=v1/sessCap/actCap/0\n`,
		date + `INFO no context\n`,
		date + `INFO from the log package\n`,
	} {
		if !regexp.MustCompile(want).MatchString(text) {
			t.Errorf("output lacks a line matching %q:\n%s", want, text)
		}
	}
	if strings.Count(text, "charging card") != 1 {
		t.Errorf("the record was written %d times, want once:\n%s", strings.Count(text, "charging card"), text)
	}

	lines := held("sessCap/actCap")
	if len(lines) != 1 || !strings.HasSuffix(lines[0], "INFO charging card deal=9912 adt_trace=v1/sessCap/actCap/0") {
		t.Errorf("held %q, want the one traced line", lines)
	}
}

// The level the default handler honours (slog.SetLogLoggerLevel) still
// decides what is written and kept.
func TestNewLogHandler_DefaultHandlerKeepsItsLevel(t *testing.T) {
	useCapture(t)
	origLogger := slog.Default()
	origOut, origFlags := log.Writer(), log.Flags()
	var out syncBuffer
	log.SetOutput(&out)
	t.Cleanup(func() {
		slog.SetDefault(origLogger)
		log.SetOutput(origOut)
		log.SetFlags(origFlags)
	})

	h := NewLogHandler(slog.Default().Handler())
	logger := slog.New(h)
	app := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.DebugContext(r.Context(), "debug is below the default level")
		logger.InfoContext(r.Context(), "info is not")
	}))
	app.ServeHTTP(httptest.NewRecorder(), tracedRequest(capTrace))

	if strings.Contains(out.String(), "debug is below") {
		t.Errorf("a debug record was written:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "INFO info is not") {
		t.Errorf("the info record was not written:\n%s", out.String())
	}
	if lines := held("sessCap/actCap"); len(lines) != 1 {
		t.Errorf("held %q, want only the info line", lines)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
