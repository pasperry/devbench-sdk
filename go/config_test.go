package devbench

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseDSN(t *testing.T) {
	good := []struct{ dsn, base, key string }{
		{"https://k123@adt-ingest.onrender.com", "https://adt-ingest.onrender.com", "k123"},
		{"https://k123@ingest.example.com:8443/ignored/path?x=1", "https://ingest.example.com:8443", "k123"},
		{"http://k@127.0.0.1:9000", "http://127.0.0.1:9000", "k"},
		{"  https://k@h.example  ", "https://h.example", "k"},
	}
	for _, c := range good {
		base, key, err := parseDSN(c.dsn)
		if err != nil || base != c.base || key != c.key {
			t.Errorf("parseDSN(%q) = %q, %q, %v; want %q, %q", c.dsn, base, key, err, c.base, c.key)
		}
	}

	for _, bad := range []string{
		"adt-ingest.onrender.com",            // no scheme
		"https://adt-ingest.onrender.com",    // no key
		"ftp://k@adt-ingest.onrender.com",    // wrong scheme
		"https://k@",                         // no host
		"https://:secret@host.example",       // empty username
		"://k@host",                          // unparseable
		"https://k%zz@host.example/broken%%", // malformed escapes
	} {
		if _, _, err := parseDSN(bad); err == nil {
			t.Errorf("parseDSN(%q) accepted an invalid DSN", bad)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("parseDSN(%q) error quotes the credential: %v", bad, err)
		}
	}
}

func TestResolve_EnvironmentAndPrecedence(t *testing.T) {
	resetSDK(t)
	for _, k := range releaseEnvFallbacks {
		t.Setenv(k, "")
	}

	// Nothing set: sidecar, never direct.
	if c := resolve(Options{}); c.mode != modeSidecar {
		t.Errorf("no DSN: mode %v, want sidecar", c.mode)
	}

	// ADT_DSN is the fallback; DEVBENCH_DSN wins over it; Options win over both.
	t.Setenv(envLegacyDSN, "https://legacy@legacy.example")
	if c := resolve(Options{}); c.mode != modeDirect || c.key != "legacy" {
		t.Errorf("ADT_DSN: %+v", c)
	}
	t.Setenv(EnvDSN, "https://fresh@fresh.example")
	if c := resolve(Options{}); c.key != "fresh" || c.base != "https://fresh.example" {
		t.Errorf("DEVBENCH_DSN over ADT_DSN: %+v", c)
	}
	if c := resolve(Options{DSN: "https://opt@opt.example"}); c.key != "opt" {
		t.Errorf("Options.DSN over env: %+v", c)
	}

	t.Setenv(EnvService, "billing-env")
	t.Setenv(EnvRelease, "rel-env")
	if c := resolve(Options{}); c.service != "billing-env" || c.release != "rel-env" {
		t.Errorf("service/release from env: %+v", c)
	}
	if c := resolve(Options{Service: "svc", Release: "rel"}); c.service != "svc" || c.release != "rel" {
		t.Errorf("service/release from Options: %+v", c)
	}

	// Release falls back through the CI variables, in order.
	t.Setenv(EnvRelease, "")
	t.Setenv("RENDER_GIT_COMMIT", "render-sha")
	t.Setenv("SOURCE_VERSION", "heroku-sha")
	if c := resolve(Options{}); c.release != "heroku-sha" {
		t.Errorf("release = %q, want SOURCE_VERSION ahead of RENDER_GIT_COMMIT", c.release)
	}
	t.Setenv("GIT_SHA", "git-sha")
	if c := resolve(Options{}); c.release != "git-sha" {
		t.Errorf("release = %q, want GIT_SHA first", c.release)
	}

	// Disabled beats everything.
	for _, v := range []string{"false", "0", "FALSE", "off", "no"} {
		t.Setenv(EnvEnabled, v)
		if c := resolve(Options{}); c.mode != modeOff {
			t.Errorf("%s=%s: mode %v, want off", EnvEnabled, v, c.mode)
		}
	}
	t.Setenv(EnvEnabled, "true")
	if c := resolve(Options{Disabled: true}); c.mode != modeOff {
		t.Errorf("Options.Disabled: mode %v, want off", c.mode)
	}
}

