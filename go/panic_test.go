package devbench

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

// panicServer serves h behind the real stack — Middleware(Handled(...)) —
// inside an outermost recoverer standing in for the application's own panic
// handling. It returns the server and a channel of what that recoverer
// caught, which is how the tests prove the panic was re-raised unchanged.
func panicServer(t *testing.T, h http.Handler) (*httptest.Server, <-chan any) {
	t.Helper()
	caught := make(chan any, 10)
	outer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				caught <- v
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()
		Middleware(Handled(h)).ServeHTTP(w, r)
	})
	srv := httptest.NewServer(outer)
	t.Cleanup(srv.Close)
	return srv, caught
}

func get(t *testing.T, url string, trace string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if trace != "" {
		req.Header.Set(Header, trace)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	res.Body.Close()
	return res
}

func caughtValue(t *testing.T, caught <-chan any) any {
	t.Helper()
	select {
	case v := <-caught:
		return v
	default:
		t.Fatal("the panic did not propagate past devbench.Middleware")
		return nil
	}
}

func frames(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	raw, ok := m["frames"].([]any)
	if !ok {
		t.Fatalf("no frames: %v", m)
	}
	out := make([]map[string]any, len(raw))
	for i, f := range raw {
		out[i] = f.(map[string]any)
	}
	return out
}

var panicLine int

func panickingHandler(w http.ResponseWriter, r *http.Request) {
	WithUser(r.Context(), User{Email: "Pat@Example.com", Account: "acct-1182"})
	_, _, line, _ := runtime.Caller(0)
	panicLine = line + 2
	panic(fmt.Errorf("load deal %s: %w", r.PathValue("id"), &conflictError{key: "deals_pkey"}))
}

func TestMiddleware_PanicIsReportedAndRepanicked(t *testing.T) {
	s := listenSidecar(t)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /deals/{id}", panickingHandler)
	srv, caught := panicServer(t, mux)

	res := get(t, srv.URL+"/deals/9912", "v1/sessA/act1/2")

	// Propagated: the application's own recovery ran and saw the original
	// value, not a copy or a wrapper.
	if res.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want the outer recoverer's 500", res.StatusCode)
	}
	v := caughtValue(t, caught)
	err, ok := v.(error)
	if !ok || !strings.Contains(err.Error(), "load deal 9912") {
		t.Fatalf("re-panicked value = %#v, want the original error", v)
	}

	m := s.next(t)
	checks := map[string]any{
		"v":       float64(1),
		"kind":    "exception",
		"context": "request",
		"handled": false,
		"error":   "*devbench.conflictError",
		"message": "load deal 9912: conflict on deals_pkey",
		"symbol":  wantSymbol("GET /deals/{id}"), // the pattern, never the raw path
		"trace":   "v1/sessA/act1/2",
	}
	for k, want := range checks {
		if m[k] != want {
			t.Errorf("%s = %#v, want %#v", k, m[k], want)
		}
	}
	// Set inside the handler, on a context Middleware never sees.
	if u, _ := m["user"].(map[string]any); u["email"] != "pat@example.com" || u["account"] != "acct-1182" {
		t.Errorf("user = %v", m["user"])
	}

	fs := frames(t, m)
	top := fs[0]
	if !strings.HasSuffix(top["function"].(string), "devbench-sdk/go.panickingHandler") {
		t.Errorf("innermost frame = %v, want panickingHandler", top)
	}
	if top["file"] != "panic_test.go" {
		t.Errorf("file = %v, want a path relative to the module root", top["file"])
	}
	if top["line"] != float64(panicLine) {
		t.Errorf("line = %v, want %d (the panic statement)", top["line"], panicLine)
	}
	for _, f := range fs {
		fn := f["function"].(string)
		if strings.HasPrefix(fn, "runtime.") || strings.HasPrefix(fn, "net/http.") ||
			(strings.HasPrefix(fn, selfPkg+".") && !strings.HasSuffix(f["file"].(string), "_test.go")) {
			t.Errorf("non-application frame reported: %v", f)
		}
	}
}

func TestMiddleware_NonErrorPanicValue(t *testing.T) {
	s := listenSidecar(t)
	srv, caught := panicServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(struct{ N int }{42})
	}))

	get(t, srv.URL+"/x", "")
	if v := caughtValue(t, caught); v != any(struct{ N int }{42}) {
		t.Errorf("re-panicked value = %#v", v)
	}

	m := s.next(t)
	if m["error"] != "panic" || m["message"] != "{42}" {
		t.Errorf("error/message = %v / %v, want panic / {42}", m["error"], m["message"])
	}
	// No ServeMux, so no pattern: symbol is omitted rather than a raw path.
	if _, ok := m["symbol"]; ok {
		t.Errorf("symbol = %v without a route pattern", m["symbol"])
	}
	if _, ok := m["trace"]; ok {
		t.Errorf("trace present without a header: %v", m["trace"])
	}
}

