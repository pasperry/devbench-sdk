package devbench

import "net/http"

// Transport forwards the correlation id on outbound requests, with the hop
// incremented.
//
// This is the capability that actually breaks (SERVER_SDK_SPEC, capability 1).
// Accepting a header is easy and obviously correct; *forwarding* it is the part
// people forget, and when it is missing the failure is silent — client evidence
// simply cannot be joined to downstream logs, and both detectors degrade to
// guesswork with nothing in any log to say why.
//
//	client := &http.Client{Transport: devbench.WrapTransport(nil)}
//	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
//	res, _ := client.Do(req)   // carries the trace, hop+1
//
// The context on the request is what supplies the trace, so a request built
// without the parent context propagates nothing. That is the other common way
// this breaks.
type Transport struct {
	Base http.RoundTripper
}

// WrapTransport returns a RoundTripper that forwards the trace. A nil base
// means http.DefaultTransport.
func WrapTransport(base http.RoundTripper) http.RoundTripper {
	return &Transport{Base: base}
}

func (t *Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}

	trace, ok := FromContext(r.Context())
	if !ok || trace.Hop >= maxHop {
		// No trace, or a routing loop. Either way, send the request untouched:
		// propagation is never worth failing or distorting a real call.
		return base.RoundTrip(r)
	}

	// Clone before mutating. The caller may reuse the request, and RoundTrip is
	// documented as not modifying it.
	clone := r.Clone(r.Context())
	clone.Header.Set(Header, trace.Next().String())
	return base.RoundTrip(clone)
}
