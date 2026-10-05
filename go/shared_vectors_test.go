package devbench

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The direct transport's fingerprinting and redaction are copies of the
// backend's (internal/fingerprint, internal/scrub, the sidecar's strict
// egress). A copy that drifts splits every issue in two or leaks what the
// other side removes, so both are held to the shared vector files every
// implementation must reproduce.
//
// testdata/ lives at the root of the backend repository. A vendored or
// published copy of this module does not carry it; the tests skip there,
// unless ADT_REQUIRE_VECTORS=1 (CI) turns a missing file into a failure.

// testdataDir is where the shared vectors live: DEVBENCH_TESTDATA when set
// (the public SDK repo's CI, whose layout differs), else the backend repo.
var testdataDir = func() string {
	if d := os.Getenv("DEVBENCH_TESTDATA"); d != "" {
		return d
	}
	return "../../../testdata"
}()

func readVectors(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(testdataDir, name))
	if errors.Is(err, fs.ErrNotExist) && os.Getenv("ADT_REQUIRE_VECTORS") != "1" {
		t.Skipf("%s not present (vendored copy); set ADT_REQUIRE_VECTORS=1 to require it", name)
	}
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return raw
}

func TestFingerprintVectors_ReproducedExactly(t *testing.T) {
	var vectors []struct {
		Name   string `json:"name"`
		Signal struct {
			Kind    string  `json:"kind"`
			Source  string  `json:"source"`
			Service string  `json:"service"`
			Type    string  `json:"type"`
			Message string  `json:"message"`
			Frames  []Frame `json:"frames"`
		} `json:"signal"`
		Expected string       `json:"expected_fp"`
		Norm     fpNormalized `json:"normalized"`
	}
	if err := json.Unmarshal(readVectors(t, "fingerprint_vectors.json"), &vectors); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(vectors) == 0 {
		t.Fatal("no vectors; this test would prove nothing")
	}

	for _, v := range vectors {
		sig := fpSignal{
			Kind: v.Signal.Kind, Source: v.Signal.Source, Service: v.Signal.Service,
			Type: v.Signal.Type, Message: v.Signal.Message, Frames: v.Signal.Frames,
		}
		fp, norm, err := computeFingerprint(sig)
		if err != nil {
			t.Errorf("%s: %v", v.Name, err)
			continue
		}
		if fp != v.Expected {
			t.Errorf("%s: fingerprint %s, want %s — direct mode would fork this issue from the sidecar's",
				v.Name, fp, v.Expected)
		}
		gotNorm, _ := json.Marshal(norm)
		wantNorm, _ := json.Marshal(v.Norm)
		if string(gotNorm) != string(wantNorm) {
			t.Errorf("%s: normalized\n  got  %s\n  want %s", v.Name, gotNorm, wantNorm)
		}
	}
}

type redactionVector struct {
	Name string `json:"name"`
	In   string `json:"in"`
	Out  string `json:"out"`
}

type redactionVectors struct {
	Shared      []redactionVector `json:"shared"`
	ServerOnly  []redactionVector `json:"server_only"`
	MustSurvive []redactionVector `json:"must_survive"`
	Prose       []redactionVector `json:"prose"`
}

func loadRedaction(t *testing.T) redactionVectors {
	t.Helper()
	var vf redactionVectors
	if err := json.Unmarshal(readVectors(t, "redaction_vectors.json"), &vf); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(vf.Shared) == 0 || len(vf.ServerOnly) == 0 || len(vf.MustSurvive) == 0 || len(vf.Prose) == 0 {
		t.Fatal("vector file is missing sections; this test would prove nothing")
	}
	return vf
}

// The shape pass and the name heuristic, each exactly as the vectors say.
func TestRedactionVectors_ReproducedExactly(t *testing.T) {
	vf := loadRedaction(t)
	for _, set := range [][]redactionVector{vf.Shared, vf.ServerOnly, vf.MustSurvive} {
		for _, v := range set {
			if got := scrubText(v.In); got != v.Out {
				t.Errorf("%s:\n  in   %q\n  got  %q\n  want %q", v.Name, v.In, got, v.Out)
			}
		}
	}
	for _, v := range vf.Prose {
		if got := scrubProse(v.In); got != v.Out {
			t.Errorf("prose %s:\n  in   %q\n  got  %q\n  want %q", v.Name, v.In, got, v.Out)
		}
	}
}

// The full strict egress path that evidence takes: whatever a must-redact
// vector removes must not survive it, and what a must-survive vector keeps
// must still be readable after it (templating turns numbers into holes; the
// words that explain the failure stay).
func TestRedactionVectors_StrictEgress(t *testing.T) {
	vf := loadRedaction(t)

	redact := append(append(append([]redactionVector{}, vf.Shared...), vf.ServerOnly...), vf.Prose...)
	checked := 0
	for _, v := range redact {
		out := egressLine(v.In)
		for _, secret := range removedTokens(v.In, v.Out) {
			checked++
			if strings.Contains(out, secret) {
				t.Errorf("%s: %q survived strict egress: %q", v.Name, secret, out)
			}
		}
		// Bundle templates go through the same passes plus one more.
		if tpl := egressTemplate(v.In); tpl != scrubText(out) {
			t.Errorf("%s: template egress %q disagrees with line egress %q", v.Name, tpl, out)
		}
	}
	if checked == 0 {
		t.Fatal("no must-redact token was checked")
	}

	for _, v := range vf.MustSurvive {
		out := egressLine(v.In)
		for _, word := range strings.Fields(v.Out) {
			if len(word) < 4 || strings.ContainsAny(word, "0123456789") {
				continue
			}
			if !strings.Contains(out, word) {
				t.Errorf("%s: %q was destroyed by strict egress: %q", v.Name, word, out)
			}
		}
	}
}

// removedTokens are the words of in that out does not keep — the values a
// vector says must go — trimmed of punctuation, long enough to be a value.
func removedTokens(in, out string) []string {
	kept := map[string]bool{}
	for _, w := range strings.Fields(out) {
		kept[w] = true
	}
	var gone []string
	for _, w := range strings.Fields(in) {
		if kept[w] {
			continue
		}
		w = strings.Trim(w, `"'(),.:;`)
		if len(w) >= 4 {
			gone = append(gone, w)
		}
	}
	return gone
}

// A correlation id in a message is lifted out and put back in front, as the
// sidecar does, rather than templated into holes.
func TestEgressLine_KeepsTheTraceLikeTheSidecar(t *testing.T) {
	got := egressLine("*errors.errorString: failed for pat@example.com under v1/sessA/actB/2 [request]")
	want := "[v1/sessA/actB/2] *errors.errorString: failed for <email> under <trace> [request]"
	if got != want {
		t.Errorf("egressLine = %q\nwant         %q", got, want)
	}
}
