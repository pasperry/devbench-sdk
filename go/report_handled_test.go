package devbench

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"
)

type conflictError struct{ key string }

func (e *conflictError) Error() string { return "conflict on " + e.key }

type codeError struct{ code int }

func (e codeError) Error() string { return fmt.Sprintf("code %d", e.code) }

// brokenError's Error panics, as a nil-receiver method would.
type brokenError struct{ inner *conflictError }

func (e *brokenError) Error() string { return e.inner.key }

// The whole path an app uses: a real server, the real middleware stack, a
// handler that reports, and the line that reaches a real socket.
func TestReportHandled_SendsHandledFailureToTheSidecar(t *testing.T) {
	s := listenSidecar(t)

	srv := httptest.NewServer(Middleware(Handled(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := WithUser(r.Context(), User{Email: " Pat@Example.com", Account: "acct-1182"})
		err := fmt.Errorf("persist invoice: %w", &conflictError{key: "invoices_number_key"})
		ReportHandled(ctx, err, "billing.Invoicer.Persist")
		w.WriteHeader(http.StatusOK)
	}))))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/invoices", nil)
	req.Header.Set(Header, "v1/sessA/act1/0")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	// The existing contract is unchanged.
	if got := res.Header.Get(HandledHeader); got != "1" {
		t.Errorf("%s = %q, want 1", HandledHeader, got)
	}

	m := s.next(t)
	want := map[string]any{
		"v":       float64(1),
		"kind":    "handled_failure",
		"symbol":  "billing.Invoicer.Persist",
		"error":   "*devbench.conflictError",
		"message": "persist invoice: conflict on invoices_number_key",
		"trace":   "v1/sessA/act1/0",
		"user":    map[string]any{"email": "pat@example.com", "account": "acct-1182"},
	}
	for k, v := range want {
		if fmt.Sprint(m[k]) != fmt.Sprint(v) {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
	if len(m) != len(want) {
		t.Errorf("unexpected fields: %v", m)
	}
}

func TestReportHandled_WithoutTraceOrUserOmitsThem(t *testing.T) {
	s := listenSidecar(t)
	ReportHandled(context.Background(), errors.New("boom"), "x.Y")

	m := s.next(t)
	if _, ok := m["trace"]; ok {
		t.Errorf("trace present without one bound: %v", m)
	}
	if _, ok := m["user"]; ok {
		t.Errorf("user present without one set: %v", m)
	}
	if m["error"] != "*errors.errorString" || m["message"] != "boom" {
		t.Errorf("got %v", m)
	}
}

func TestReportHandled_NilErrorSendsNothing(t *testing.T) {
	s := listenSidecar(t)
	ctx := WithHandledCounter(context.Background())

	ReportHandled(ctx, nil, "nothing.Failed")
	ReportHandled(context.Background(), errors.New("marker"), "marker")

	// One FIFO writer: if the nil report had been sent, it would be first.
	if m := s.next(t); m["symbol"] != "marker" {
		t.Errorf("first message = %v, want the marker", m)
	}
	if n := HandledCount(ctx); n != 0 {
		t.Errorf("nil error counted: %d", n)
	}
}

func TestReportHandled_MessageIsTruncatedTo2000Characters(t *testing.T) {
	s := listenSidecar(t)
	ReportHandled(context.Background(), errors.New(strings.Repeat("ü", 5000)), "x")

	msg, _ := s.next(t)["message"].(string)
	if n := len([]rune(msg)); n != 2000 {
		t.Errorf("message is %d characters, want 2000", n)
	}
	if !strings.HasSuffix(msg, "…") {
		t.Errorf("truncation is not marked")
	}
}

// An Error method that panics must not escape into the customer's rescue
// path, and the type and site — the fingerprint — still arrive.
func TestReportHandled_PanickingErrorIsSwallowedAndStillReported(t *testing.T) {
	s := listenSidecar(t)
	ctx := WithHandledCounter(context.Background())

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("ReportHandled panicked: %v", r)
			}
		}()
		ReportHandled(ctx, &brokenError{}, "broken.Site")
	}()

	m := s.next(t)
	if m["error"] != "*devbench.brokenError" || m["symbol"] != "broken.Site" {
		t.Errorf("got %v", m)
	}
	if HandledCount(ctx) != 1 {
		t.Errorf("count = %d, want 1", HandledCount(ctx))
	}
}

