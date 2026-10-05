package devbenchgin_test

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	devbench "github.com/pasperry/devbench-sdk/go"
	devbenchgin "github.com/pasperry/devbench-sdk/go/gin"
)

// A gin handler's slog lines — logged with c.Request.Context() or with c
// itself — are captured under the request's trace and delivered, redacted,
// when the stand-in ingest asks for that trace.
func TestGin_SlogLinesAreCapturedUnderTheRequestsTrace(t *testing.T) {
	in := newIngest(t)
	in.mu.Lock()
	in.needLogs = []string{"sessG/actG", "sessNoLines/act1"}
	in.mu.Unlock()

	var appOut bytes.Buffer
	logger := slog.New(devbench.NewLogHandler(slog.NewTextHandler(&appOut, nil)))

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(gin.RecoveryWithWriter(&bytes.Buffer{}), devbenchgin.Middleware())
	r.POST("/charge/:id", func(c *gin.Context) {
		logger.InfoContext(c.Request.Context(), "charging card for pat@example.com", "deal", c.Param("id"))
		logger.WarnContext(c, "gateway declined the charge", "status", 402)
		devbench.ReportHandled(c.Request.Context(), errors.New("declined"), "charge.Create")
		c.Status(http.StatusPaymentRequired)
	})

	req := httptest.NewRequest(http.MethodPost, "/charge/9912", nil)
	req.Header.Set(devbench.Header, "v1/sessG/actG/0")
	r.ServeHTTP(httptest.NewRecorder(), req)
	// An untraced request: logged, never captured.
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/charge/1", nil))
	closeSDK(t)

	in.mu.Lock()
	defer in.mu.Unlock()
	lines := in.slices["sessG/actG"]
	if len(lines) != 2 {
		t.Fatalf("delivered %d lines for the request's trace, want 2: %q (all: %v)", len(lines), lines, in.slices)
	}
	if !strings.Contains(lines[0], "charging card for") || !strings.Contains(lines[1], "gateway declined the charge") {
		t.Errorf("lines = %q", lines)
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "[v1/sessG/actG/0] ") {
			t.Errorf("line not led by its trace: %q", l)
		}
		if strings.Contains(l, "pat@example.com") || strings.Contains(l, "9912") {
			t.Errorf("PII delivered: %q", l)
		}
	}
	if _, ok := in.slices["sessNoLines/act1"]; ok || len(in.slices) != 1 {
		t.Errorf("slices for keys with no lines: %v", in.slices)
	}
	// The application's own log output is exactly what it logged.
	if !strings.Contains(appOut.String(), "pat@example.com") || strings.Count(appOut.String(), "\n") != 4 {
		t.Errorf("the application's log output changed:\n%s", appOut.String())
	}
}