func TestServiceFromModule(t *testing.T) {
	cases := map[string]string{
		"github.com/acme/billing":    "billing",
		"github.com/acme/billing/v2": "billing",
		"example.com/svc/v10":        "svc",
		"billing":                    "billing",
		"v2":                         "v2",
		"":                           "app",
		"command-line-arguments":     "app",
	}
	for in, want := range cases {
		if got := serviceFromModule(in); got != want {
			t.Errorf("serviceFromModule(%q) = %q, want %q", in, got, want)
		}
	}
}

// A broken DSN costs one log line and the reports — never a panic, never the
// application, and never a silent fallback to the sidecar.
func TestInit_InvalidDSNLogsOnceAndDisables(t *testing.T) {
	resetSDK(t)
	path := socketPath(t)
	s := startSidecarAt(t, path)
	t.Setenv(SocketEnv, path)

	var buf bytes.Buffer
	var mu sync.Mutex
	prev := log.Writer()
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))
	t.Cleanup(func() { log.SetOutput(prev) })
	logged = sync.Map{}

	err := Init(Options{DSN: "https://no-key-here.example"})
	if err == nil || !strings.Contains(err.Error(), "invalid DSN") {
		t.Errorf("Init = %v, want an invalid-DSN error", err)
	}
	_ = Init(Options{DSN: "https://no-key-here.example"})
	if Mode() != "off" {
		t.Errorf("Mode = %s, want off", Mode())
	}

	ReportHandled(context.Background(), errors.New("x"), "after.BadDSN")
	CaptureException(context.Background(), errors.New("y"))
	s.expectNone(t, 200*time.Millisecond)

	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if n := strings.Count(out, "invalid DSN"); n != 1 {
		t.Errorf("logged %d times, want once:\n%s", n, out)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// Without Init, the first report configures the SDK from the environment.
func TestLazyConfiguration_FromTheEnvironment(t *testing.T) {
	for _, name := range []string{EnvDSN, envLegacyDSN} {
		t.Run(name, func(t *testing.T) {
			f := newFakeIngest(t)
			resetSDK(t)
			t.Setenv(name, f.dsn("lazy-key"))
			t.Setenv(EnvService, "lazy-svc")

			ReportHandled(context.Background(), errors.New("x"), "lazy.Site")
			if Mode() != "direct" {
				t.Fatalf("Mode = %s, want direct", Mode())
			}
			closeNow(t)

			_, flushes, _ := f.snapshot()
			if len(flushes) != 1 || flushes[0].Key != "lazy-key" || flushes[0].Body.Service != "lazy-svc" {
				t.Fatalf("flushes = %+v", flushes)
			}
		})
	}
}

// DEVBENCH_ENABLED=false: nothing goes anywhere, whatever else is set.
func TestDisabled_SendsNothingAnywhere(t *testing.T) {
	f := newFakeIngest(t)
	resetSDK(t)
	path := socketPath(t)
	s := startSidecarAt(t, path)
	t.Setenv(SocketEnv, path)
	t.Setenv(EnvDSN, f.dsn("k"))
	t.Setenv(EnvEnabled, "false")

	ReportHandled(context.Background(), errors.New("x"), "disabled.Site")
	closeNow(t)

	if attempts, _, _ := f.snapshot(); attempts != 0 {
		t.Errorf("%d requests reached ingest while disabled", attempts)
	}
	s.expectNone(t, 200*time.Millisecond)
}

// Init after reports were counted under another configuration flushes those
// first, under the configuration they were counted with.
func TestInit_AgainFlushesThePreviousConfiguration(t *testing.T) {
	first := newFakeIngest(t)
	second := newFakeIngest(t)
	resetSDK(t)
	if err := Init(Options{DSN: first.dsn("one")}); err != nil {
		t.Fatal(err)
	}
	ReportHandled(context.Background(), errors.New("x"), "first.Site")
	if err := Init(Options{DSN: second.dsn("two")}); err != nil {
		t.Fatal(err)
	}
	ReportHandled(context.Background(), errors.New("x"), "second.Site")
	closeNow(t)

	if len(first.counts()) != 1 || len(second.counts()) != 1 {
		t.Errorf("first got %v, second got %v; want one each", first.counts(), second.counts())
	}
}
