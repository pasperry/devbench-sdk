//go:build !go1.23

package devbench

import "net/http"

// routePattern: http.Request.Pattern arrived in Go 1.23. Older toolchains get
// no symbol rather than a raw path, which carries ids and would split one
// failure into one group per record. This is the SDK's only 1.23 dependency;
// keeping it behind a build tag lets the module declare go 1.21, so adding
// the SDK never forces a consumer's toolchain or vet rules forward.
func routePattern(*http.Request) string { return "" }
