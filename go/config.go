package devbench

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Configuration (SERVER_SDK_SPEC "Dev Bench naming and configuration").
//
// One DSN per environment carries everything the SDK needs to send:
//
//	DEVBENCH_DSN=https://<public key>:<secret key>@adt-ingest.onrender.com
//
// The public part is the environment's browser (`client`) key; the secret
// part is its `server` key, and is what this SDK authenticates with
// (DECISIONS #161). A DSN with a single key (`https://<key>@host`, the 0.5
// form) still works: that key is sent as before.
//
// With a DSN the SDK sends directly to ingest; without one it writes to the
// local sidecar's socket as it always has. Exactly one of the two, decided
// once: a sidecar on the same host still reads logs, but reports never go
// both ways, or every one would be counted twice.

// Environment variables the SDK reads. Options fields take precedence.
const (
	EnvDSN     = "DEVBENCH_DSN"
	EnvService = "DEVBENCH_SERVICE"
	EnvRelease = "DEVBENCH_RELEASE"
	EnvEnabled = "DEVBENCH_ENABLED"

	// envLegacyDSN is read when DEVBENCH_DSN is unset, for installs that
	// predate the rename.
	envLegacyDSN = "ADT_DSN"
)

// releaseEnvFallbacks are the CI and platform variables that commonly carry
// the deployed commit, tried in order when DEVBENCH_RELEASE is unset.
var releaseEnvFallbacks = []string{"GIT_SHA", "SOURCE_VERSION", "RENDER_GIT_COMMIT"}

// Options configures the SDK. Every field is optional: an empty field falls
// back to its environment variable, then to a default.
type Options struct {
	// DSN is https://<public>:<secret>@<ingest host> (or the 0.5 form,
	// https://<key>@<ingest host>). Empty means DEVBENCH_DSN,
	// then ADT_DSN; with none of them, reports go to the local sidecar.
	DSN string
	// Service names this application. Default: DEVBENCH_SERVICE, then the
	// last element of the main module's path, then "app".
	Service string
	// Release identifies the deployed build. Default: DEVBENCH_RELEASE,
	// GIT_SHA, SOURCE_VERSION, RENDER_GIT_COMMIT, the vcs.revision Go
	// stamped into the binary, else "".
	Release string
	// Disabled turns reporting off entirely, like DEVBENCH_ENABLED=false.
	Disabled bool
}

type mode int

const (
	modeSidecar mode = iota
	modeDirect
	modeOff
)

func (m mode) String() string {
	switch m {
	case modeDirect:
		return "direct"
	case modeOff:
		return "off"
	default:
		return "sidecar"
	}
}

// config is resolved Options: immutable once built.
type config struct {
	mode    mode
	base    string // scheme://host[:port] of ingest, direct mode only
	key     string // sent as X-ADT-Key: the DSN's secret, else its only key
	service string
	release string
	// problem says why reporting is off, for Test to return.
	problem string
}

// cfg is the process's resolved configuration. nil until Init or the first
// report, which resolves it from the environment.
var cfg atomic.Pointer[config]

// cfgMu serializes Init against lazy resolution so a report racing Init
// cannot install an environment-only config over the explicit one.
var cfgMu sync.Mutex

// Init configures the SDK. Optional: without it, the first report
// configures itself from the environment.
//
//	devbench.Init(devbench.Options{Service: "billing"}) // DSN from DEVBENCH_DSN
//
// An invalid DSN is logged once and turns reporting off; it never panics and
// never fails the application. The returned error says the same thing, for
// callers that want it.
//
// Calling Init again replaces the configuration; reports already counted
// under the previous one are flushed first, bounded to two seconds.
func Init(opts Options) error {
	c := resolve(opts)

	cfgMu.Lock()
	prev := cfg.Swap(c)
	cfgMu.Unlock()

	if prev != nil {
		ctx, cancel := context.WithTimeout(context.Background(), exitFlushBound)
		_ = closeTransports(ctx)
		cancel()
	}

	if c.mode == modeOff && c.problem != "" {
		return errors.New(c.problem)
	}
	return nil
}

// activeConfig returns the configuration, resolving it from the environment
// on first use.
func activeConfig() *config {
	if c := cfg.Load(); c != nil {
		return c
	}
	cfgMu.Lock()
	defer cfgMu.Unlock()
	if c := cfg.Load(); c != nil {
		return c
	}
	c := resolve(Options{})
	cfg.Store(c)
	return c
}

// resetConfig forgets the configuration so the next report re-reads the
// environment. For tests.
func resetConfig() {
	cfgMu.Lock()
	cfg.Store(nil)
	cfgMu.Unlock()
}

