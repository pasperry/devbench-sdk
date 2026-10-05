package devbenchgin_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	devbench "github.com/pasperry/devbench-sdk/go"
	devbenchgin "github.com/pasperry/devbench-sdk/go/gin"
)

// Real gin, real HTTP, and the real SDK in direct mode against a real HTTP
// server standing in for ingest. Assertions are on what reached it.

type ingest struct {
	srv     *httptest.Server
	mu      sync.Mutex
	counts  []map[string]any
	bundles []map[string]any
}

func newIngest(t *testing.T) *ingest {
	t.Helper()
	in := &ingest{}
	asked := map[string]bool{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/flush", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Counts []map[string]any `json:"counts"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var reply struct {
			NeedEvidence []map[string]any `json:"need_evidence"`
		}
		in.mu.Lock()
		for _, c := range body.Counts {
			in.counts = append(in.counts, c)
			fp, _ := c["fp"].(string)
			if !asked[fp] {
				asked[fp] = true
				reply.NeedEvidence = append(reply.NeedEvidence, map[string]any{"fp": fp, "url": in.srv.URL + "/put/" + fp})
			}
		}
		in.mu.Unlock()
		_ = json.NewEncoder(w).Encode(reply)
	})
	mux.HandleFunc("/put/", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		in.mu.Lock()
		in.bundles = append(in.bundles, b)
		in.mu.Unlock()
	})
	in.srv = httptest.NewServer(mux)
	t.Cleanup(in.srv.Close)

	for _, k := range []string{"DEVBENCH_DSN", "ADT_DSN", "DEVBENCH_ENABLED"} {
		t.Setenv(k, "")
	}
	dsn := strings.Replace(in.srv.URL, "://", "://gin-key@", 1)
	if err := devbench.Init(devbench.Options{DSN: dsn, Service: "gin-app"}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = devbench.Close(context.Background()) })
	return in
}

func closeSDK(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := devbench.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func router() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(gin.RecoveryWithWriter(io.Discard), devbenchgin.Middleware())
	r.Use(func(c *gin.Context) { // the app's auth middleware names the user
		ctx := devbench.WithUser(c.Request.Context(), devbench.User{Email: "Pat@Example.com", Account: "acct-1182"})
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	r.GET("/deals/:id", func(c *gin.Context) {
		if c.Param("id") == "boom" {
			panic(errors.New("deal vanished"))
		}
		c.JSON(http.StatusOK, gin.H{"id": c.Param("id")})
	})
	r.POST("/deals/:id/save", func(c *gin.Context) {
		devbench.ReportHandled(c.Request.Context(), errors.New("stale price"), "deals.Save")
		c.String(http.StatusOK, "saved")
	})
	r.DELETE("/deals/:id", func(c *gin.Context) {
		devbench.ReportHandled(c.Request.Context(), errors.New("already gone"), "deals.Delete")
		c.Status(http.StatusNoContent)
	})
	return r
}

func do(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestGin_PanicIsReportedAndRecoveryStillAnswers500(t *testing.T) {
	in := newIngest(t)
	r := router()

	rec := do(t, r, http.MethodGet, "/deals/boom")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want gin Recovery's 500", rec.Code)
	}
	closeSDK(t)

	in.mu.Lock()
	defer in.mu.Unlock()
	if len(in.counts) != 1 {
		t.Fatalf("counts = %v, want exactly the panic", in.counts)
	}
	c := in.counts[0]
	users, _ := c["users"].([]any)
	if c["kind"] != "error" || len(users) != 1 {
		t.Fatalf("count = %v", c)
	}
	if u := users[0].(map[string]any); u["email"] != "pat@example.com" || u["account"] != "acct-1182" {
		t.Errorf("user = %v", u)
	}
	if len(in.bundles) != 1 {
		t.Fatalf("bundles = %v", in.bundles)
	}
	b := in.bundles[0]
	if b["template"] != wantPanicTemplate {
		t.Errorf("template = %q, want %q (gin's route as the symbol)", b["template"], wantPanicTemplate)
	}
	frames, _ := b["frames"].([]any)
	if len(frames) == 0 || !strings.Contains(frames[0].(map[string]any)["function"].(string), "router.func") {
		t.Errorf("innermost frame should be the gin handler: %v", frames)
	}
	for _, f := range frames {
		if strings.Contains(f.(map[string]any)["function"].(string), "devbench-sdk/go/gin.") {
			t.Errorf("the adapter's own frame was reported: %v", f)
		}
	}
}

func TestGin_NormalRoutesAreUnaffected(t *testing.T) {
	in := newIngest(t)
	r := router()

	rec := do(t, r, http.MethodGet, "/deals/42")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"id":"42"}` {
		t.Errorf("response = %d %q", rec.Code, rec.Body.String())
	}
	if rec.Result().Header.Get(devbench.HandledHeader) != "" {
		t.Error("a clean request carried the handled header")
	}
	if do(t, r, http.MethodGet, "/nowhere").Code != http.StatusNotFound {
		t.Error("gin's 404 changed")
	}
	closeSDK(t)

	in.mu.Lock()
	defer in.mu.Unlock()
	if len(in.counts) != 0 {
		t.Errorf("clean requests were reported: %v", in.counts)
	}
}

// ReportHandled under gin counts as a handled failure and stamps the header
// the browser reads, whether gin writes a body or only a status.
func TestGin_ReportHandledSetsTheHeaderAndIsCounted(t *testing.T) {
	in := newIngest(t)
	r := router()

	rec := do(t, r, http.MethodPost, "/deals/42/save")
	if rec.Code != http.StatusOK || rec.Body.String() != "saved" {
		t.Errorf("response = %d %q", rec.Code, rec.Body.String())
	}
	if got := rec.Result().Header.Get(devbench.HandledHeader); got != "1" {
		t.Errorf("%s = %q on a body response, want 1", devbench.HandledHeader, got)
	}
	rec = do(t, r, http.MethodDelete, "/deals/42")
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d", rec.Code)
	}
	if got := rec.Result().Header.Get(devbench.HandledHeader); got != "1" {
		t.Errorf("%s = %q on a status-only response, want 1", devbench.HandledHeader, got)
	}
	closeSDK(t)

	in.mu.Lock()
	defer in.mu.Unlock()
	if len(in.counts) != 2 {
		t.Fatalf("counts = %v, want two handled failures", in.counts)
	}
	for _, c := range in.counts {
		if c["kind"] != "handled_failure" {
			t.Errorf("count = %v", c)
		}
	}
}
