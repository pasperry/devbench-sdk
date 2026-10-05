//go:build !go1.23

package devbench

// Before Go 1.23 there is no Request.Pattern: the symbol is omitted, never
// replaced by a raw path.
func wantSymbol(string) any { return nil }
