package devbench

import (
	"bufio"
	"log"
	"net"
	"net/http"
	"runtime"
)

// Recover answers a panicking request with a 500 instead of the empty reply
// net/http gives it. Wrap it outermost, around Middleware:
//
//	http.ListenAndServe(":8080", devbench.Recover(devbench.Middleware(devbench.Handled(mux))))
//
// Middleware reports the panic and re-panics it; Recover is what stops it.
// They are separate because Middleware must never change what a request
// gets: under gin, gin.Recovery answers the 500 and devbenchgin uses
// Middleware alone. Recover reports nothing itself — without Middleware
// inside it, a panic becomes a 500 and nothing more.
//
// A recovered panic is logged as net/http logs one ("http: panic serving
// <addr>: <value>" and the stack, to the server's ErrorLog), so recovering
// costs the operator nothing they saw before. Then:
//
//   - nothing written yet: the client gets 500 Internal Server Error, with
//     none of the headers the handler had set for the response it never
//     sent;
//   - the response already started: the connection is aborted, as net/http
//     does, rather than a 500 appended to a half-sent response that would
//     read as a success.
//
// http.ErrAbortHandler is net/http's control-flow panic: re-panicked
// untouched, neither logged nor answered.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &recoverWriter{ResponseWriter: w}
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == any(http.ErrAbortHandler) {
				panic(v)
			}
			logPanic(r, v)
			if rw.started {
				panic(http.ErrAbortHandler)
			}
			// The handler's headers belonged to the response it never sent
			// (a Content-Encoding, a cookie, a cache policy); the 500 is a
			// different response.
			h := w.Header()
			for k := range h {
				delete(h, k)
			}
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}()
		next.ServeHTTP(rw, r)
	})
}

// logPanic is net/http's own panic log line, to the same logger.
func logPanic(r *http.Request, v any) {
	const size = 64 << 10
	buf := make([]byte, size)
	buf = buf[:runtime.Stack(buf, false)]
	logf := log.Printf
	if srv, ok := r.Context().Value(http.ServerContextKey).(*http.Server); ok && srv.ErrorLog != nil {
		logf = srv.ErrorLog.Printf
	}
	logf("http: panic serving %v: %v\n%s", r.RemoteAddr, v, buf)
}

// recoverWriter notes whether the response has started. Flush, Hijack and
// Unwrap pass through, so wrapping the whole application costs streaming
// and websocket handlers nothing.
type recoverWriter struct {
	http.ResponseWriter
	started bool
}

func (w *recoverWriter) WriteHeader(status int) {
	// 1xx informational headers do not start the response.
	if status >= 200 {
		w.started = true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *recoverWriter) Write(p []byte) (int, error) {
	w.started = true
	return w.ResponseWriter.Write(p)
}

func (w *recoverWriter) Flush() {
	w.started = true
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *recoverWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	conn, brw, err := h.Hijack()
	if err == nil {
		// The connection is the handler's now; nothing may be written to it.
		w.started = true
	}
	return conn, brw, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *recoverWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
