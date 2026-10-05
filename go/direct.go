package devbench

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The direct transport (SERVER_SDK_SPEC "Direct mode", DECISIONS #160).
//
// With a DSN, the SDK does in-process what the sidecar does for it
// otherwise: fingerprint each report, count repeats, flush counts to ingest
// every minute, and upload evidence only when ingest asks for it. Same
// protocol as the browser sensor, so ingest needs no new endpoint, and the
// same fingerprints as the sidecar, so a customer moving between modes keeps
// every issue.
//
// The host's rules are the reporter's rules:
//
//   - the caller never blocks: a report is a non-blocking channel send into a
//     bounded queue; a full queue drops and counts;
//   - fingerprinting, counting and all I/O happen on background goroutines;
//   - network calls time out within five seconds, are retried once with
//     jitter, then discarded — nothing is buffered across a failure;
//   - nothing panics into the application.

const (
	// directQueueSize bounds reports waiting to be counted.
	directQueueSize = 1024

	// maxWindowFingerprints bounds distinct fingerprints per flush window;
	// further ones are counted in `overflowed` (spec: 512, ingest's limit).
	maxWindowFingerprints = 512

	// maxUsersPerCount matches ingest (store.MaxUsersPerCount).
	maxUsersPerCount = 20

	// maxDetails bounds the first-occurrence detail kept for evidence.
	// Kept across windows: ingest may ask for evidence on a later flush.
	maxDetails = 1024

	// maxFlushBodyBytes keeps one request under ingest's 256 KiB limit with
	// room to spare (spec: 192 KiB).
	maxFlushBodyBytes = 192 << 10

	// maxCountsPerFlush is ingest's per-request limit.
	maxCountsPerFlush = 512

	// httpTimeout bounds every request (spec: ≤ 5 s).
	httpTimeout = 5 * time.Second
)

// Tunables, variables so tests can shorten them.
var (
	flushInterval  = 60 * time.Second
	retryJitterMin = 200 * time.Millisecond
	retryJitterMax = 800 * time.Millisecond
)

// sensorID identifies this process to ingest. Random per process.
var sensorID = newSensorID()

func newSensorID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "go-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return "go-" + hex.EncodeToString(b[:])
}

// directClient is one direct transport: one queue, one counter goroutine,
// one ticker goroutine.
type directClient struct {
	cfg    *config
	client *http.Client
	every  time.Duration

	queue    chan item
	start    sync.Once
	stop     chan struct{}
	closing  sync.Once
	aggDone  chan struct{}
	tickDone chan struct{}

	mu      sync.Mutex // guards win, details, detailOrder
	win     *window
	details map[string]*detail
	order   []string

	flushMu  sync.Mutex // one flush cycle at a time
	dropped  atomic.Int64
	launched atomic.Bool

	// policy is the learned redaction rule set from the latest flush
	// response that carried one; nil until then.
	policy atomic.Pointer[egressPolicy]
}

// item is a report, or a barrier that Flush waits on to know every report
// queued before it has been counted.
type item struct {
	m       message
	barrier chan struct{}
}

type window struct {
	entries    map[string]*entry
	overflowed int
}

type entry struct {
	fp, kind      string
	n             int64
	first, last   int64
	users         []User
	usersOverflow int64
}

// detail is the first occurrence of a fingerprint, raw. It is redacted when
// it leaves (evidence), never before: until then it is the application's own
// memory.
type detail struct {
	kind     string
	text     string // the sidecar's template text: "<error> at <symbol>" or the symbol
	exemplar string // the sidecar's exemplar line
	frames   []Frame
}

var curDirect atomic.Pointer[directClient]

func newWindow() *window { return &window{entries: make(map[string]*entry)} }

func newDirectClient(c *config) *directClient {
	return &directClient{
		cfg:      c,
		client:   &http.Client{Timeout: httpTimeout},
		every:    flushInterval,
		queue:    make(chan item, directQueueSize),
		stop:     make(chan struct{}),
		aggDone:  make(chan struct{}),
		tickDone: make(chan struct{}),
		win:      newWindow(),
		details:  make(map[string]*detail),
	}
}

func getDirect(c *config) *directClient {
	for {
		if d := curDirect.Load(); d != nil {
			return d
		}
		d := newDirectClient(c)
		if curDirect.CompareAndSwap(nil, d) {
			return d
		}
	}
}

// enqueue hands m to the counter goroutine. Never blocks.
func (d *directClient) enqueue(m message) {
	d.start.Do(d.launch)
	select {
	case <-d.stop:
		stats.dropped.Add(1)
		return
	default:
	}
	select {
	case d.queue <- item{m: m}:
	default:
		stats.dropped.Add(1)
		d.dropped.Add(1)
	}
}

