//go:build !go1.23

package devbenchgin_test

// No route symbol before Go 1.23 (see pattern_legacy.go).
const wantPanicTemplate = "*errors.errorString"
