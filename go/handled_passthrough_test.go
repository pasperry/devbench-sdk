package devbench_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	devbench "github.com/pasperry/devbench-sdk/go"
)

// The README wiring. Every test here goes through all three wrappers, because
// any one of them hiding Flusher or Hijacker breaks streaming for the app.
func readmeStack(h http.Handler) http.Handler {
	return devbench.Recover(devbench.Middleware(devbench.Handled(h)))
}

// unwrapOnly is the shape of many third-party middlewares (loggers, metrics):
// it wraps the writer and offers only Unwrap. Anything below it must reach
// the real writer through http.ResponseController, not a type assertion.
type unwrapOnly struct{ http.ResponseWriter }

func (u unwrapOnly) Unwrap() http.ResponseWriter { return u.ResponseWriter }

func behindUnwrapOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(unwrapOnly{w}, r)
	})
}

// streamingHandler reports a handled failure, then streams: its very first
// output is a Flush (so Flush, not Write, sends the headers), then a chunk,
// then it holds the response open until release closes.
func streamingHandler(t *testing.T, release <-chan struct{}) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		devbench.ReportHandled(r.Context(), errors.New("cache miss"), "events.Stream")
		f, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f.Flush()
		_, _ = w.Write([]byte("data: tick\n\n"))
		f.Flush()
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			t.Error("the client never released the handler")
		}
	})
}

// assertStreams checks the headers and the first chunk arrive while the
// handler is still running, and that the handled count rode on the headers.
func assertStreams(t *testing.T, url string, release chan struct{}) {
	t.Helper()
	type result struct {
		res   *http.Response
		chunk string
		err   error
	}
	got := make(chan result, 1)
	go func() {
		res, err := http.Get(url)
		if err != nil {
			got <- result{err: err}
			return
		}
		buf := make([]byte, len("data: tick\n\n"))
		n, err := io.ReadFull(res.Body, buf)
		got <- result{res: res, chunk: string(buf[:n]), err: err}
	}()

	select {
	case r := <-got:
		close(release)
		if r.err != nil {
			t.Fatalf("stream: %v", r.err)
		}
		defer r.res.Body.Close()
		if r.res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", r.res.StatusCode)
		}
		if r.chunk != "data: tick\n\n" {
			t.Errorf("first chunk = %q", r.chunk)
		}
		if h := r.res.Header.Get(devbench.HandledHeader); h != "1" {
			t.Errorf("%s = %q, want 1: the flush sent the headers, so it must stamp the count first",
				devbench.HandledHeader, h)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("the flushed chunk did not reach the client while the handler ran")
	}
}

// Server-sent events behind the README wiring: the first chunk reaches the
// client while the handler is still running, with the handled count stamped.
func TestHandled_FlushStreamsAndStampsTheCount(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(readmeStack(streamingHandler(t, release)))
	defer srv.Close()
	assertStreams(t, srv.URL+"/events", release)
}

// The same, with an Unwrap-only middleware outside the Dev Bench stack.
func TestHandled_FlushStreamsBelowAnUnwrapOnlyWrapper(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(behindUnwrapOnly(readmeStack(streamingHandler(t, release))))
	defer srv.Close()
	assertStreams(t, srv.URL+"/events", release)
}

// hijackingHandler upgrades the connection the way a websocket library does:
// Hijack, then write the response on the raw connection. "/rc" exercises
// http.ResponseController, which needs Unwrap to reach the real writer.
func hijackingHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		devbench.ReportHandled(r.Context(), errors.New("ignored"), "ws.Upgrade")
		if r.URL.Path == "/rc" {
			if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Minute)); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijacker", http.StatusInternalServerError)
			return
		}
		conn, brw, err := h.Hijack()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer conn.Close()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\nhello")
		_ = brw.Flush()
	})
}

func assertHijackAndController(t *testing.T, srvURL string) {
	t.Helper()

	res, err := http.Get(srvURL + "/rc")
	if err != nil {
		t.Fatalf("/rc: %v", err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Errorf("/rc: status = %d %q, want 204 (ResponseController.SetWriteDeadline)", res.StatusCode, body)
	}

	req, _ := http.NewRequest(http.MethodGet, srvURL+"/ws", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("/ws: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("/ws: status = %d %q, want 101 from the hijacked connection", res.StatusCode, body)
	}
	if res.Header.Get(devbench.HandledHeader) != "" {
		t.Errorf("a hijacked response carried %s: the handler wrote these headers itself", devbench.HandledHeader)
	}
	// net/http hands a 101's connection over as the body.
	rest, _ := io.ReadAll(res.Body)
	if string(rest) != "hello" {
		t.Errorf("/ws: upgraded stream = %q, want hello", rest)
	}
}

// Websocket upgrades and ResponseController behind the README wiring.
func TestHandled_HijackAndResponseControllerPassThrough(t *testing.T) {
	srv := httptest.NewServer(readmeStack(hijackingHandler()))
	defer srv.Close()
	assertHijackAndController(t, srv.URL)
}

func TestHandled_HijackPassesThroughBelowAnUnwrapOnlyWrapper(t *testing.T) {
	srv := httptest.NewServer(behindUnwrapOnly(readmeStack(hijackingHandler())))
	defer srv.Close()
	assertHijackAndController(t, srv.URL)
}

// HTTP/2 cannot hijack. The handler gets http.ErrNotSupported (as it would
// without Dev Bench), not a missing interface or a panic.
func TestHandled_HijackOverHTTP2IsNotSupported(t *testing.T) {
	srv := httptest.NewUnstartedServer(readmeStack(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijacker", http.StatusInternalServerError)
			return
		}
		_, _, err := h.Hijack()
		if !errors.Is(err, http.ErrNotSupported) {
			http.Error(w, "hijack err = "+errString(err), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("not supported"))
	})))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	res, err := srv.Client().Get(srv.URL + "/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.ProtoMajor != 2 {
		t.Fatalf("proto = %s, want HTTP/2", res.Proto)
	}
	if res.StatusCode != http.StatusOK || string(body) != "not supported" {
		t.Errorf("status = %d %q, want 200 \"not supported\"", res.StatusCode, body)
	}
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// bareWriter is a ResponseWriter with nothing else: no Flush, no Hijack.
type bareWriter struct{ h http.Header }

func (b *bareWriter) Header() http.Header         { return b.h }
func (b *bareWriter) Write(p []byte) (int, error) { return len(p), nil }
func (b *bareWriter) WriteHeader(int)             {}

// A handler using http.ResponseController learns that the writer below cannot
// flush, as it would without Dev Bench, instead of a silent success.
func TestHandled_ResponseControllerFlushReportsUnsupported(t *testing.T) {
	var flushErr error
	h := readmeStack(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flushErr = http.NewResponseController(w).Flush()
	}))
	h.ServeHTTP(&bareWriter{h: http.Header{}}, httptest.NewRequest(http.MethodGet, "/", nil))
	if !errors.Is(flushErr, http.ErrNotSupported) {
		t.Errorf("ResponseController.Flush = %v, want http.ErrNotSupported", flushErr)
	}
}
