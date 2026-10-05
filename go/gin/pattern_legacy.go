//go:build !go1.23

package devbenchgin

import "net/http"

// setPattern: http.Request.Pattern arrived in Go 1.23, and the SDK reads
// its route symbol only from there. Older toolchains report no symbol — the
// same as the core SDK on 1.22.
func setPattern(*http.Request, string) {}
