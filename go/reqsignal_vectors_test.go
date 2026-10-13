package devbench

import (
	"encoding/json"
	"reflect"
	"testing"
)

// testdata/request_signal_vectors.json is what holds every SDK's request
// signals (Capability 2b) to the backend's internal/reqsignal. Every
// section, every case; required in CI (ADT_REQUIRE_VECTORS=1).

type reqVectorFile struct {
	Constants struct {
		SlowThresholdMs    int64  `json:"slow_threshold_ms"`
		SpikeWindowSeconds int64  `json:"spike_window_seconds"`
		SpikeMinCount      int64  `json:"spike_min_count"`
		SpikeMinPercent    int64  `json:"spike_min_percent"`
		MaxRoutesPerWindow int    `json:"max_routes_per_window"`
		MaxRouteChars      int    `json:"max_route_chars"`
		UnknownRoute       string `json:"unknown_route"`
		OtherMethod        string `json:"other_method"`
	} `json:"constants"`
	Classify []struct {
		Name              string `json:"name"`
		Status            int    `json:"status"`
		DurationMs        int64  `json:"duration_ms"`
		ExceptionReported bool   `json:"exception_reported"`
		HandledReported   bool   `json:"handled_reported"`
		Streaming         bool   `json:"streaming"`
		Want              string `json:"want"`
	} `json:"classify"`
	Methods []struct{ In, Out string } `json:"methods"`
	Routes  []struct{ In, Out string } `json:"routes"`
	Build   []struct {
		Name    string `json:"name"`
		Service string `json:"service"`
		Request struct {
			Signal     string `json:"signal"`
			Method     string `json:"method"`
			Route      string `json:"route"`
			Status     int    `json:"status"`
			DurationMs int64  `json:"duration_ms"`
			Count      int64  `json:"count"`
			Total      int64  `json:"total"`
		} `json:"request"`
		Kind   string `json:"kind"`
		Signal struct {
			Kind    string `json:"kind"`
			Source  string `json:"source"`
			Service string `json:"service"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"signal"`
		ExpectedFP string       `json:"expected_fp"`
		Normalized fpNormalized `json:"normalized"`
		Text       string       `json:"text"`
		Exemplar   string       `json:"exemplar"`
	} `json:"build"`
	Invalid []struct {
		Name   string `json:"name"`
		Signal string `json:"signal"`
		Status int    `json:"status"`
	} `json:"invalid_build"`
	Spikes []struct {
		Name  string `json:"name"`
		Steps []struct {
			Observe *struct {
				TMs    int64  `json:"t_ms"`
				Method string `json:"method"`
				Route  string `json:"route"`
				Status int    `json:"status"`
				Repeat int    `json:"repeat"`
			} `json:"observe"`
			TickMs *int64  `json:"tick_ms"`
			End    bool    `json:"end"`
			Want   []spike `json:"want"`
		} `json:"steps"`
	} `json:"spikes"`
}

func loadRequestVectors(t *testing.T) reqVectorFile {
	t.Helper()
	var v reqVectorFile
	if err := json.Unmarshal(readVectors(t, "request_signal_vectors.json"), &v); err != nil {
		t.Fatalf("parse request_signal_vectors.json: %v", err)
	}
	// An empty section would pass every loop below vacuously.
	if len(v.Classify) == 0 || len(v.Methods) == 0 || len(v.Routes) == 0 ||
		len(v.Build) == 0 || len(v.Invalid) == 0 || len(v.Spikes) == 0 {
		t.Fatalf("a vector section is empty: classify=%d methods=%d routes=%d build=%d invalid=%d spikes=%d",
			len(v.Classify), len(v.Methods), len(v.Routes), len(v.Build), len(v.Invalid), len(v.Spikes))
	}
	return v
}

func TestRequestVectors_Constants(t *testing.T) {
	c := loadRequestVectors(t).Constants
	got := map[string]any{
		"slow_threshold_ms":     int64(slowThresholdMs),
		"spike_window_seconds":  int64(spikeWindowSeconds),
		"spike_min_count":       int64(spikeMinCount),
		"spike_min_percent":     int64(spikeMinPercent),
		"max_routes_per_window": maxRoutesPerWindow,
		"max_route_chars":       maxRouteChars,
		"unknown_route":         unknownRoute,
		"other_method":          otherMethod,
	}
	want := map[string]any{
		"slow_threshold_ms":     c.SlowThresholdMs,
		"spike_window_seconds":  c.SpikeWindowSeconds,
		"spike_min_count":       c.SpikeMinCount,
		"spike_min_percent":     c.SpikeMinPercent,
		"max_routes_per_window": c.MaxRoutesPerWindow,
		"max_route_chars":       c.MaxRouteChars,
		"unknown_route":         c.UnknownRoute,
		"other_method":          c.OtherMethod,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("constants = %v, vectors say %v", got, want)
	}
}

func TestRequestVectors_Classify(t *testing.T) {
	for _, c := range loadRequestVectors(t).Classify {
		// handled_reported is in the vectors to prove it changes nothing:
		// the SDK has no input for it, by design.
		got := classifyRequest(requestOutcome{
			status: c.Status, durationMs: c.DurationMs,
			exceptionReported: c.ExceptionReported, streaming: c.Streaming,
		})
		if got != c.Want {
			t.Errorf("%s: classify = %q, want %q", c.Name, got, c.Want)
		}
	}
}

func TestRequestVectors_MethodsAndRoutes(t *testing.T) {
	v := loadRequestVectors(t)
	for _, c := range v.Methods {
		if got := normalizeMethod(c.In); got != c.Out {
			t.Errorf("method %q = %q, want %q", c.In, got, c.Out)
		}
	}
	for _, c := range v.Routes {
		if got := normalizeRoute(c.In); got != c.Out {
			t.Errorf("route %q = %q, want %q", c.In, got, c.Out)
		}
	}
}

func TestRequestVectors_Build(t *testing.T) {
	for _, c := range loadRequestVectors(t).Build {
		r := c.Request
		b, err := buildRequestSignal(c.Service, requestSignal{
			signal: r.Signal, method: r.Method, route: r.Route, status: r.Status,
			durationMs: r.DurationMs, count: r.Count, total: r.Total,
		})
		if err != nil {
			t.Errorf("%s: build: %v", c.Name, err)
			continue
		}
		if b.kind != c.Kind {
			t.Errorf("%s: kind = %q, want %q", c.Name, b.kind, c.Kind)
		}
		s := c.Signal
		want := fpSignal{Kind: s.Kind, Source: s.Source, Service: s.Service, Type: s.Type, Message: s.Message}
		if !reflect.DeepEqual(b.sig, want) {
			t.Errorf("%s: signal = %+v, want %+v", c.Name, b.sig, want)
		}
		fp, norm, err := computeFingerprint(b.sig)
		if err != nil {
			t.Errorf("%s: fingerprint: %v", c.Name, err)
			continue
		}
		if fp != c.ExpectedFP {
			t.Errorf("%s: fp = %s, want %s", c.Name, fp, c.ExpectedFP)
		}
		if !reflect.DeepEqual(norm, c.Normalized) {
			t.Errorf("%s: normalized = %+v, want %+v", c.Name, norm, c.Normalized)
		}
		if b.text != c.Text {
			t.Errorf("%s: text = %q, want %q", c.Name, b.text, c.Text)
		}
		if b.exemplar != c.Exemplar {
			t.Errorf("%s: exemplar = %q, want %q", c.Name, b.exemplar, c.Exemplar)
		}
	}
}

func TestRequestVectors_InvalidBuildIsRefused(t *testing.T) {
	for _, c := range loadRequestVectors(t).Invalid {
		if b, err := buildRequestSignal("app", requestSignal{signal: c.Signal, method: "GET", route: "/x", status: c.Status}); err == nil {
			t.Errorf("%s: built %+v, want it refused", c.Name, b)
		}
	}
}

func TestRequestVectors_Spikes(t *testing.T) {
	for _, c := range loadRequestVectors(t).Spikes {
		d := &spikeDetector{}
		for i, st := range c.Steps {
			var got []spike
			switch {
			case st.Observe != nil:
				o := st.Observe
				n := o.Repeat
				if n == 0 {
					n = 1
				}
				for j := 0; j < n; j++ {
					got = append(got, d.observe(o.TMs, o.Method, o.Route, o.Status)...)
				}
			case st.TickMs != nil:
				got = d.tick(*st.TickMs)
			case st.End:
				got = d.end()
			default:
				t.Fatalf("%s step %d: no action", c.Name, i)
			}
			if len(got) == 0 && len(st.Want) == 0 {
				continue
			}
			if !reflect.DeepEqual(got, st.Want) {
				t.Errorf("%s step %d: spikes = %+v, want %+v", c.Name, i, got, st.Want)
			}
		}
	}
}
