package devbench

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Test helpers for the control socket. Everything here is real: a real unix
// listener on a real path, reading the bytes the reporter actually wrote. No
// part of the reporter is replaced.

var socketSeq atomic.Int64

// socketPath returns a fresh short path. t.TempDir is too long on macOS,
// where a unix socket path is limited to ~104 bytes.
func socketPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join("/tmp", fmt.Sprintf("adt-go-%d-%d.sock", os.Getpid(), socketSeq.Add(1)))
	_ = os.Remove(p)
	t.Cleanup(func() { _ = os.Remove(p) })
	return p
}

// useSocket points the reporter at path for this test, resetting any reporter
// a previous test left behind, and closes it (flushing) at the end.
func useSocket(t *testing.T, path string) {
	t.Helper()
	closeReporter(t)
	sidecarMode(t)
	t.Setenv(SocketEnv, path)
	t.Cleanup(func() { closeReporter(t) })
}

// sidecarMode makes this test resolve to the sidecar transport whatever the
// environment says: no DSN, not disabled.
func sidecarMode(t *testing.T) {
	t.Helper()
	t.Setenv(EnvDSN, "")
	t.Setenv(envLegacyDSN, "")
	t.Setenv(EnvEnabled, "")
	resetConfig()
	t.Cleanup(resetConfig)
}

func closeReporter(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// sidecar is a real listener that decodes every line it receives.
type sidecar struct {
	path  string
	ln    net.Listener
	lines chan map[string]any
	raw   chan string
}

// listenSidecar starts a listener at a fresh path and points the reporter at
// it.
func listenSidecar(t *testing.T) *sidecar {
	t.Helper()
	path := socketPath(t)
	s := startSidecarAt(t, path)
	useSocket(t, path)
	return s
}

func startSidecarAt(t *testing.T, path string) *sidecar {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	s := &sidecar{path: path, ln: ln, lines: make(chan map[string]any, 100000), raw: make(chan string, 100000)}

	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				sc := bufio.NewScanner(conn)
				sc.Buffer(make([]byte, 0, 4096), 1<<20)
				for sc.Scan() {
					s.raw <- sc.Text()
					var m map[string]any
					if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
						m = map[string]any{"_malformed": sc.Text()}
					}
					s.lines <- m
				}
			}()
		}
	}()
	return s
}

// next waits for the next line.
func (s *sidecar) next(t *testing.T) map[string]any {
	t.Helper()
	select {
	case m := <-s.lines:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("no message reached the sidecar within 5s")
		return nil
	}
}

// expectNone asserts that nothing arrives within d.
func (s *sidecar) expectNone(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case m := <-s.lines:
		t.Fatalf("unexpected message reached the sidecar: %v", m)
	case <-time.After(d):
	}
}

// stalledSidecar accepts connections and never reads from them: a wedged
// sidecar whose socket buffers fill and whose writes block.
func stalledSidecar(t *testing.T) string {
	t.Helper()
	path := socketPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
	})
	return path
}