func (d *directClient) launch() {
	d.launched.Store(true)
	go d.runCounter()
	go d.runTicker()
}

// runCounter fingerprints and counts queued reports until stopped, then
// counts whatever is still queued and exits.
func (d *directClient) runCounter() {
	defer close(d.aggDone)
	for {
		select {
		case it := <-d.queue:
			d.handle(it)
		case <-d.stop:
			for {
				select {
				case it := <-d.queue:
					d.handle(it)
				default:
					return
				}
			}
		}
	}
}

func (d *directClient) handle(it item) {
	defer func() {
		if recover() != nil {
			stats.failed.Add(1)
		}
	}()
	if it.barrier != nil {
		close(it.barrier)
		return
	}
	d.count(it.m, time.Now().Unix())
}

func (d *directClient) runTicker() {
	defer close(d.tickDone)
	t := time.NewTicker(d.every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			d.backgroundFlush()
		case <-d.stop:
			return
		}
	}
}

func (d *directClient) backgroundFlush() {
	defer func() { _ = recover() }()
	_ = d.flushCycle(context.Background())
}

// signalFor builds exactly the signal the sidecar builds from the
// equivalent control message (control.go recordException / recordHandled),
// plus the template text and exemplar it keeps for evidence.
func signalFor(m message, service string) (sig fpSignal, text string) {
	switch m.Kind {
	case "handled_failure":
		// Symbol and class only: the message carries ids and would make
		// every occurrence new; frames arrived later and would move every
		// existing handled-failure fingerprint.
		return fpSignal{
			Kind: "handled_failure", Source: "server", Service: service,
			Type: m.Error, Message: m.Symbol,
		}, m.Symbol
	default:
		frames := m.Frames
		if len(frames) > maxFrames {
			frames = frames[:maxFrames]
		}
		text = m.Error
		if m.Symbol != "" {
			text += " at " + m.Symbol
		}
		return fpSignal{
			Kind: "error", Source: "server", Service: service,
			Type: m.Error, Message: truncateChars(m.Message, maxMessage), Frames: frames,
		}, text
	}
}

// exemplarLine is the sidecar's exemplar(): the human-readable line kept
// for evidence, raw until egress.
func exemplarLine(m message) string {
	var b strings.Builder
	b.WriteString(m.Error)
	if msg := truncateChars(m.Message, maxMessage); msg != "" {
		b.WriteString(": ")
		b.WriteString(strings.ReplaceAll(msg, "\n", " "))
	}
	if m.Context != "" {
		b.WriteString(" [")
		b.WriteString(m.Context)
		if m.Kind == "exception" && m.Handled != nil && *m.Handled {
			b.WriteString(", handled")
		}
		b.WriteString("]")
	}
	if m.Reason != "" {
		b.WriteString(" reason=")
		b.WriteString(m.Reason)
	}
	return b.String()
}

// truncateChars cuts at n characters with no marker, as the sidecar does.
func truncateChars(s string, n int) string {
	return truncateRunes(s, n)
}

