package devbenchgin_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	devbench "github.com/pasperry/devbench-sdk/go"
	devbenchgin "github.com/pasperry/devbench-sdk/go/gin"
)

// Capability 2b under gin: real gin on a real listener, the real SDK in
// direct mode against the real-HTTP ingest stand-in. A request signal's
// route is gin's FullPath ("/deals/:id"), never the raw path.

func signalRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(gin.RecoveryWithWriter(discard{}), devbenchgin.Middleware())
	r.GET("/deals/:id/fail", func(c *gin.Context) {
		c.Status(http.StatusServiceUnavailable) // gin commits it after the chain
	})
	r.GET("/deals/:id/changed-mind", func(c *gin.Context) {
		c.Status(http.StatusInternalServerError)
		c.JSON(http.StatusOK, gin.H{"ok": true}) // gin answers 200
	})
	r.GET("/deals/:id/captured", func(c *gin.Context) {
		devbench.CaptureException(c.Request.Context(), fmt.Errorf("ledger mismatch"))
		c.AbortWithStatus(http.StatusBadGateway)
	})
	r.GET("/deals/:id/boom", func(c *gin.Context) { panic("deal vanished") })
	r.GET("/deals/:id/slow", func(c *gin.Context) {
		time.Sleep(3100 * time.Millisecond)
		c.String(http.StatusOK, "late")
	})
	r.GET("/limited/:id", func(c *gin.Context) {
		c.AbortWithStatus(http.StatusTooManyRequests)
	})
	return r
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func TestGin_RequestSignals(t *testing.T) {
	in := newIngest(t)
	srv := httptest.NewServer(signalRouter())
	t.Cleanup(srv.Close)

	get := func(path string) int {
		res, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	for path, want := range map[string]int{
		"/deals/41/fail":         503,
		"/deals/42/changed-mind": 200,
		"/deals/43/captured":     502,
		"/deals/44/boom":         500,
		"/deals/45/slow":         200,
		"/nowhere":               404,
	} {
		if got := get(path); got != want {
			t.Errorf("GET %s = %d, want %d (the response must not change)", path, got, want)
		}
	}
	for i := 0; i < 10; i++ {
		if got := get(fmt.Sprintf("/limited/%d", i)); got != 429 {
			t.Fatalf("429 route answered %d", got)
		}
	}
	closeSDK(t) // process exit: the open spike window is evaluated

	in.mu.Lock()
	defer in.mu.Unlock()
	var templates []string
	for _, b := range in.bundles {
		templates = append(templates, fmt.Sprintf("%v %v", b["kind"], b["template"]))
		raw := fmt.Sprint(b)
		for _, id := range []string{"/deals/41", "/deals/45", "/limited/3"} {
			if strings.Contains(raw, id) {
				t.Errorf("a raw path reached evidence: %s", raw)
			}
		}
	}
	sort.Strings(templates)
	want := []string{
		"degradation slow_request at GET " + ginRoute("/deals/:id/slow"),
		"error http_429_spike at GET " + ginRoute("/limited/:id"),
		"error http_503 at GET " + ginRoute("/deals/:id/fail"),
	}
	var reqTemplates []string
	exceptions := 0
	for _, s := range templates {
		if strings.Contains(s, "http_") || strings.Contains(s, "slow_request") {
			reqTemplates = append(reqTemplates, s)
		} else {
			exceptions++
		}
	}
	if fmt.Sprint(reqTemplates) != fmt.Sprint(want) {
		t.Errorf("request signals = %q\nwant              %q", reqTemplates, want)
	}
	if exceptions != 2 {
		t.Errorf("exceptions = %d, want the capture and the panic only: %q", exceptions, templates)
	}
	for _, c := range in.counts {
		if c["kind"] == "error" && c["n"] == float64(1) {
			continue
		}
		if c["kind"] == "degradation" && c["n"] == float64(1) {
			continue
		}
		t.Errorf("count = %v", c)
	}
}
