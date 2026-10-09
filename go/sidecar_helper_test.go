package devbench

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
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
	return newStalledSidecar(t).path
}

// stalled is a wedged sidecar that counts the connections it accepted and can
// be released.
type stalled struct {
	path     string
	accepted atomic.Int64
	// release ends the stall: it closes the listener and every accepted
	// connection, so a write blocked on the sidecar fails at once instead of
	// waiting out its deadline. Safe to call more than once; it also runs at
	// cleanup.
	release func()
}

func newStalledSidecar(t *testing.T) *stalled {
	t.Helper()
	st := &stalled{path: socketPath(t)}
	ln, err := net.Listen("unix", st.path)
	if err != nil {
		t.Fatalf("listen %s: %v", st.path, err)
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
			st.accepted.Add(1)
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	var once sync.Once
	st.release = func() {
		once.Do(func() {
			_ = ln.Close()
			<-done
			mu.Lock()
			for _, c := range conns {
				_ = c.Close()
			}
			mu.Unlock()
		})
	}
	t.Cleanup(st.release)
	return st
}

// wedgedWriter points the reporter at a stalled sidecar and wedges its writer
// there before returning: the writer is mid-write on its one connection, and
// stays so for wedgedHold. Meanwhile nothing a caller does can reach the
// sidecar except by doing I/O itself, and a caller that waits for the writer
// waits until the hold ends — then the stall is released, so such a caller
// fails the test's time bound instead of hanging the test.
//
// The wedge is deterministic, not a timing guess: four plug reports of 240 KB
// are queued before the writer starts, so it takes them as one batch on one
// connection — about 1 MB, more than any default unix socket buffer (8 KB on
// macOS, ~208 KB on Linux) — and blocks writing it. The write deadline is
// widened to wedgedWriteTimeout (the plug's deadline is a multiple of it), so
// the block outlasts the hold; release ends it.
//
// The returned sidecar's accepted count is 1 (the writer's) when this
// returns. Any later connection was opened by someone other than the writer.
func wedgedWriter(t *testing.T) *stalled {
	t.Helper()
	st := newStalledSidecar(t)
	setWriteTimeout(t, wedgedWriteTimeout)
	useSocket(t, st.path)
	t.Cleanup(st.release) // before useSocket's Close: cleanups are LIFO

	r := getReporter()
	plug := strings.Repeat("p", 240<<10)
	for i := 0; i < 4; i++ {
		r.queue <- message{V: 1, Kind: "handled_failure", Symbol: "plug", Message: plug}
	}
	r.start.Do(func() { go r.run() })

	deadline := time.Now().Add(5 * time.Second)
	for st.accepted.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the writer never connected to the stalled sidecar")
		}
		time.Sleep(time.Millisecond)
	}
	if n := len(r.queue); n != 0 {
		t.Fatalf("the writer left %d plug reports queued; want all four in its first batch", n)
	}
	hold := time.AfterFunc(wedgedHold, st.release)
	t.Cleanup(func() { hold.Stop() })
	return st
}

const (
	// wedgedHold is how long wedgedWriter keeps the writer wedged: three
	// times the never-blocks bound, and over ten times what the whole
	// burst those tests make takes under -race.
	wedgedHold = 3 * time.Second
	// wedgedWriteTimeout is the write deadline while a writer is wedged. The
	// plug's deadline is a multiple of it, so it outlasts wedgedHold by far:
	// the hold ends by release, never by the deadline.
	wedgedWriteTimeout = 5 * time.Second
)

// setWriteTimeout sets the writer's per-chunk write deadline for this test.
// No writer may be running while it changes (the writer reads it), so the
// reporter is closed first, and closed again before it is restored.
func setWriteTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	closeReporter(t)
	old := writeTimeout
	writeTimeout = d
	t.Cleanup(func() {
		closeReporter(t)
		writeTimeout = old
	})
}
