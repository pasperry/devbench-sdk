package devbench

import (
	"context"
	"net/http"
)

// Middleware accepts the correlation id, binds it to the request context, and
// reports panics.
//
// The trace bound to r.Context() is what keys in-process log capture: a
// record logged with that context (slog.InfoContext(r.Context(), ...))
// through NewLogHandler is kept under the request's trace. A request with
// no (or a malformed) x-adt-trace header has no trace and nothing is kept
// for it — no failure Dev Bench triages can ever ask for its lines.
//
// It never rejects a request and never alters the response. A diagnostics
// middleware that can fail a request is worse than no diagnostics.
//
// A panic in the handler is recovered, sent to the sidecar as an `exception`
// (context "request": type, message, route pattern, frames from the panic
// site, trace, user), and then re-panicked with the same value, so whatever
// the application or net/http does with a panic still happens exactly as
// before. ADT observes; it never swallows. http.ErrAbortHandler is net/http's
// control-flow panic and is re-panicked without a report. To answer the
// panic with a 500 rather than net/http's empty reply, wrap Recover around
// this.
//
// Only the request's own goroutine is covered: a panic in a goroutine the
// handler starts is outside any middleware's reach and still crashes the
// process, as it would without ADT.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if trace, ok := TraceFromRequest(r); ok {
			ctx = WithTrace(ctx, trace)
		}
		ctx = context.WithValue(ctx, scopeKey{}, &requestScope{})
		r = r.WithContext(ctx)
		noteRequest(r)

		defer func() {
			v := recover()
			if v == nil {
				// Since Go 1.21 panic(nil) recovers as *runtime.PanicNilError,
				// so nil here means there was no panic.
				return
			}
			if v != any(http.ErrAbortHandler) {
				reportPanic(ctx, v)
			}
			panic(v)
		}()

		next.ServeHTTP(w, r)
	})
}
