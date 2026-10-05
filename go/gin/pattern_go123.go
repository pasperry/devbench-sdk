//go:build go1.23

package devbenchgin

import "net/http"

// setPattern records gin's route where the SDK reads a route (Go 1.23+).
func setPattern(r *http.Request, route string) { r.Pattern = route }