// The wrapped-error naming rule (see errorType).
func TestErrorType_NamesTheFirstMeaningfulTypeInTheChain(t *testing.T) {
	conflict := &conflictError{key: "k"}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"plain errors.New", errors.New("x"), "*errors.errorString"},
		{"fmt.Errorf without %w", fmt.Errorf("x %d", 1), "*errors.errorString"},
		{"custom type", conflict, "*devbench.conflictError"},
		{"wrapped once", fmt.Errorf("a: %w", conflict), "*devbench.conflictError"},
		{"wrapped twice", fmt.Errorf("b: %w", fmt.Errorf("a: %w", conflict)), "*devbench.conflictError"},
		{"value type", fmt.Errorf("a: %w", codeError{code: 7}), "devbench.codeError"},
		{"wrapped sentinel", fmt.Errorf("load: %w", fs.ErrNotExist), "*errors.errorString"},
		{"wrapped errno", fmt.Errorf("dial: %w", syscall.ECONNREFUSED), "syscall.Errno"},
		{"named wrapper is kept", &url.Error{Op: "Get", URL: "u", Err: conflict}, "*url.Error"},
		{"errors.Join follows the first", errors.Join(conflict, errors.New("y")), "*devbench.conflictError"},
		{"several %w follow the first", fmt.Errorf("%w and %w", codeError{1}, conflict), "devbench.codeError"},
		{"empty join", errors.Join(), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := errorType(tc.err); got != tc.want {
				t.Errorf("errorType = %q, want %q", got, tc.want)
			}
		})
	}
}

// The concurrency contract, through the public entry point: 1000 goroutines
// reporting against an absent, a wedged, and a healthy sidecar. Run under
// -race. No call may wait on I/O, and the counter must stay exact.
func TestReportHandled_ConcurrentCallersNeverBlock(t *testing.T) {
	for _, state := range []string{"absent", "stalled", "present"} {
		t.Run(state, func(t *testing.T) {
			var s *sidecar
			switch state {
			case "absent":
				useSocket(t, socketPath(t))
			case "stalled":
				useSocket(t, stalledSidecar(t))
			case "present":
				s = listenSidecar(t)
			}

			const goroutines, perG = 1000, 10
			ctx := WithUser(WithTrace(WithHandledCounter(context.Background()), Trace{Session: "s", Intent: "i"}), User{Email: "a@b.c"})

			start := time.Now()
			worst := concurrentSends(t, goroutines, perG, func(g, i int) {
				ReportHandled(ctx, fmt.Errorf("op %d: %w", i, &conflictError{key: "k"}), "hot.Loop")
			})
			total := time.Since(start)

			// The bound has to separate "never waits on I/O" from "waits on the
			// socket", not measure scheduler noise: a caller that blocked on a
			// stalled sidecar would wait out the 250ms write deadline. 150ms
			// sits well under that and well over what -race on a busy CI
			// runner costs a non-blocking send (a 50ms bound flaked at 50.8ms).
			if worst > 150*time.Millisecond {
				t.Errorf("slowest ReportHandled took %v with the sidecar %s", worst, state)
			}
			if total > 5*time.Second {
				t.Errorf("%d calls took %v", goroutines*perG, total)
			}
			if got := HandledCount(ctx); got != goroutines*perG {
				t.Errorf("handled count = %d, want %d", got, goroutines*perG)
			}

			if s != nil {
				closeReporter(t)
				m := s.next(t)
				if m["symbol"] != "hot.Loop" || m["error"] != "*devbench.conflictError" {
					t.Errorf("present sidecar received %v", m)
				}
			}
		})
	}
}
