package devbench

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// A sidecar that is slow to read — not dead, just behind — must cost time,
// not reports. Before writes were chunked, one batch of 256 large reports
// went out as a single multi-megabyte write under one 250ms deadline; a
// reader at a few MB/s timed it out partway, truncating a line and losing
// the rest of the batch. Reproduces the failure CI hit on Linux: here the
// reader manages ~400 KB/s (macOS's small socket buffer caps each read), so
// 1 MB written under one deadline cannot finish.
func TestReporter_SlowSidecarLosesNothing(t *testing.T) {
	path := socketPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	var mu sync.Mutex
	var got []string
	var wg sync.WaitGroup
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				defer c.Close()
				// ~3 MB/s: 64 KiB every 20ms.
				r := bufio.NewReaderSize(&throttled{c: c, chunk: 64 << 10, every: 20 * time.Millisecond}, 1<<20)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					mu.Lock()
					got = append(got, line)
					mu.Unlock()
				}
			}(conn)
		}
	}()

	useSocket(t, path)
	t.Cleanup(func() { closeReporter(t) })

	const n = 64
	body := strings.Repeat("z", 16<<10)
	for i := 0; i < n; i++ {
		send(message{V: 1, Kind: "handled_failure", Symbol: "slow", Message: body})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_ = ln.Close()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	whole := 0
	for _, l := range got {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["symbol"] == "slow" {
			whole++
		}
	}
	if whole != n {
		t.Errorf("sidecar received %d whole reports of %d (%d lines read); a slow reader lost reports", whole, n, len(got))
	}
}

// throttled reads at most chunk bytes per interval.
type throttled struct {
	c     net.Conn
	chunk int
	every time.Duration
}

func (t *throttled) Read(p []byte) (int, error) {
	time.Sleep(t.every)
	if len(p) > t.chunk {
		p = p[:t.chunk]
	}
	return t.c.Read(p)
}