// count adds one report to the current window.
func (d *directClient) count(m message, now int64) {
	sig, text := signalFor(m, d.cfg.service)
	fp, _, err := computeFingerprint(sig)
	if err != nil {
		stats.failed.Add(1)
		return
	}
	kind := sig.Kind

	var u User
	if m.User != nil {
		// As the sidecar normalizes it: trimmed, email lowercased, bounded.
		u = User{
			Email:   truncateRunes(strings.ToLower(strings.TrimSpace(m.User.Email)), maxUserField),
			Account: truncateRunes(strings.TrimSpace(m.User.Account), maxUserField),
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	e := d.win.entries[fp]
	if e == nil {
		if len(d.win.entries) >= maxWindowFingerprints {
			d.win.overflowed++
			return
		}
		e = &entry{fp: fp, kind: kind, first: now}
		d.win.entries[fp] = e
	}
	e.n++
	e.last = now
	if u != (User{}) {
		addUser(e, u)
	}

	if _, ok := d.details[fp]; !ok {
		if len(d.order) >= maxDetails {
			oldest := d.order[0]
			d.order = d.order[1:]
			delete(d.details, oldest)
		}
		frames := m.Frames
		if len(frames) > maxFrames {
			frames = frames[:maxFrames]
		}
		d.details[fp] = &detail{kind: kind, text: text, exemplar: exemplarLine(m), frames: frames}
		d.order = append(d.order, fp)
	}
}

func addUser(e *entry, u User) {
	for _, existing := range e.users {
		if existing == u {
			return
		}
	}
	if len(e.users) >= maxUsersPerCount {
		e.usersOverflow++
		return
	}
	e.users = append(e.users, u)
}

// Wire shapes: SERVER_SDK_SPEC "Wire protocol". Ingest rejects unknown
// fields, so nothing beyond these is ever sent.
type flushCount struct {
	FP            string `json:"fp"`
	N             int64  `json:"n"`
	First         int64  `json:"first"`
	Last          int64  `json:"last"`
	Kind          string `json:"kind"`
	Users         []User `json:"users,omitempty"`
	UsersOverflow int64  `json:"users_overflow,omitempty"`
}

type flushBody struct {
	V          int          `json:"v"`
	Source     string       `json:"source"`
	Service    string       `json:"service"`
	Release    string       `json:"release"`
	SensorID   string       `json:"sensor_id"`
	Counts     []flushCount `json:"counts"`
	Overflowed int          `json:"overflowed,omitempty"`
	Dropped    int          `json:"dropped,omitempty"`
}

type evidenceRequest struct {
	FP  string `json:"fp"`
	URL string `json:"url"`
}

type flushReply struct {
	NeedEvidence []evidenceRequest `json:"need_evidence"`
	// NeedLogs asks for the lines held for these trace keys (server keys
	// only, DECISIONS #161).
	NeedLogs []logRequest `json:"need_logs"`
	// Redaction is the full current set of learned rules. A pointer so an
	// absent field (a failed lookup on ingest's side) leaves the rules in
	// force, while an explicit [] withdraws them all — as the sidecar does.
	Redaction *[]egressRule `json:"redaction"`
}

type logRequest struct {
	Trace string `json:"trace"`
}

// evidenceBundle is the sidecar's TemplateBundle, so triage needs no change.
type evidenceBundle struct {
	V         int      `json:"v"`
	FP        string   `json:"fp"`
	Kind      string   `json:"kind"`
	Template  string   `json:"template"`
	Service   string   `json:"service"`
	Exemplars []string `json:"exemplars"`
	Frames    []Frame  `json:"frames,omitempty"`
}

// flushNow counts everything queued so far, then flushes. For Flush.
func (d *directClient) flushNow(ctx context.Context) error {
	if d.launched.Load() {
		b := make(chan struct{})
		select {
		case d.queue <- item{barrier: b}:
			select {
			case <-b:
			case <-d.aggDone: // stopped meanwhile; it counted the queue first
			case <-ctx.Done():
				return ctx.Err()
			}
		case <-d.stop:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return d.flushCycle(ctx)
}

// flushCycle sends the current window and answers evidence and log
// requests.
//
// An empty window sends nothing — unless this process holds traced log
// lines, when it sends one empty flush ("counts": []) as a poll
// (SERVER_SDK_SPEC "Poll while holding lines"): need_logs only rides a
// flush response, so without it a quiet process would never hear that
// triage wants lines only it has. Ingest accepts an empty flush from a
// server key for exactly this.
func (d *directClient) flushCycle(ctx context.Context) error {
	d.flushMu.Lock()
	defer d.flushMu.Unlock()

	d.mu.Lock()
	win := d.win
	d.win = newWindow()
	d.mu.Unlock()

	if len(win.entries) == 0 {
		if !captured.holding(logNow()) {
			return nil
		}
		return d.flushBatches(ctx, win, [][]flushCount{{}}) // the poll
	}

	counts := make([]flushCount, 0, len(win.entries))
	for _, e := range win.entries {
		counts = append(counts, flushCount{
			FP: e.fp, N: e.n, First: e.first, Last: e.last, Kind: e.kind,
			Users: e.users, UsersOverflow: e.usersOverflow,
		})
	}
	sort.Slice(counts, func(i, j int) bool { return counts[i].FP < counts[j].FP })

	envelope := flushBody{
		V: 1, Source: "server", Service: d.cfg.service, Release: d.cfg.release, SensorID: sensorID,
	}
	return d.flushBatches(ctx, win, splitCounts(envelope, counts))
}

// flushBatches posts each batch of a window (an empty batch is the poll),
// then answers what the responses asked for. Caller holds flushMu.
func (d *directClient) flushBatches(ctx context.Context, win *window, batches [][]flushCount) error {
	envelope := flushBody{
		V: 1, Source: "server", Service: d.cfg.service, Release: d.cfg.release, SensorID: sensorID,
	}
	dropped := int(d.dropped.Swap(0))
	var askedLogs []string
	asked := map[string]bool{}
	for i, batch := range batches {
		body := envelope
		body.Counts = batch
		if i == 0 {
			body.Overflowed = win.overflowed
			body.Dropped = dropped
		}
		reply, err := d.post(ctx, body)
		if err != nil {
			// Discarded, not buffered: a diagnostics tool that grows while
			// its backend is down harms the host. This batch and the rest
			// of the window are lost; the next window starts clean.
			for _, rest := range batches[i:] {
				stats.failed.Add(sumN(rest))
			}
			d.answerLogs(ctx, askedLogs)
			return err
		}
		stats.sent.Add(sumN(batch))
		// Rules before answers, as the sidecar does: a rule arriving with a
		// request for the shape it governs must apply to that request.
		if reply.Redaction != nil {
			d.policy.Store(compilePolicy(*reply.Redaction))
		}
		for _, r := range reply.NeedLogs {
			if r.Trace != "" && !asked[r.Trace] {
				asked[r.Trace] = true
				askedLogs = append(askedLogs, r.Trace)
			}
		}
		d.answerEvidence(ctx, reply.NeedEvidence)
	}
	d.answerLogs(ctx, askedLogs)
	return nil
}

func sumN(counts []flushCount) uint64 {
	var n uint64
	for _, c := range counts {
		n += uint64(c.N)
	}
	return n
}

// splitCounts groups counts so each request stays under maxFlushBodyBytes
// and maxCountsPerFlush. Measured, not estimated.
func splitCounts(envelope flushBody, counts []flushCount) [][]flushCount {
	envelope.Counts = []flushCount{}
	envelope.Overflowed, envelope.Dropped = 1<<30, 1<<30 // worst-case widths
	head, _ := json.Marshal(envelope)
	base := len(head)

	var out [][]flushCount
	cur := make([]flushCount, 0, min(len(counts), maxCountsPerFlush))
	size := base
	for _, c := range counts {
		raw, err := json.Marshal(c)
		if err != nil {
			stats.failed.Add(uint64(c.N))
			continue
		}
		if len(cur) > 0 && (len(cur) >= maxCountsPerFlush || size+len(raw)+1 > maxFlushBodyBytes) {
			out = append(out, cur)
			cur = make([]flushCount, 0, cap(cur))
			size = base
		}
		cur = append(cur, c)
		size += len(raw) + 1
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// post sends one phase-1 request, retrying once.
func (d *directClient) post(ctx context.Context, body flushBody) (flushReply, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return flushReply{}, errorf("encode flush: %v", err)
	}
	status, resp, err := doWithRetry(ctx, d.client, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.cfg.base+"/v1/flush", bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("content-type", "application/json")
		req.Header.Set("X-ADT-Key", d.cfg.key)
		return req, nil
	})
	if err != nil {
		return flushReply{}, err
	}
	if status < 200 || status >= 300 {
		return flushReply{}, statusError(status)
	}
	// An unparseable reply is not a failed flush: the counts were accepted.
	var reply flushReply
	_ = json.Unmarshal(resp, &reply)
	return reply, nil
}

func statusError(status int) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return errorf("ingest rejected the key (HTTP %d): check the DSN", status)
	default:
		return errorf("ingest answered HTTP %d", status)
	}
}

// answerEvidence uploads the bundle for each fingerprint ingest asked about.
func (d *directClient) answerEvidence(ctx context.Context, reqs []evidenceRequest) {
	for _, r := range reqs {
		if r.FP == "" || r.URL == "" {
			continue
		}
		d.mu.Lock()
		det := d.details[r.FP]
		d.mu.Unlock()
		if det == nil {
			// Aged out. The claim expires and ingest asks again later.
			continue
		}
		_ = uploadEvidence(ctx, d.client, r.URL, buildBundleWith(d.policy.Load(), r.FP, d.cfg.service, det))
	}
}

// buildBundle redacts a detail for egress exactly as the sidecar does in
// strict mode. Identity is never part of a detail, so never part of this.
func buildBundle(fp, service string, det *detail) evidenceBundle {
	return buildBundleWith(nil, fp, service, det)
}

// buildBundleWith is buildBundle under learned rules p, as the sidecar's
// answerEvidenceRequests applies them: the template gets the rule stored
// under the reported fingerprint ('none' sends no text), the exemplar gets
// the line rules.
func buildBundleWith(p *egressPolicy, fp, service string, det *detail) evidenceBundle {
	template := ""
	rule := ""
	if p != nil {
		rule = p.shapes[fp]
	}
	if text, ok := p.redactBody(det.text, rule); ok {
		template = scrubText(text)
	}
	return evidenceBundle{
		V: 1, FP: fp, Kind: det.kind,
		Template:  template,
		Service:   service,
		Exemplars: []string{egressLineWith(p, service, det.exemplar)},
		Frames:    det.frames,
	}
}

// uploadEvidence PUTs a bundle to the presigned URL. No ingest key: the URL
// carries its own authorization, and ours must not leak to a storage host.
func uploadEvidence(ctx context.Context, client *http.Client, url string, bundle evidenceBundle) error {
	payload, err := json.Marshal(bundle)
	if err != nil {
		return errorf("encode evidence: %v", err)
	}
	status, _, err := doWithRetry(ctx, client, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("content-type", "application/json")
		return req, nil
	})
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return errorf("evidence upload answered HTTP %d", status)
	}
	return nil
}

