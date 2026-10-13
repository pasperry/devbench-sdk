package devbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// Every flush request names the SDK that sent it:
// "sdk": {"name": "go", "version": Version}
// (docs/server-sdk/wire-protocol.md, "the SDK's name and version"; #309).
// Checked on what actually arrives at the stand-in ingest's real socket.

// topKeys is the flush body's top-level keys in the order they were sent.
func topKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("flush body is not an object: %s", raw)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("read key: %v", err)
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatalf("read value of %v: %v", tok, err)
		}
	}
	return keys
}

func assertSDK(t *testing.T, fl gotFlush) {
	t.Helper()
	if fl.Body.SDK == nil {
		t.Fatalf("no sdk field in %.300s", fl.Raw)
	}
	if fl.Body.SDK.Name != "go" || fl.Body.SDK.Version != Version {
		t.Errorf("sdk = %+v, want {go %s}", *fl.Body.SDK, Version)
	}
	var whole struct {
		SDK map[string]json.RawMessage `json:"sdk"`
	}
	if err := json.Unmarshal(fl.Raw, &whole); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(whole.SDK) != 2 {
		t.Errorf("sdk has keys %v, want exactly name and version", whole.SDK)
	}
	// After sensor_id and before counts, as the wire-protocol example has it.
	keys := topKeys(t, fl.Raw)
	si, ci, ki := slices.Index(keys, "sensor_id"), slices.Index(keys, "sdk"), slices.Index(keys, "counts")
	if !(si >= 0 && ci == si+1 && ki == ci+1) {
		t.Errorf("keys = %v, want sdk right after sensor_id and right before counts", keys)
	}
}

func TestSDKField_ACountsFlushCarriesIt(t *testing.T) {
	f := newFakeIngest(t)
	useDirect(t, f)

	ReportHandled(context.Background(), errors.New("x"), "billing.Persist")
	flushNow(t)

	_, flushes, _ := f.snapshot()
	if len(flushes) != 1 {
		t.Fatalf("got %d flushes, want 1", len(flushes))
	}
	assertSDK(t, flushes[0])
	// Nothing else changes: exactly the keys the body had before #309, plus sdk.
	want := []string{"v", "source", "service", "release", "sensor_id", "sdk", "counts"}
	if got := topKeys(t, flushes[0].Raw); !slices.Equal(got, want) {
		t.Errorf("keys = %v, want %v", got, want)
	}
}

func TestSDKField_EveryRequestOfASplitWindowCarriesIt(t *testing.T) {
	f := newFakeIngest(t)
	useDirect(t, f)

	long := strings.Repeat("a", 60)
	err := errors.New("x")
	for i := 0; i < 150; i++ { // ~2 KB a count: ~300 KB, more than one request
		site := siteName(i)
		for u := 0; u < maxUsersPerCount; u++ {
			ctx := WithUser(context.Background(), User{Email: fmt.Sprintf("%s%d@example.com", long, u), Account: fmt.Sprintf("acct-%d", u)})
			ReportHandled(ctx, err, site)
		}
		for d := curDirect.Load(); d != nil && len(d.queue) > directQueueSize/2; {
			time.Sleep(time.Millisecond)
		}
	}
	flushNow(t)

	_, flushes, _ := f.snapshot()
	if len(flushes) < 2 {
		t.Fatalf("%d flushes; want the window split", len(flushes))
	}
	for _, fl := range flushes {
		assertSDK(t, fl)
		if fl.Size >= maxFlushBodyBytes {
			t.Errorf("a request was %d bytes, limit %d", fl.Size, maxFlushBodyBytes)
		}
	}
}

func TestSDKField_TheEmptyPollCarriesIt(t *testing.T) {
	f := newFakeIngest(t)
	f.mu.Lock()
	f.logsOnlyOnPoll = true
	f.mu.Unlock()
	useLogs(t, f)

	stdLog().Printf("[v1/sessSDK/act1/0] hello")
	f.askLogs("sessSDK/act1")
	flushNow(t)

	_, flushes, _ := f.snapshot()
	if len(flushes) != 1 {
		t.Fatalf("got %d flushes, want the one poll", len(flushes))
	}
	if !bytes.Contains(flushes[0].Raw, []byte(`"counts":[]`)) {
		t.Fatalf("not an empty poll: %s", flushes[0].Raw)
	}
	assertSDK(t, flushes[0])
}

// The name is the one the shared vectors give this SDK's directory, and
// both values pass ingest's patterns (else ingest drops the field).
func TestSDKField_NameIsTheVectorsAndBothValuesPassIngestsPatterns(t *testing.T) {
	var vectors struct {
		SDK struct {
			Names          map[string]string `json:"names"`
			NamePattern    string            `json:"name_pattern"`
			VersionPattern string            `json:"version_pattern"`
		} `json:"sdk"`
	}
	if err := json.Unmarshal(readVectors(t, "protocol_vectors.json"), &vectors); err != nil {
		t.Fatalf("parse protocol_vectors.json: %v", err)
	}
	var mine []string
	for name, dir := range vectors.SDK.Names {
		if dir == "sdk/server/go" {
			mine = append(mine, name)
		}
	}
	if !slices.Equal(mine, []string{Name}) {
		t.Errorf("vectors name sdk/server/go %v; Name = %q", mine, Name)
	}
	if !regexp.MustCompile(vectors.SDK.NamePattern).MatchString(Name) {
		t.Errorf("Name %q does not match %s", Name, vectors.SDK.NamePattern)
	}
	if !regexp.MustCompile(vectors.SDK.VersionPattern).MatchString(Version) {
		t.Errorf("Version %q does not match %s", Version, vectors.SDK.VersionPattern)
	}
}
