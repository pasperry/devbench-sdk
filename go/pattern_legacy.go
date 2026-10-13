//go:build !go1.23

package devbench

import "net/http"

// routePattern: http.Request.Pattern arrived in Go 1.23. Older toolchains get
// no symbol rather than a raw path, which carries ids and would split one
// failure into one group per record. This is the SDK's only 1.23 dependency;
// keeping it behind a build tag lets the module declare go 1.21, so adding
// the SDK never forces a consumer's toolchain or vet rules forward.
func routePattern(*http.Request) string { return "" }

// muxPattern asks a *http.ServeMux which pattern r matches (Go 1.22's
// ServeMux already has method and wildcard patterns, and Handler returns
// the one that matched). Only for request signals' route, and only when
// Middleware or Handled wraps the mux directly; anything else has no
// pattern before Go 1.23.
func muxPattern(next http.Handler, r *http.Request) string {
	mux, ok := next.(*http.ServeMux)
	if !ok {
		return ""
	}
	_, p := mux.Handler(r)
	return p
}
