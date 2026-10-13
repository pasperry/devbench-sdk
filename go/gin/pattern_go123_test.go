//go:build go1.23

package devbenchgin_test

const wantPanicTemplate = "*errors.errorString at GET /deals/:id"

// ginRoute is the route a request signal reports for a gin route.
func ginRoute(route string) string { return route }
