package devbench_test

import (
	"encoding/json"
	"errors"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	devbench "github.com/pasperry/devbench-sdk/go"
)

var update = flag.Bool("update", false, "regenerate the shared trace vectors")

// vectorsPath honours DEVBENCH_TESTDATA (the public SDK repo's CI, whose
// layout differs), else the backend repo's testdata/.
var vectorsPath = func() string {
	if d := os.Getenv("DEVBENCH_TESTDATA"); d != "" {
		return filepath.Join(d, "trace_vectors.json")
	}
	return "../../../testdata/trace_vectors.json"
}()

// Vectors is the contract between three implementations: Go, TypeScript, and
// Ruby all parse and render this correlation id.
//
// Risk #9 is that the trace does not survive all four runtimes. Most of that
// risk is wiring, but a slice of it is disagreement: if Rails accepts a value
// the browser wrote and renders it differently, or Go rejects what Ruby
// forwards, the request still succeeds and the evidence simply cannot be
// joined. Nothing fails, nothing logs, and the detectors quietly degrade.
//
// A shared fixture turns that from a code-review hope into a test each
// implementation must pass.
type vectorFile struct {
	Valid []struct {
		Raw     string `json:"raw"`
		Session string `json:"session"`
		Intent  string `json:"intent"`
		Hop     int    `json:"hop"`
		Key     string `json:"key"`
		NextHop string `json:"next_hop"` // "" when at the limit
	} `json:"valid"`
	Invalid []string `json:"invalid"`
}

func buildVectors() vectorFile {
	var f vectorFile

	for _, raw := range []string{
		"v1/sessA/actB/0",
		"v1/sess_A-1/act_B-2/3",
		"v1/a/b/99",
		"v1/0/0/0",
		"v1/" + repeat("x", 64) + "/b/12",
	} {
		t, ok := devbench.ParseTrace(raw)
		if !ok {
			panic("fixture is not parseable: " + raw)
		}
		next := ""
		if n := t.Next(); n.Valid() {
			next = n.String()
		}
		f.Valid = append(f.Valid, struct {
			Raw     string `json:"raw"`
			Session string `json:"session"`
			Intent  string `json:"intent"`
			Hop     int    `json:"hop"`
			Key     string `json:"key"`
			NextHop string `json:"next_hop"`
		}{raw, t.Session, t.Intent, t.Hop, t.Key(), next})
	}

	f.Invalid = []string{
		"", "garbage", "v2/a/b/0", "v1/a/b", "v1/a/b/c", "v1//b/0",
		"v1/a//0", "v1/a/b/-1", "v1/a/b/1000", "v1/" + repeat("x", 65) + "/b/0",
		"v1/a b/c/0", "v1/a/b/0/extra", "V1/a/b/0", "v1/a/b/0 extra",
		"v1/a.b/c/0", "v1/a/b/", "/a/b/0",
	}
	return f
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

func TestTraceVectors(t *testing.T) {
	built := buildVectors()

	if *update {
		body, err := json.MarshalIndent(built, "", "  ")
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.WriteFile(vectorsPath, append(body, '\n'), 0o644); err != nil {
			t.Fatalf("write vectors: %v", err)
		}
		t.Logf("wrote %d valid and %d invalid vectors", len(built.Valid), len(built.Invalid))
		return
	}

	raw, err := os.ReadFile(vectorsPath)
	if errors.Is(err, fs.ErrNotExist) && os.Getenv("ADT_REQUIRE_VECTORS") != "1" {
		// A vendored or published copy has no testdata/; CI sets the
		// variable so a missing file fails there instead of skipping.
		t.Skip("trace vectors not present (vendored copy); set ADT_REQUIRE_VECTORS=1 to require them")
	}
	if err != nil {
		t.Fatalf("read vectors (run with -update): %v", err)
	}

	var want vectorFile
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}

	for _, v := range want.Valid {
		got, ok := devbench.ParseTrace(v.Raw)
		if !ok {
			t.Errorf("ParseTrace(%q) rejected a vector every implementation must accept", v.Raw)
			continue
		}
		if got.Session != v.Session || got.Intent != v.Intent || got.Hop != v.Hop {
			t.Errorf("ParseTrace(%q) = %+v, want session=%s intent=%s hop=%d", v.Raw, got, v.Session, v.Intent, v.Hop)
		}
		if got.Key() != v.Key {
			t.Errorf("Key(%q) = %q, want %q", v.Raw, got.Key(), v.Key)
		}
		if got.String() != v.Raw {
			t.Errorf("String() = %q, want %q", got.String(), v.Raw)
		}

		next := ""
		if n := got.Next(); n.Valid() {
			next = n.String()
		}
		if next != v.NextHop {
			t.Errorf("Next(%q) = %q, want %q", v.Raw, next, v.NextHop)
		}
	}

	for _, bad := range want.Invalid {
		if _, ok := devbench.ParseTrace(bad); ok {
			t.Errorf("ParseTrace(%q) accepted a vector every implementation must reject", bad)
		}
	}
}
