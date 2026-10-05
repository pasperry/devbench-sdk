//go:build go1.23

package devbench

// wantSymbol is the symbol a request with this ServeMux pattern reports.
func wantSymbol(pattern string) any { return pattern }