// resolve turns Options plus the environment into a config. Never fails:
// a problem becomes modeOff with a reason, logged once.
func resolve(opts Options) *config {
	c := &config{
		service: firstNonEmpty(opts.Service, os.Getenv(EnvService), defaultService()),
		release: firstNonEmpty(opts.Release, os.Getenv(EnvRelease), envRelease(), vcsRevision()),
	}

	if opts.Disabled || isFalse(os.Getenv(EnvEnabled)) {
		c.mode = modeOff
		c.problem = "devbench: reporting is disabled (" + EnvEnabled + "=false or Options.Disabled)"
		return c
	}

	dsn := firstNonEmpty(opts.DSN, os.Getenv(EnvDSN), os.Getenv(envLegacyDSN))
	if dsn == "" {
		c.mode = modeSidecar
		return c
	}

	base, key, err := parseDSN(dsn)
	if err != nil {
		c.mode = modeOff
		c.problem = "devbench: invalid DSN, reporting disabled: " + err.Error()
		logOnce(c.problem)
		return c
	}
	c.mode = modeDirect
	c.base = base
	c.key = key
	return c
}

// parseDSN splits a DSN into the ingest base URL and the key to send:
//
//	https://<public>:<secret>@host[:port]  -> <secret>  (DECISIONS #161)
//	https://<key>@host[:port]              -> <key>     (the 0.5 form)
//
// The public part of a pair is the browser's key; a server never sends it.
// A path or query is ignored. The error never quotes the DSN or either key:
// they are credentials, and this message goes to the application's log.
func parseDSN(dsn string) (base, key string, err error) {
	u, err := url.Parse(strings.TrimSpace(dsn))
	if err != nil {
		return "", "", errors.New("not a URL")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", "", errors.New("scheme must be https (or http)")
	}
	if u.Host == "" || u.Hostname() == "" {
		return "", "", errors.New("no host")
	}
	// The key is a server secret. Over plain http it crosses the network
	// readable by anything on the path, so http is for a loopback ingest
	// only (local development and tests) — as the Ruby gem enforces.
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		return "", "", errors.New("http:// is accepted only for a loopback ingest (the key would cross the network in plaintext); use https://")
	}
	if u.User == nil || u.User.Username() == "" {
		return "", "", errors.New("no ingest key before the @")
	}
	base = u.Scheme + "://" + u.Host
	if pass, hasPass := u.User.Password(); hasPass {
		if strings.TrimSpace(pass) == "" {
			// "https://pub:@host" is a pasting mistake. Falling back to the
			// public key would send server reports as a browser.
			return "", "", errors.New("the server key after the ':' is empty")
		}
		return base, pass, nil
	}
	return base, u.User.Username(), nil
}

// isLoopback: localhost, *.localhost, or a loopback IP (127.0.0.0/8, ::1).
func isLoopback(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// defaultService is the last element of the main module's path, skipping a
// major-version suffix: github.com/acme/billing/v2 -> "billing". "app" when
// the binary carries no module information.
func defaultService() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok || bi == nil {
		return "app"
	}
	return serviceFromModule(bi.Main.Path)
}

func serviceFromModule(path string) string {
	path = strings.Trim(path, "/")
	if path == "" || path == "command-line-arguments" {
		return "app"
	}
	parts := strings.Split(path, "/")
	last := parts[len(parts)-1]
	if len(parts) > 1 && isMajorVersion(last) {
		last = parts[len(parts)-2]
	}
	if last == "" {
		return "app"
	}
	return last
}

func isMajorVersion(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func envRelease() string {
	for _, name := range releaseEnvFallbacks {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

// vcsRevision is the commit `go build` stamped into the binary, when it was
// built inside a checkout.
func vcsRevision() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok || bi == nil {
		return ""
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return ""
}

func isFalse(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "false", "0", "no", "off":
		return true
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// logOnce writes msg to the standard logger the first time it is seen.
// A misconfiguration is worth one line, not one per report.
var logged sync.Map

func logOnce(msg string) {
	if _, seen := logged.LoadOrStore(msg, true); seen {
		return
	}
	log.Print(msg)
}

// exitFlushBound bounds the final flush at shutdown (spec: 2 s).
const exitFlushBound = 2 * time.Second

// closeTransports drains and stops whichever transports have been started.
func closeTransports(ctx context.Context) error {
	errDirect := closeDirect(ctx)
	errSocket := closeSocket(ctx)
	if errDirect != nil {
		return errDirect
	}
	return errSocket
}

// Close flushes everything counted or queued and stops the background
// goroutine, waiting until it is done or ctx ends, whichever is first. In
// direct mode the final flush is also bounded to two seconds.
//
// Call it from graceful shutdown:
//
//	defer devbench.Close(context.Background())
//
// Optional — without it, at most the last minute's counts are lost at exit —
// and never a trap: a report made after Close starts a new transport with
// the same configuration.
func Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	return closeTransports(ctx)
}

// Flush sends what has been counted so far without stopping anything. In
// sidecar mode reports are already streamed, and Flush returns at once.
func Flush(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if d := curDirect.Load(); d != nil {
		return d.flushNow(ctx)
	}
	return nil
}

// Mode reports the transport in use: "direct", "sidecar" or "off".
func Mode() string { return activeConfig().mode.String() }

// errorf is fmt.Errorf with the package prefix.
func errorf(format string, args ...any) error {
	return fmt.Errorf("devbench: "+format, args...)
}
