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
// Once the handler has returned (without panicking), the request is
// classified as docs/server-sdk/failed-and-slow-requests.md (Capability 2b) says: a 5xx with no exception
// reported during the request is a `failed` request, one that took more
// than 3 s (not streaming) a `slow` one, and a route's 429s feed the
// per-process spike rule. The route is the matched pattern without its
// method ("/deals/{id}"), never the raw path; "*" when none matched.
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
		// An inner Middleware (the gin adapter's, under an outer one) shares
		// the outer request's scope, so the request is observed once and an
		// exception captured anywhere inside counts for it.
		outer := scopeFrom(ctx) == nil
		if trace, ok := TraceFromRequest(r); ok {
			ctx = WithTrace(ctx, trace)
		}
		scope := scopeFrom(ctx)
		if outer {
			scope = &requestScope{}
			ctx = context.WithValue(ctx, scopeKey{}, scope)
		}
		r = r.WithContext(ctx)
		noteRequest(r)
		noteMux(r, next)

		// Request signals (Capability 2b): only the outermost Middleware
		// observes, and nothing at all with reporting off.
		var sw *statusWriter
		if outer && activeConfig().mode != modeOff {
			sw = &statusWriter{ResponseWriter: w}
			w = sw
		}
		start := requestClock()

		defer func() {
			v := recover()
			if v == nil {
				// Since Go 1.21 panic(nil) recovers as *runtime.PanicNilError,
				// so nil here means there was no panic. A request that raised
				// out of here is not observed: the exception is its report.
				if sw != nil {
					observeRequest(ctx, scope, sw, r, start, requestClock())
				}
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