// A nil dereference panics from inside the runtime (sigpanic, panicmem).
// Those frames are not the application; the handler is the innermost frame.
func TestMiddleware_RuntimeErrorStartsAtTheFaultingFrame(t *testing.T) {
	s := listenSidecar(t)
	srv, caught := panicServer(t, http.HandlerFunc(nilDeref))

	get(t, srv.URL+"/x", "")
	if _, ok := caughtValue(t, caught).(runtime.Error); !ok {
		t.Error("the runtime error was not re-panicked as itself")
	}

	m := s.next(t)
	if e, _ := m["error"].(string); !strings.HasPrefix(e, "runtime.") {
		t.Errorf("error = %v", m["error"])
	}
	if fn := frames(t, m)[0]["function"]; !strings.HasSuffix(fn.(string), "devbench-sdk/go.nilDeref") {
		t.Errorf("innermost frame = %v, want nilDeref", fn)
	}
}

func nilDeref(w http.ResponseWriter, r *http.Request) {
	var c *conflictError
	_, _ = w.Write([]byte(c.key))
}

func TestMiddleware_FramesAreCappedAt50(t *testing.T) {
	s := listenSidecar(t)
	srv, _ := panicServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		recurse(200)
	}))

	get(t, srv.URL+"/x", "")
	fs := frames(t, s.next(t))
	if len(fs) != 50 {
		t.Errorf("%d frames, want 50", len(fs))
	}
	if !strings.HasSuffix(fs[0]["function"].(string), "devbench-sdk/go.recurse") {
		t.Errorf("innermost = %v", fs[0])
	}
}

func recurse(n int) {
	if n == 0 {
		panic("deep")
	}
	recurse(n - 1)
}

// http.ErrAbortHandler is how a handler tells net/http to abort the response.
// It is control flow, not a failure: re-panicked, never reported.
func TestMiddleware_ErrAbortHandlerIsRepanickedWithoutAReport(t *testing.T) {
	s := listenSidecar(t)
	srv, caught := panicServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	get(t, srv.URL+"/x", "")
	if v := caughtValue(t, caught); v != any(http.ErrAbortHandler) {
		t.Errorf("re-panicked value = %#v, want http.ErrAbortHandler", v)
	}

	send(message{V: 1, Kind: "handled_failure", Symbol: "marker"})
	if m := s.next(t); m["symbol"] != "marker" {
		t.Errorf("ErrAbortHandler was reported: %v", m)
	}
}

// Without the application's own recovery, net/http's still applies — the
// connection is dropped for that one request and the server keeps serving.
func TestMiddleware_PanicReachesNetHTTPUnchanged(t *testing.T) {
	s := listenSidecar(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/boom", func(http.ResponseWriter, *http.Request) { panic("boom") })
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	srv := httptest.NewUnstartedServer(Middleware(mux))
	srv.Config.ErrorLog = discardLogger()
	srv.Start()
	defer srv.Close()

	if _, err := http.Get(srv.URL + "/boom"); err == nil {
		t.Error("a panicking request got a response: the panic was swallowed")
	}
	if m := s.next(t); m["kind"] != "exception" || m["symbol"] != wantSymbol("/boom") {
		t.Errorf("report = %v", m)
	}
	if res := get(t, srv.URL+"/ok", ""); res.StatusCode != http.StatusNoContent {
		t.Errorf("server stopped serving after a panic: %d", res.StatusCode)
	}
}

func TestMiddleware_NoPanicNoReport(t *testing.T) {
	s := listenSidecar(t)
	srv, caught := panicServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	if res := get(t, srv.URL+"/x", ""); res.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d", res.StatusCode)
	}
	if len(caught) != 0 {
		t.Error("a panic appeared from nowhere")
	}
	send(message{V: 1, Kind: "handled_failure", Symbol: "marker"})
	if m := s.next(t); m["symbol"] != "marker" {
		t.Errorf("unexpected report: %v", m)
	}
}

// discardLogger keeps net/http's "http: panic serving" noise out of the
// output of the one test that lets a panic reach it.
func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func TestRelFile(t *testing.T) {
	mod := mainModule // the test binary's main module: github.com/pasperry/devbench-sdk/go
	cases := []struct{ pkg, file, want string }{
		{mod + "/sub", "/home/ci/src/sdk/server/go/sub/x.go", "sub/x.go"},
		{mod + "/sub", mod + "/sub/x.go", "sub/x.go"}, // -trimpath
		{mod, "/anywhere/root.go", "root.go"},
		{mod + "/sub", "/elsewhere/other/x.go", "/elsewhere/other/x.go"}, // path disagrees: leave it
		{"github.com/dep/lib", "/go/pkg/mod/github.com/dep/lib@v1.0.0/x.go", "/go/pkg/mod/github.com/dep/lib@v1.0.0/x.go"},
	}
	for _, tc := range cases {
		if got := relFile(tc.pkg, tc.file); got != tc.want {
			t.Errorf("relFile(%q, %q) = %q, want %q", tc.pkg, tc.file, got, tc.want)
		}
	}
}

func TestFuncPackage(t *testing.T) {
	cases := map[string]string{
		"github.com/acme/billing/inv.(*Invoicer).Persist": "github.com/acme/billing/inv",
		"github.com/acme/billing/inv.Load.func1":          "github.com/acme/billing/inv",
		"net/http.(*conn).serve":                          "net/http",
		"main.main":                                       "main",
		// The runtime escapes dots in the last path element.
		"gopkg.in/yaml%2ev3.Unmarshal": "gopkg.in/yaml%2ev3",
	}
	for fn, want := range cases {
		if got := funcPackage(fn); got != want {
			t.Errorf("funcPackage(%q) = %q, want %q", fn, got, want)
		}
	}
}
