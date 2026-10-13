package devbench

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// Request signals (docs/server-sdk/failed-and-slow-requests.md "Capability 2b — Failed and slow
// requests"; DECISIONS #171): what a request does without raising.
//
//   - failed: the application answered 500–599 and no exception was
//     reported during the request;
//   - slow: it answered 100–499, not streaming, after more than 3,000 ms;
//   - rate_limit_spike: one route's 429s in one unix minute met the spike
//     rule (at least ten, and at least 10% of that route's requests).
//
// A copy of the backend's internal/reqsignal, which this module cannot
// import (stdlib only, published on its own). A copy can drift, and a
// drifted copy reports a different set of problems by language — so it is
// held to the shared vectors (testdata/request_signal_vectors.json) every
// implementation must reproduce. Change nothing here without changing
// internal/reqsignal in lockstep.

// The thresholds: constants, not configuration. Tenant-configurable
// thresholds are a later issue (DECISIONS #171), and every SDK must apply
// the same numbers or the same app reports differently by language.
const (
	// slowThresholdMs: a request is slow when it took strictly longer.
	slowThresholdMs = 3000
	// spikeWindowSeconds is the length of a spike window, aligned to the
	// unix minute (window = floor(t_ms / 60000)).
	spikeWindowSeconds = 60
	// spikeMinCount is the fewest 429s on one route in one window that can
	// be a spike.
	spikeMinCount = 10
	// spikeMinPercent is the smallest share (percent) of the route's
	// requests in the window that must have been 429s.
	spikeMinPercent = 10
	// maxRoutesPerWindow bounds the spike table; requests on further
	// (method, route) keys in the same window are not tracked.
	maxRoutesPerWindow = 512
	// maxRouteChars bounds a route, in code points.
	maxRouteChars = 255
)

// The `signal` field of the `request` control message.
const (
	signalFailed         = "failed"
	signalSlow           = "slow"
	signalRateLimitSpike = "rate_limit_spike"
)

// unknownRoute stands for a request matched to no route pattern: one
// literal, never the raw path (a path carries ids and slugs).
const unknownRoute = "*"

// otherMethod stands for any method outside the standard set, so a client
// sending invented methods cannot mint fingerprints.
const otherMethod = "OTHER"

// requestOutcome is what Middleware knows once the application has handed
// back its response.
type requestOutcome struct {
	status     int
	durationMs int64
	// exceptionReported: an `exception` was reported during this request
	// (any context). A handled failure does not count.
	exceptionReported bool
	// streaming: a 101 upgrade, a hijacked connection or a
	// text/event-stream response, whose duration is a connection's
	// lifetime, not the request's latency.
	streaming bool
}

// classifyRequest returns the signal one request produces: signalFailed,
// signalSlow, or "" for none. At most one per request. 429s are not
// classified here; they feed the spike detector.
func classifyRequest(o requestOutcome) string {
	if o.status < 100 || o.status > 599 {
		return ""
	}
	if o.status >= 500 {
		if o.exceptionReported {
			return "" // the exception is the report
		}
		return signalFailed
	}
	if o.streaming || o.status == 101 {
		return ""
	}
	if o.durationMs > slowThresholdMs {
		return signalSlow
	}
	return ""
}

func isStandardMethod(m string) bool {
	switch m {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "TRACE", "CONNECT":
		return true
	}
	return false
}

// normalizeMethod uppercases and trims a method; anything outside the
// standard set is otherMethod.
func normalizeMethod(m string) string {
	m = strings.ToUpper(strings.TrimSpace(m))
	if !isStandardMethod(m) {
		return otherMethod
	}
	return m
}

// normalizeRoute trims a route pattern and bounds it; empty is
// unknownRoute.
func normalizeRoute(r string) string {
	r = strings.TrimSpace(r)
	if r == "" {
		return unknownRoute
	}
	if utf8.RuneCountInString(r) > maxRouteChars {
		r = string([]rune(r)[:maxRouteChars])
	}
	return r
}

// routeOfPattern is a ServeMux pattern without its method prefix:
// "GET /v1/stock/{sku}" is "/v1/stock/{sku}" (the spec's route; the method
// is part of the identity on its own). ServeMux separates the method with
// spaces or tabs, and a pattern's path never contains one.
func routeOfPattern(p string) string {
	p = strings.TrimSpace(p)
	if i := strings.IndexAny(p, " \t"); i >= 0 {
		return strings.TrimLeft(p[i+1:], " \t")
	}
	return p
}

// requestSignal is one request signal, as the control message carries it.
type requestSignal struct {
	signal     string
	method     string
	route      string
	status     int
	durationMs int64
	count      int64 // a spike's 429s
	total      int64 // a spike's requests on the route
}

// builtSignal is everything the direct transport needs to count a request
// signal.
type builtSignal struct {
	kind     string // the count's kind: error or degradation
	sig      fpSignal
	text     string // template text kept for evidence
	exemplar string // raw evidence line, scrubbed at egress
}

var errInvalidRequestSignal = errors.New("devbench: invalid request signal")

