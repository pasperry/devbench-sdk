package devbench

import (
	"context"
	"log/slog"
	"net/http"
)

// Middleware accepts the correlation id, binds it to the request context, and
// reports panics.
//
// It never rejects a request and never alters the response. A diagnostics
// middleware that can fail a request is worse than no diagnostics.
//
// A panic in the handler is recovered, sent to the sidecar as an `exception`
// (context "request": type, message, route pattern, frames from the panic
// site, trace, user), and then re-panicked with the same value, so whatever
// the application or net/http does with a panic still happens exactly as
// before. ADT observes; it never swallows. http.ErrAbortHandler is net/http's
// control-flow panic and is re-panicked without a report.
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

// LogHandler wraps a slog.Handler so every record carries the active trace.
//
// This is what makes the sidecar's index work: the trace has to reach the log
// line, or there is nothing to index by and client evidence cannot be joined
// to server logs (ARCHITECTURE_PROPOSAL.md risk #9).
type LogHandler struct {
	slog.Handler
}

// NewLogHandler wraps h.
func NewLogHandler(h slog.Handler) *LogHandler { return &LogHandler{Handler: h} }

func (h *LogHandler) Handle(ctx context.Context, r slog.Record) error {
	if t, ok := FromContext(ctx); ok {
		r.AddAttrs(slog.String("adt_trace", t.String()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h *LogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &LogHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *LogHandler) WithGroup(name string) slog.Handler {
	return &LogHandler{Handler: h.Handler.WithGroup(name)}
}
