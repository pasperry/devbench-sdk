// Package devbenchgin connects the Dev Bench Go SDK to gin.
//
//	r := gin.New()
//	r.Use(gin.Logger(), gin.Recovery(), devbenchgin.Middleware())
//
// Register it after gin.Recovery: a panic in a handler is reported (type,
// message, frames, the gin route as its symbol, the request's user) and then
// re-panicked, so gin's Recovery still answers 500 exactly as before. Dev
// Bench observes; it never swallows.
//
// The request's trace is bound to c.Request.Context() (and to c itself), so
// with the core SDK's slog handler installed, lines a handler logs with
// either context are captured under the request's trace in direct mode:
//
//	slog.SetDefault(slog.New(devbench.NewLogHandler(slog.Default().Handler())))
//	slog.InfoContext(c.Request.Context(), "charging card")  // or slog.InfoContext(c, ...)
//
// Everything else is the core SDK's: trace propagation, devbench.WithUser,
// devbench.ReportHandled (and the x-adt-handled response header it drives),
// devbench.CaptureException, Init, Close.
package devbenchgin

import (
	"net/http"

	"github.com/gin-gonic/gin"
	devbench "github.com/pasperry/devbench-sdk/go"
)

// Middleware returns the gin handler. It wraps the rest of the chain in
// devbench.Middleware(devbench.Handled(...)), so gin requests get exactly
// the net/http behaviour.
func Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		inner := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			// The request now carries the SDK's context (trace, scope,
			// handled counter); handlers must see it through c.Request.
			c.Request = req
			// So log calls given the gin.Context itself are keyed too:
			// gin.Context.Value only reads c.Keys for a string key, and
			// reaches the request's context only with ContextWithFallback.
			if t, ok := devbench.FromContext(req.Context()); ok {
				c.Set(devbench.TraceContextKey, t)
			}
			// Responses go through Handled's writer so the handled count is
			// stamped before the status line, whichever gin method writes.
			orig := c.Writer
			c.Writer = &handledWriter{ResponseWriter: orig, h: w}
			defer func() { c.Writer = orig }()
			c.Next()
		})

		// gin routes without ServeMux, so the SDK's route symbol (a
		// ServeMux pattern) is given gin's: "PUT /deals/:id". A shallow
		// copy: gin's own request is not modified.
		req := c.Request.WithContext(c.Request.Context())
		if route := c.FullPath(); route != "" {
			setPattern(req, c.Request.Method+" "+route)
		}
		devbench.Middleware(devbench.Handled(inner)).ServeHTTP(c.Writer, req)
	}
}

// handledWriter routes gin's writes through the http.ResponseWriter that
// devbench.Handled passed down, which stamps x-adt-handled at WriteHeader
// time and forwards to gin's own writer. Everything else is gin's.
type handledWriter struct {
	gin.ResponseWriter
	h http.ResponseWriter
}

func (w *handledWriter) WriteHeader(code int) { w.h.WriteHeader(code) }

func (w *handledWriter) Write(p []byte) (int, error) { return w.h.Write(p) }

func (w *handledWriter) WriteString(s string) (int, error) { return w.h.Write([]byte(s)) }

// WriteHeaderNow is how gin commits headers for a body-less response
// (c.Status, c.AbortWithStatus).
func (w *handledWriter) WriteHeaderNow() {
	if !w.ResponseWriter.Written() {
		w.h.WriteHeader(w.ResponseWriter.Status())
	}
	w.ResponseWriter.WriteHeaderNow()
}
