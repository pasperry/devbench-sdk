// Package devbench is the Dev Bench (formerly ADT) server SDK for Go.
//
// Capabilities, specified in docs/SERVER_SDK_SPEC.md before either SDK was
// written, because two SDKs written in sequence diverge in exactly the places
// that matter:
//
//  1. Trace propagation — accept, carry, and *forward* the correlation id
//     (Middleware, WrapTransport, NewLogHandler)
//  2. Unhandled failure capture — panics in Middleware, reported and then
//     re-panicked; CaptureException for explicit reports
//  3. ReportHandled at failure sites
//  4. The verify probe
//  5. Identity — WithUser
//
// Typical setup:
//
//	devbench.Init(devbench.Options{}) // optional: DEVBENCH_DSN is read on first use
//	defer devbench.Close(context.Background())
//
//	mux := http.NewServeMux()
//	mux.HandleFunc("GET /deals/{id}", showDeal)
//	http.ListenAndServe(":8080", devbench.Middleware(devbench.Handled(mux)))
//
//	// in a handler
//	ctx := devbench.WithUser(r.Context(), devbench.User{Email: u.Email, Account: acct.ID})
//	devbench.ReportHandled(ctx, err, "deals.Update")
//	devbench.CaptureException(ctx, err)
//
// Reports take exactly one of two transports, decided at start
// (SERVER_SDK_SPEC "Transports"):
//
//   - direct (a DSN is set — DEVBENCH_DSN, ADT_DSN or Options.DSN): reports
//     are fingerprinted, counted and redacted in-process and flushed to
//     ingest every minute from a background goroutine, with evidence uploaded
//     only when ingest asks (direct.go);
//   - sidecar (no DSN): reports are written to the local sidecar's unix
//     socket ($ADT_SIDECAR_SOCKET, default /tmp/adt-sidecar.sock), which does
//     the rest (reporter.go). With no sidecar they are dropped.
//
// Either way the caller never waits on I/O. Close flushes on graceful
// shutdown; nothing needs starting.
//
// No dependencies outside the standard library, and none planned. This runs
// inside someone else's application.
package devbench

import (
	"context"
	"net/http"
	"strconv"
	"strings"
)

// Header carries the correlation id. Lowercase because that is how it appears
// on the wire in HTTP/2 and how Go canonicalizes it anyway.
const Header = "x-adt-trace"

// maxHop bounds forwarding. A request that has crossed a hundred services is a
// routing loop, and continuing to propagate would help it along.
const maxHop = 99

// idMaxLen matches the spec: [A-Za-z0-9_-]{1,64}.
const idMaxLen = 64

// Trace identifies one user action as it crosses services.
//
// Session and Intent come from the browser and never change; Hop increments at
// each service. A slice of one action wants every hop, so the hop is not part
// of the identity.
type Trace struct {
	Session string
	Intent  string
	Hop     int
}

// String renders the wire form: v1/<session>/<intent>/<hop>
func (t Trace) String() string {
	return "v1/" + t.Session + "/" + t.Intent + "/" + strconv.Itoa(t.Hop)
}

// Key identifies the user action, independent of which service saw it.
func (t Trace) Key() string { return t.Session + "/" + t.Intent }

// Valid reports whether the trace is well-formed.
func (t Trace) Valid() bool {
	return validID(t.Session) && validID(t.Intent) && t.Hop >= 0 && t.Hop <= maxHop
}

// Next returns the trace to send to a downstream service.
func (t Trace) Next() Trace {
	t.Hop++
	return t
}

// ParseTrace reads a header value.
//
// Returns ok=false rather than an error: a malformed header is ignored, never
// rejected. ADT must never fail a customer's request, and a request carrying a
// mangled correlation id is still a request the user wants served.
func ParseTrace(value string) (Trace, bool) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "v1/") {
		return Trace{}, false
	}

	parts := strings.Split(value[3:], "/")
	if len(parts) != 3 {
		return Trace{}, false
	}

	hop, err := strconv.Atoi(parts[2])
	if err != nil {
		return Trace{}, false
	}

	t := Trace{Session: parts[0], Intent: parts[1], Hop: hop}
	if !t.Valid() {
		return Trace{}, false
	}
	return t, true
}

func validID(s string) bool {
	if len(s) == 0 || len(s) > idMaxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

type traceKey struct{}

// WithTrace returns a context carrying t.
func WithTrace(ctx context.Context, t Trace) context.Context {
	return context.WithValue(ctx, traceKey{}, t)
}

// FromContext returns the trace bound to ctx, if any.
func FromContext(ctx context.Context) (Trace, bool) {
	t, ok := ctx.Value(traceKey{}).(Trace)
	return t, ok
}

// TraceFromRequest reads the trace off an inbound request.
func TraceFromRequest(r *http.Request) (Trace, bool) {
	return ParseTrace(r.Header.Get(Header))
}