// doWithRetry makes a request, and once more after a jittered pause if the
// first attempt failed in a way a retry could fix (network error, 429, 5xx).
// Then it gives up: discarded, never queued.
func doWithRetry(ctx context.Context, client *http.Client, build func(context.Context) (*http.Request, error)) (int, []byte, error) {
	var (
		status int
		body   []byte
		err    error
	)
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			if !sleepCtx(ctx, jitter()) {
				return 0, nil, ctx.Err()
			}
		}
		status, body, err = doOnce(ctx, client, build)
		if err == nil && status != http.StatusTooManyRequests && status < 500 {
			return status, body, nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	if err != nil {
		return 0, nil, err
	}
	return status, body, nil
}

func doOnce(ctx context.Context, client *http.Client, build func(context.Context) (*http.Request, error)) (int, []byte, error) {
	req, err := build(ctx)
	if err != nil {
		return 0, nil, errorf("build request: %v", err)
	}
	res, err := client.Do(req)
	if err != nil {
		return 0, nil, errorf("ingest unreachable: %v", redactURLError(err))
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	return res.StatusCode, body, nil
}

// redactURLError drops the query from a *url.Error: a presigned URL's query
// is a credential.
func redactURLError(err error) error {
	msg := err.Error()
	if i := strings.Index(msg, "?"); i >= 0 {
		end := strings.IndexAny(msg[i:], "\": ")
		if end < 0 {
			msg = msg[:i]
		} else {
			msg = msg[:i] + msg[i+end:]
		}
	}
	return errors.New(msg)
}

func jitter() time.Duration {
	span := retryJitterMax - retryJitterMin
	if span <= 0 {
		return retryJitterMin
	}
	return retryJitterMin + time.Duration(mrand.Int64N(int64(span)))
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// closeDirect stops the direct transport after counting everything queued
// and sending one final flush, bounded by ctx and by exitFlushBound.
func closeDirect(ctx context.Context) error {
	d := curDirect.Swap(nil)
	if d == nil {
		return nil
	}
	d.closing.Do(func() { close(d.stop) })
	// Never launched: nothing queued, nothing to flush.
	launched := true
	d.start.Do(func() {
		launched = false
		close(d.aggDone)
		close(d.tickDone)
	})
	if !launched {
		return nil
	}

	select {
	case <-d.aggDone:
	case <-ctx.Done():
		return ctx.Err()
	}

	fctx, cancel := context.WithTimeout(ctx, exitFlushBound)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- errorf("final flush panicked")
			}
		}()
		done <- d.flushCycle(fctx)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Test sends one synthetic exception straight to ingest and reports the
// result, so an operator can check a DSN without waiting for a flush:
//
//	if err := devbench.Test(ctx); err != nil {
//	    log.Fatal(err) // no DSN, the key was rejected, or ingest is unreachable
//	}
//
// It asks ingest whether this DSN's key is valid (POST /v1/check) and
// records nothing: it used to send a synthetic exception, which became a real
// issue on a new customer's punch list.
func Test(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errorf("self-test panicked: %v", r)
		}
	}()
	if ctx == nil {
		ctx = context.Background()
	}

	c := activeConfig()
	switch c.mode {
	case modeOff:
		if c.problem != "" {
			return errors.New(c.problem)
		}
		return errorf("reporting is disabled")
	case modeSidecar:
		return errorf("no DSN configured: set %s or Options.DSN (without one, reports go to the local sidecar)", EnvDSN)
	}

	d := newDirectClient(c)
	status, _, err := doWithRetry(ctx, d.client, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/check", strings.NewReader("{}"))
		if err != nil {
			return nil, err
		}
		req.Header.Set("content-type", "application/json")
		req.Header.Set("X-ADT-Key", c.key)
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
