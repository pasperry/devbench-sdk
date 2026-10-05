package devbench

import (
	"bytes"
	"html"
	"net/http"
	"strings"
	"time"
)

// SessionOptions configures automatic session-token injection.
type SessionOptions struct {
	// Secret is the tenant's session signing secret.
	Secret string
	// Tenant is the tenant slug the token is bound to.
	Tenant string
	// TTL defaults to DefaultSessionTTL.
	TTL time.Duration
	// Subject derives an optional per-user identifier from the request. Leave
	// nil unless you want per-user attribution; the token works without it.
	Subject func(*http.Request) string
}

// WithSession wraps a handler so HTML responses carry a session token.
//
// Deliberately automatic. The customer already installs Middleware for trace
// propagation; making them also thread a token through every template would be
// real work in their codebase for something we can do here. The token is
// injected as a meta tag in <head>, which the browser SDK reads.
//
//	http.Handle("/", devbench.Middleware(devbench.WithSession(opts)(mux)))
//
// Only HTML responses are touched. JSON, images, and downloads pass through
// untouched and unbuffered.
func WithSession(opts SessionOptions) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if opts.Secret == "" || opts.Tenant == "" {
				next.ServeHTTP(w, r)
				return
			}

			subject := ""
			if opts.Subject != nil {
				subject = opts.Subject(r)
			}

			token, err := MintSession(opts.Secret, opts.Tenant, subject, opts.TTL)
			if err != nil {
				// Never fail a page over instrumentation.
				next.ServeHTTP(w, r)
				return
			}

			rec := &htmlInjector{ResponseWriter: w, token: token}
			next.ServeHTTP(rec, r)
			rec.flush()
		})
	}
}

// htmlInjector inserts the meta tag into an HTML response.
//
// Buffering is limited to HTML, and only until <head> is passed. A streaming
// JSON endpoint or a large download is written straight through: buffering
// someone's entire response to add a meta tag they will never read would be a
// poor trade in a library that runs inside their app.
type htmlInjector struct {
	http.ResponseWriter

	token    string
	buf      bytes.Buffer
	isHTML   bool
	decided  bool
	injected bool
	wrote    bool
}

func (h *htmlInjector) WriteHeader(status int) {
	h.decide()
	if h.isHTML {
		// Length changes once the tag is added, and a stale value truncates the
		// page in the browser.
		h.Header().Del("Content-Length")
	}
	h.wrote = true
	h.ResponseWriter.WriteHeader(status)
}

func (h *htmlInjector) Write(p []byte) (int, error) {
	h.decide()

	if !h.isHTML || h.injected {
		if !h.wrote {
			h.wrote = true
		}
		return h.ResponseWriter.Write(p)
	}

	h.buf.Write(p)

	// Inject as soon as <head> has been seen, then stop buffering entirely.
	if idx := headInsertPoint(h.buf.Bytes()); idx >= 0 {
		body := h.buf.Bytes()
		out := make([]byte, 0, len(body)+128)
		out = append(out, body[:idx]...)
		out = append(out, []byte(h.metaTag())...)
		out = append(out, body[idx:]...)

		h.buf.Reset()
		h.injected = true
		if _, err := h.ResponseWriter.Write(out); err != nil {
			return 0, err
		}
		return len(p), nil
	}

	// A document with no <head> in the first chunk: give up on injecting rather
	// than buffering the whole thing.
	if h.buf.Len() > 64<<10 {
		body := h.buf.Bytes()
		h.buf.Reset()
		h.injected = true
		if _, err := h.ResponseWriter.Write(body); err != nil {
			return 0, err
		}
	}

	return len(p), nil
}

// flush writes anything still buffered.
func (h *htmlInjector) flush() {
	if h.buf.Len() > 0 {
		_, _ = h.ResponseWriter.Write(h.buf.Bytes())
		h.buf.Reset()
	}
}

func (h *htmlInjector) decide() {
	if h.decided {
		return
	}
	h.decided = true
	h.isHTML = strings.Contains(strings.ToLower(h.Header().Get("Content-Type")), "text/html")
}

func (h *htmlInjector) metaTag() string {
	return `<meta name="` + SessionMetaName + `" content="` + html.EscapeString(h.token) + `">`
}

// headInsertPoint returns the offset just after <head...>, or -1.
func headInsertPoint(body []byte) int {
	lower := bytes.ToLower(body)
	idx := bytes.Index(lower, []byte("<head"))
	if idx < 0 {
		return -1
	}
	end := bytes.IndexByte(lower[idx:], '>')
	if end < 0 {
		return -1
	}
	return idx + end + 1
}
