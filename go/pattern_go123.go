//go:build go1.23

package devbench

import "net/http"

// routePattern is the ServeMux pattern that matched (Go 1.23+), e.g.
// "PUT /deals/{id}" — never the raw path.
func routePattern(r *http.Request) string { return r.Pattern }

// muxPattern is unused from Go 1.23: Request.Pattern carries the pattern.
func muxPattern(http.Handler, *http.Request) string { return "" }
