package devbench

import (
	"context"
	"net/http"
	"sync/atomic"
)

// HandledHeader tells the browser that a failure was handled while producing
// this response.
//
// Detector 2's entire wire surface: one count, no detail.
//
// The browser already knows whether the application showed the user an error.
// It cannot know whether anything went wrong getting to that answer. This
// header closes the gap, and the join then happens in the browser — the one
// place that holds both facts at the same moment — rather than in a database
// that would need per-trace records from both sides.
const HandledHeader = "x-adt-handled"

// exposeHeader lets a cross-origin browser read HandledHeader at all.
//
// Easy to omit and silent when omitted, which is the worst combination: an
// Angular app on a different origin from its Go API reads nothing, the
// detector never fires, and the system looks like it is working.
const exposeHeader = "Access-Control-Expose-Headers"

// handledCounter is request-scoped, carried in the context.
type handledCounter struct{ n atomic.Int64 }

type handledKey struct{}

// WithHandledCounter attaches a counter to the context.
//
// Exported for tests and for anyone composing their own middleware; Handled
// installs it for the ordinary case.
func WithHandledCounter(ctx context.Context) context.Context {
	return context.WithValue(ctx, handledKey{}, &handledCounter{})
}

// ReportHandled records that a failure was handled here.
//
// Go has no rescue blocks, so unlike Rails there is nothing to pattern-match
// at runtime: a discarded error is an *absence*, visible only in the AST. The
// engineer drone finds those sites statically and inserts a call to this
// function where a site is genuinely handling a failure (§3.2). It is also
// perfectly reasonable to call by hand.
//
//	if err := charge(ctx, order); err != nil {
//	    devbench.ReportHandled(ctx, err, "billing.Charge")
//	    return fallbackReceipt(), nil   // <- the user is about to be told this worked
//	}
//
// Two things happen, independently:
//
//  1. The request's handled count goes up, which Handled turns into the
//     x-adt-handled response header. The browser is told a count and nothing
//     more, because that header is readable by any script on the page.
//  2. A handled_failure message — site, error type, message, trace, user —
//     is queued for the local sidecar (see reporter.go). The site and error
//     type are the fingerprint; the message is detail.
//
// Never blocks, never panics, and never returns an error: this runs on the
// customer's request path, inside the handling of a failure that already
// happened. The cost on the caller is err.Error() plus a channel send; I/O
// happens on a background goroutine, and with no sidecar the report is
// dropped.
func ReportHandled(ctx context.Context, err error, site string) {
	if err == nil || ctx == nil {
		return
	}

	// Counted first, and regardless of whether the report below works: the
	// header does not need the sidecar.
	if c, ok := ctx.Value(handledKey{}).(*handledCounter); ok {
		c.n.Add(1)
	}

	reportHandled(ctx, err, site)
}

func reportHandled(ctx context.Context, err error, site string) {
	defer func() { _ = recover() }()

	send(message{
		V:       1,
		Kind:    "handled_failure",
		Symbol:  truncateRunes(site, maxSymbol),
		Error:   errorType(err),
		Message: errorMessage(err),
		Trace:   traceField(ctx),
		User:    userField(ctx),
	})
}

// HandledCount reports how many failures were handled during this request.
func HandledCount(ctx context.Context) int64 {
	if c, ok := ctx.Value(handledKey{}).(*handledCounter); ok {
		return c.n.Load()
	}
	return 0
}

// Handled wraps a handler so responses carry the handled count.
//
// Separate from Middleware rather than folded into it: trace propagation is
// useful on its own and is the thing every service wants, while this changes
// response headers. Composing them is one line, and a customer who wants only
// one gets only one.
//
//	http.Handle("/", devbench.Middleware(devbench.Handled(mux)))
func Handled(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := WithHandledCounter(r.Context())

		// The header has to be set before the first write, and a handler can
		// report a handled failure at any point before that. So the count is
		// read at WriteHeader time, not after the handler returns — by then
		// the status line is long gone.
		rw := &handledWriter{ResponseWriter: w, ctx: ctx}
		r = r.WithContext(ctx)
		// The mux below records its route pattern on this copy, which is
		// the one Middleware must read if the request panics.
		noteRequest(r)
		next.ServeHTTP(rw, r)

		// A handler that never wrote anything still produces a 200 from
		// net/http's implicit write. Mark it now, while headers are unsent.
		rw.markIfUnwritten()
	})
}

// handledWriter stamps the count at WriteHeader time.
type handledWriter struct {
	http.ResponseWriter
	ctx     context.Context
	written bool
}

func (w *handledWriter) WriteHeader(status int) {
	w.mark()
	w.written = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *handledWriter) Write(p []byte) (int, error) {
	if !w.written {
		// net/http writes an implicit 200 here, which skips WriteHeader.
		w.mark()
		w.written = true
	}
	return w.ResponseWriter.Write(p)
}

func (w *handledWriter) markIfUnwritten() {
	if !w.written {
		w.mark()
	}
}

func (w *handledWriter) mark() {
	n := HandledCount(w.ctx)
	if n <= 0 {
		return
	}

	h := w.Header()
	h.Set(HandledHeader, itoa(n))

	// Append rather than overwrite: an application that exposes its own
	// headers should keep them.
	if existing := h.Get(exposeHeader); existing != "" {
		if !containsFold(existing, HandledHeader) {
			h.Set(exposeHeader, existing+", "+HandledHeader)
		}
	} else {
		h.Set(exposeHeader, HandledHeader)
	}
}

// itoa avoids pulling strconv into a file that needs one small conversion.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// containsFold is a case-insensitive substring check for ASCII header names.
func containsFold(haystack, needle string) bool {
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			a, b := haystack[i+j], needle[j]
			if 'A' <= a && a <= 'Z' {
				a += 'a' - 'A'
			}
			if 'A' <= b && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
