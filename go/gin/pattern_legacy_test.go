//go:build !go1.23

package devbenchgin_test

// No route symbol before Go 1.23 (see pattern_legacy.go).
const wantPanicTemplate = "*errors.errorString"

// ginRoute: before Go 1.23 gin's route cannot reach the SDK, so request
// signals report "*" (no route), never the raw path.
func ginRoute(string) string { return "*" }
