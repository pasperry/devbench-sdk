package devbench

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer is a server ErrorLog the test can read while the server runs.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// recoverServer serves h behind Recover on a real server whose ErrorLog the
// test reads.
func recoverServer(t *testing.T, h http.Handler) (*httptest.Server, *lockedBuffer) {
	t.Helper()
	logs := &lockedBuffer{}
	srv := httptest.NewUnstartedServer(Recover(h))
	srv.Config.ErrorLog = log.New(logs, "", 0)
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, logs
}

// The README stack: a panicking handler is reported once by Middleware and
// answered 500 by Recover, instead of net/http's empty reply.
func TestRecover_PanicIsReportedOnceAndAnswered500(t *testing.T) {
	s := listenSidecar(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /deals/{id}", func(w http.ResponseWriter, r *http.Request) {
		// Headers for the response the handler never sends.
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Set-Cookie", "session=half-built")
		panickingHandler(w, r)
	})
	srv, logs := recoverServer(t, Middleware(Handled(mux)))

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/deals/9912", nil)
	req.Header.Set(Header, "v1/sessR/act1/0")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("a panicking request got no response: %v", err)
	}
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatalf("reading the 500: %v", err)
	}
	if res.StatusCode != http.StatusInternalServerError || string(body) != "Internal Server Error\n" {
		t.Errorf("response = %d %q, want 500 Internal Server Error", res.StatusCode, body)
	}
	for _, h := range []string{"Content-Encoding", "Set-Cookie"} {
		if v := res.Header.Get(h); v != "" {
			t.Errorf("the 500 carries the handler's %s: %q", h, v)
		}
	}

	m := s.next(t)
	if m["kind"] != "exception" || m["context"] != "request" ||
		m["message"] != "load deal 9912: conflict on deals_pkey" || m["trace"] != "v1/sessR/act1/0" {
		t.Errorf("report = %v", m)
	}
	// Exactly one: the next thing the sidecar sees is this marker.
	send(message{V: 1, Kind: "handled_failure", Symbol: "marker"})
	if m := s.next(t); m["symbol"] != "marker" {
		t.Errorf("a second report: %v", m)
	}

	// Logged as net/http logs an unrecovered panic, with the stack.
	got := logs.String()
	for _, want := range []string{"http: panic serving ", "load deal 9912", "panickingHandler"} {
		if !strings.Contains(got, want) {
			t.Errorf("server log lacks %q:\n%s", want, got)
		}
	}
}

// http.ErrAbortHandler is control flow: re-panicked as itself, with no 500
// and no log line.
func TestRecover_ErrAbortHandlerIsRepanicked(t *testing.T) {
	caught := make(chan any, 1)
	logs := &lockedBuffer{}
	outer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			caught <- v
			w.WriteHeader(http.StatusTeapot)
		}()
		Recover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic(http.ErrAbortHandler)
		})).ServeHTTP(w, r)
	})
	srv := httptest.NewUnstartedServer(outer)
	srv.Config.ErrorLog = log.New(logs, "", 0)
	srv.Start()
	defer srv.Close()

	res := get(t, srv.URL+"/x", "")
	if v := <-caught; v != any(http.ErrAbortHandler) {
		t.Errorf("re-panicked value = %#v, want http.ErrAbortHandler", v)
	}
	if res.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d: Recover answered an ErrAbortHandler", res.StatusCode)
	}
	if got := logs.String(); got != "" {
		t.Errorf("ErrAbortHandler was logged: %s", got)
	}
}

// A panic after the response started cannot become a 500: the connection is
// aborted, so the client sees a failed read, not a complete response.
func TestRecover_PanicAfterTheResponseStartedAbortsIt(t *testing.T) {
	srv, logs := recoverServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		panic("mid-stream")
	}))

	res, err := http.Get(srv.URL + "/x")
	if err != nil {
		t.Fatalf("the started response never arrived: %v", err)
	}
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err == nil {
		t.Errorf("body %q read cleanly: the half-sent response looks complete", body)
	}
	if strings.Contains(string(body), "Internal Server Error") {
		t.Errorf("a 500 body was appended to a started response: %q", body)
	}
	if !strings.Contains(logs.String(), "mid-stream") {
		t.Errorf("the panic was not logged: %q", logs.String())
	}
}

// A 1xx informational header does not start the response.
func TestRecover_PanicAfterAnEarlyHintIsStill500(t *testing.T) {
	srv, _ := recoverServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusEarlyHints)
		panic("after 103")
	}))

	res, err := http.Get(srv.URL + "/x")
	if err != nil {
		t.Fatalf("no response: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", res.StatusCode)
	}
}

// Without a panic, Recover changes nothing and logs nothing.
func TestRecover_NoPanicPassesThrough(t *testing.T) {
	srv, logs := recoverServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-App", "1")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("made"))
	}))

	res, err := http.Get(srv.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusCreated || string(body) != "made" || res.Header.Get("X-App") != "1" {
		t.Errorf("response = %d %q X-App=%q", res.StatusCode, body, res.Header.Get("X-App"))
	}
	if got := logs.String(); got != "" {
		t.Errorf("logged without a panic: %s", got)
	}
}

// Streaming handlers wrapped by Recover still flush: the client reads the
// first chunk while the handler is still running.
func TestRecover_FlushReachesTheClient(t *testing.T) {
	release := make(chan struct{})
	srv, _ := recoverServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("tick\n"))
		f.Flush()
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
	}))
	defer close(release)

	res, err := http.Get(srv.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 5)
		n, _ := io.ReadFull(res.Body, buf)
		got <- string(buf[:n])
	}()
	select {
	case s := <-got:
		if s != "tick\n" {
			t.Errorf("first chunk = %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Error("the flushed chunk did not reach the client while the handler ran")
	}
}

// Hijack and http.ResponseController pass through the wrapper.
func TestRecover_HijackAndResponseControllerPassThrough(t *testing.T) {
	srv, _ := recoverServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		_, _ = brw.WriteString("HTTP/1.1 202 Accepted\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		_ = brw.Flush()
	}))

	for path, want := range map[string]int{"/hijack": http.StatusAccepted, "/rc": http.StatusNoContent} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != want {
			t.Errorf("%s: status = %d %q, want %d", path, res.StatusCode, body, want)
		}
	}
}

// A panic after Hijack writes nothing: the connection is the handler's.
func TestRecover_PanicAfterHijackWritesNothing(t *testing.T) {
	srv, logs := recoverServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_, _ = brw.WriteString("HTTP/1.1 202 Accepted\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		_ = brw.Flush()
		conn.Close()
		panic("after hijack")
	}))

	res, err := http.Get(srv.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want the handler's own 202", res.StatusCode)
	}
	// The panic is logged; net/http's complaint about a write to a hijacked
	// connection must not be.
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logs.String(), "after hijack") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := logs.String()
	if !strings.Contains(got, "after hijack") {
		t.Fatalf("the panic was not logged: %q", got)
	}
	time.Sleep(50 * time.Millisecond) // anything Recover wrote next is logged by now
	if got := logs.String(); strings.Contains(got, "hijacked connection") {
		t.Errorf("Recover wrote to a hijacked connection:\n%s", got)
	}
}