// buildRequestSignal fingerprints a request signal for service, as the
// sidecar does with the `request` control message.
func buildRequestSignal(service string, r requestSignal) (builtSignal, error) {
	method := normalizeMethod(r.method)
	route := normalizeRoute(r.route)
	message := method + " " + route

	var kind, typ, exemplar string
	switch r.signal {
	case signalFailed:
		if r.status < 500 || r.status > 599 {
			return builtSignal{}, fmt.Errorf("%w: failed with status %d", errInvalidRequestSignal, r.status)
		}
		kind = "error"
		typ = fmt.Sprintf("http_%d", r.status)
		exemplar = fmt.Sprintf("%s: %s in %d ms", typ, message, nonNegative(r.durationMs))
	case signalSlow:
		if r.status < 100 || r.status > 499 {
			return builtSignal{}, fmt.Errorf("%w: slow with status %d", errInvalidRequestSignal, r.status)
		}
		kind = "degradation"
		typ = "slow_request"
		exemplar = fmt.Sprintf("%s: %s in %d ms (status %d)", typ, message, nonNegative(r.durationMs), r.status)
	case signalRateLimitSpike:
		kind = "error"
		typ = "http_429_spike"
		exemplar = fmt.Sprintf("%s: %s %d of %d requests answered 429 in one minute",
			typ, message, nonNegative(r.count), nonNegative(r.total))
	default:
		return builtSignal{}, fmt.Errorf("%w: signal %q", errInvalidRequestSignal, r.signal)
	}

	return builtSignal{
		kind:     kind,
		sig:      fpSignal{Kind: kind, Source: "server", Service: service, Type: typ, Message: message},
		text:     typ + " at " + message,
		exemplar: exemplar,
	}, nil
}

func nonNegative(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}

// spike is one (method, route) whose window met the spike rule.
type spike struct {
	Method string `json:"method"`
	Route  string `json:"route"`
	// Window is the window's start, unix seconds.
	Window int64 `json:"window"`
	Count  int64 `json:"count"`
	Total  int64 `json:"total"`
}

type routeKey struct{ method, route string }

type tally struct{ count, total int64 }

// spikeDetector applies the 429 spike rule to one process's requests. Safe
// for concurrent use: every request's goroutine observes, and the flush
// goroutine ticks.
//
// One window is open at a time. An observation in a later window closes the
// open one first; one in an earlier window (a clock step back) counts in the
// open window. Memory is bounded by maxRoutesPerWindow, and nothing is kept
// past the open window.
type spikeDetector struct {
	mu      sync.Mutex
	open    bool
	window  int64 // unix minute index of the open window
	routes  map[routeKey]*tally
	dropped int64
}

// observe records one finished request at tMs (unix milliseconds) and
// returns the spikes of any window it closed.
func (d *spikeDetector) observe(tMs int64, method, route string, status int) []spike {
	w := windowOf(tMs)
	key := routeKey{normalizeMethod(method), normalizeRoute(route)}

	d.mu.Lock()
	defer d.mu.Unlock()

	var closed []spike
	switch {
	case !d.open:
		d.start(w)
	case w > d.window:
		closed = d.evaluate()
		d.start(w)
	}

	t, ok := d.routes[key]
	if !ok {
		if len(d.routes) >= maxRoutesPerWindow {
			d.dropped++
			return closed
		}
		t = &tally{}
		d.routes[key] = t
	}
	t.total++
	if status == 429 {
		t.count++
	}
	return closed
}

// tick closes the open window if nowMs is past it (at each flush) and
// returns its spikes.
func (d *spikeDetector) tick(nowMs int64) []spike {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.open || windowOf(nowMs) <= d.window {
		return nil
	}
	out := d.evaluate()
	d.open = false
	d.routes = nil
	return out
}

// end evaluates the open window as it stands (process exit) and closes it.
func (d *spikeDetector) end() []spike {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.open {
		return nil
	}
	out := d.evaluate()
	d.open = false
	d.routes = nil
	return out
}

// isSpike is the rule, in integer arithmetic: at least spikeMinCount 429s,
// and at least spikeMinPercent of the route's requests in the window.
func isSpike(count, total int64) bool {
	return count >= spikeMinCount && count*100 >= total*spikeMinPercent
}

func (d *spikeDetector) start(w int64) {
	d.open = true
	d.window = w
	d.routes = make(map[routeKey]*tally)
}

// evaluate returns the open window's spikes, sorted by method then route.
// Caller holds d.mu.
func (d *spikeDetector) evaluate() []spike {
	var out []spike
	for k, t := range d.routes {
		if isSpike(t.count, t.total) {
			out = append(out, spike{Method: k.method, Route: k.route,
				Window: d.window * spikeWindowSeconds, Count: t.count, Total: t.total})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Method != out[j].Method {
			return out[i].Method < out[j].Method
		}
		return out[i].Route < out[j].Route
	})
	return out
}

// windowOf floors toward negative infinity, so a pre-1970 clock still maps
// to one window per minute.
func windowOf(tMs int64) int64 {
	const ms = spikeWindowSeconds * 1000
	w := tMs / ms
	if tMs%ms < 0 {
		w--
	}
	return w
}
