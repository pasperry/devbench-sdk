package devbench

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
)

// Answering log slice requests (SERVER_SDK_SPEC "In-process log capture",
// Delivery).
//
// A flush response to a server key may carry need_logs: [{"trace": key}],
// key = <session>/<intent> — the same key Trace.Key() and the sidecar's
// TraceKey() produce, so one request finds the lines in every service the
// action crossed. For each key this process holds lines for, the lines are
// redacted with the same strict egress as evidence (plus the learned rules
// from the response) and POSTed to <base>/v1/logs with the server key.
//
// A key with no lines here gets nothing — not an empty slice. Another
// process (another replica of this service) may hold them, and ingest does
// not let an empty answer from a server SDK settle a request anyway. Lines
// are not removed when sent; they stay until they age out, so a later
// request can ask again.
//
// Runs only inside a flush cycle: the background ticker, Flush, or Close.

const (
	// maxDeliveredLines bounds one slice: what ingest stores
	// (store.MaxSliceLines, spec: ≤ 200). The newest lines are sent: the
	// failure is at the end of a request.
	maxDeliveredLines = 200
	// maxDeliveredSliceBytes matches ingest's per-slice bound
	// (store.MaxSliceBytes); more would be cut on arrival.
	maxDeliveredSliceBytes = 64 << 10
	// maxSlicesPerDelivery is ingest's per-request limit.
	maxSlicesPerDelivery = 25
)

type logSlice struct {
	Trace string   `json:"trace"`
	Lines []string `json:"lines"`
}

type logsBody struct {
	Slices []logSlice `json:"slices"`
}

// answerLogs delivers what this process holds for each requested key.
// Failures are dropped: the request stays pending on ingest's side and is
// offered again on a later flush.
func (d *directClient) answerLogs(ctx context.Context, keys []string) {
	defer func() { _ = recover() }()
	if len(keys) == 0 {
		return
	}
	policy := d.policy.Load()
	now := logNow()

	var slices []logSlice
	for _, key := range keys {
		raw := captured.lookup(key, now)
		if len(raw) == 0 {
			continue
		}
		if len(raw) > maxDeliveredLines {
			raw = raw[len(raw)-maxDeliveredLines:]
		}
		lines := make([]string, len(raw))
		for i, l := range raw {
			lines[i] = truncateBytes(egressLineWith(policy, d.cfg.service, l), logMaxLineBytes)
		}
		slices = append(slices, logSlice{Trace: key, Lines: newestWithin(lines, maxDeliveredSliceBytes)})
	}

	for _, batch := range splitSlices(slices) {
		if err := d.postLogs(ctx, batch); err != nil {
			stats.failed.Add(1)
		}
	}
}

// newestWithin keeps the newest lines whose total size fits in max bytes.
func newestWithin(lines []string, max int) []string {
	total := 0
	for i := len(lines) - 1; i >= 0; i-- {
		total += len(lines[i])
		if total > max {
			return lines[i+1:]
		}
	}
	return lines
}

// splitSlices groups slices so each request stays under ingest's slice
// limit and the flush body budget. Measured, not estimated.
func splitSlices(slices []logSlice) [][]logSlice {
	var out [][]logSlice
	var cur []logSlice
	size := len(`{"slices":[]}`)
	for _, s := range slices {
		raw, err := encodeJSON(s)
		if err != nil {
			continue
		}
		if len(cur) > 0 && (len(cur) >= maxSlicesPerDelivery || size+len(raw)+1 > maxFlushBodyBytes) {
			out = append(out, cur)
			cur, size = nil, len(`{"slices":[]}`)
		}
		cur = append(cur, s)
		size += len(raw) + 1
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// encodeJSON marshals without HTML escaping: redaction placeholders are
// full of '<' and '>', and < would triple their size on the wire.
func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// postLogs sends one delivery, retrying once.
func (d *directClient) postLogs(ctx context.Context, slices []logSlice) error {
	payload, err := encodeJSON(logsBody{Slices: slices})
	if err != nil {
		return errorf("encode log slices: %v", err)
	}
	status, _, err := doWithRetry(ctx, d.client, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.cfg.base+"/v1/logs", bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("content-type", "application/json")
		req.Header.Set("X-ADT-Key", d.cfg.key)
		return req, nil
	})
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return statusError(status)
	}
	return nil
}
