package devbench

// Before Go 1.24, debug.ReadBuildInfo in a `go test` binary reports no main
// module, so relFile could not relativize anything and every path test
// failed on 1.22/1.23. Real binaries always carry it: a Go 1.22 program
// built against this SDK reports "internal/deals/deals.go" exactly as 1.25
// does (checked 2026-10-05). Give the test binary what a real one has.
func init() {
	if mainModule == "" {
		mainModule = "github.com/pasperry/devbench-sdk/go"
	}
}
