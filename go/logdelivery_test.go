package devbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Answering need_logs, end to end inside the SDK: a real application
// handler logs through slog with r.Context() (or the std log package), the
// stand-in ingest asks for traces on its flush response and accepts POST
// /v1/logs with ingest's strictness, and the assertions are on the bytes
// that arrived there. internal/ingest/go_logs_e2e_test.go runs the real
// ingest and Postgres.

const (
	pairPublic = "pub_live_1a2b"
	pairSecret = "srv_live_9z8y"
)

// useLogs configures direct mode with a pair DSN against f, with an empty
// line store.
func useLogs(t *testing.T, f *fakeIngest) {
	t.Helper()
	resetSDK(t)
	captured.reset()
	t.Cleanup(captured.reset)
	dsn := strings.Replace(f.srv.URL, "://", "://"+pairPublic+":"+pairSecret+"@", 1)
	if err := Init(Options{DSN: dsn, Service: "billing"}); err != nil {
		t.Fatalf("Init: %v", err)
	}
}

// flushAsking makes one flush whose response asks for keys, and returns
// the slices delivered by it: trace -> lines. A handled failure is reported
// first because ingest only answers a flush that carries counts.
func flushAsking(t *testing.T, f *fakeIngest, keys ...string) (map[string][]string, []gotLogs) {
	t.Helper()
	_, before := f.delivered()
	f.askLogs(keys...)
	ReportHandled(context.Background(), errors.New("poll"), "logs.Poll")
	flushNow(t)
	_, all := f.delivered()
	fresh := all[len(before):]
	out := map[string][]string{}
	for _, l := range fresh {
		for _, s := range l.Body.Slices {
			out[s.Trace] = append(out[s.Trace], s.Lines...)
		}
	}
	return out, fresh
}

// stdLog returns a std logger writing through LogWriter, no flags, so the
// line text is exactly what the test writes.
func stdLog() *log.Logger { return log.New(LogWriter(io.Discard), "", 0) }

func TestLogs_ARealHandlersLinesAreDeliveredRedactedUnderTheirTrace(t *testing.T) {
	f := newFakeIngest(t)
	useLogs(t, f)
	vf := loadRedaction(t)

	var appOut strings.Builder
	logger := slog.New(NewLogHandler(slog.NewTextHandler(&appOut, nil)))
	redact := append(append(append([]redactionVector{}, vf.Shared...), vf.ServerOnly...), vf.Prose...)

	app := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.InfoContext(r.Context(), "charge declined by gateway", "deal", 9912, "customer", "Alice Smith")
		for _, v := range redact {
			logger.WarnContext(r.Context(), v.In)
		}
		for _, v := range vf.MustSurvive {
			logger.InfoContext(r.Context(), v.In)
		}
		w.WriteHeader(http.StatusPaymentRequired)
	}))
	other := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.InfoContext(r.Context(), "a different user's request")
	}))
	app.ServeHTTP(httptest.NewRecorder(), tracedRequest("v1/sessD/actD/0"))
	other.ServeHTTP(httptest.NewRecorder(), tracedRequest("v1/sessO/actO/0"))

	got, deliveries := flushAsking(t, f, "sessD/actD", "sessNone/actNone")

	if len(deliveries) == 0 {
		t.Fatal("nothing was delivered to /v1/logs")
	}
	for _, d := range deliveries {
		if d.Key != pairSecret {
			t.Errorf("delivery X-ADT-Key = %q, want the DSN's secret", d.Key)
		}
		for _, s := range d.Body.Slices {
			if s.Trace != "sessD/actD" {
				t.Errorf("delivered a slice for %q, which was not asked for or has no lines here", s.Trace)
			}
		}
	}
	lines := got["sessD/actD"]
	if want := 1 + len(redact) + len(vf.MustSurvive); len(lines) != want {
		t.Fatalf("delivered %d lines, want %d", len(lines), want)
	}
	text := strings.Join(lines, "\n")

	// Right trace, lifted to the front as the sidecar does.
	for _, l := range lines {
		if !strings.HasPrefix(l, "[v1/sessD/actD/0] ") {
			t.Errorf("line not led by its trace: %q", l)
		}
	}
	if !strings.Contains(lines[0], "INFO charge declined by gateway") {
		t.Errorf("the diagnostic line lost its words: %q", lines[0])
	}
	if strings.Contains(text, "different user") {
		t.Errorf("another trace's line was delivered:\n%s", text)
	}
	// PII from the shared vectors is gone.
	checked := 0
	for _, v := range redact {
		for _, secret := range removedTokens(v.In, v.Out) {
			checked++
			if strings.Contains(text, secret) {
				t.Errorf("%s: %q was delivered", v.Name, secret)
			}
		}
	}
	for _, leaked := range []string{"Alice Smith", "9912"} {
		if strings.Contains(text, leaked) {
			t.Errorf("%q was delivered:\n%s", leaked, text)
		}
	}
	if checked == 0 {
		t.Fatal("no must-redact token was checked")
	}
	// Diagnostics survive.
	for _, v := range vf.MustSurvive {
		for _, word := range strings.Fields(v.Out) {
			if len(word) < 4 || strings.ContainsAny(word, "0123456789\"'=") {
				continue
			}
			if !strings.Contains(text, word) {
				t.Errorf("%s: %q was destroyed on the way out", v.Name, word)
			}
		}
	}
	// The application's own log output is untouched by any of this.
	if !strings.Contains(appOut.String(), "Alice Smith") {
		t.Errorf("the application's own log output was redacted:\n%s", appOut.String())
	}

	// Retained until aged out: a later request gets the lines again.
	again, _ := flushAsking(t, f, "sessD/actD")
	if len(again["sessD/actD"]) != len(lines) {
		t.Errorf("asked again: %d lines, want %d", len(again["sessD/actD"]), len(lines))
	}
}

// A key with no lines in this process gets nothing — not an empty slice,
// and no request at all when nothing matched.
func TestLogs_NothingIsSentForKeysWithNoLines(t *testing.T) {
	f := newFakeIngest(t)
	useLogs(t, f)
	stdLog().Printf("[v1/sessHeld/act1/0] something else entirely")

	_, deliveries := flushAsking(t, f, "sessUnknown/act1", "sessOther/act9")
	if len(deliveries) != 0 {
		t.Errorf("deliveries for keys with no lines: %+v", deliveries)
	}
	if _, flushes, _ := f.snapshot(); len(flushes) == 0 {
		t.Fatal("precondition: no flush happened, so need_logs was never seen")
	}
}

// Outside direct mode nothing is held, so nothing is ever sent; and a flush
// response without need_logs sends nothing either.
func TestLogs_NoRequestNoDelivery(t *testing.T) {
	f := newFakeIngest(t)
	useLogs(t, f)
	stdLog().Printf("[v1/sessQ/act1/0] held but never asked for")
	if _, deliveries := flushAsking(t, f); len(deliveries) != 0 {
		t.Errorf("delivered without being asked: %+v", deliveries)
	}
}

func TestLogs_SliceAndDeliveryBounds(t *testing.T) {
	f := newFakeIngest(t)
	useLogs(t, f)
	lg := stdLog()
	for i := 0; i < 600; i++ {
		lg.Printf("[v1/sessB/act1/0] step %04d %s", i, strings.Repeat("x", 50))
	}
	long := strings.Repeat("y", 5000)
	lg.Printf("[v1/sessB/act2/0] %s", long)
	keys := []string{"sessB/act1", "sessB/act2"}
	for i := 0; i < 30; i++ {
		k := fmt.Sprintf("sessMany/act%d", i)
		lg.Printf("[v1/%s/0] one line", k)
		keys = append(keys, k)
	}

	got, deliveries := flushAsking(t, f, keys...)

	l1 := got["sessB/act1"]
	if len(l1) != maxDeliveredLines {
		t.Errorf("slice of 600 lines delivered %d, want %d", len(l1), maxDeliveredLines)
	}
	if len(l1) > 0 && !strings.Contains(l1[len(l1)-1], "step <num>") {
		t.Errorf("last line = %q", l1[len(l1)-1])
	}
	for k, lines := range got {
		size := 0
		for _, l := range lines {
			if len(l) > logMaxLineBytes {
				t.Errorf("%s: a line of %d bytes, cap %d", k, len(l), logMaxLineBytes)
			}
			size += len(l)
		}
		if size > maxDeliveredSliceBytes {
			t.Errorf("%s: %d bytes in one slice, cap %d", k, size, maxDeliveredSliceBytes)
		}
	}
	if len(got["sessB/act2"]) != 1 {
		t.Errorf("the over-long line was not delivered (truncated): %d", len(got["sessB/act2"]))
	}
	if len(got) != 32 {
		t.Errorf("delivered %d slices, want 32", len(got))
	}
	if len(deliveries) < 2 {
		t.Errorf("32 slices went in %d request(s); ingest accepts at most 25 per request", len(deliveries))
	}
	for _, d := range deliveries {
		if d.Size > maxFlushBodyBytes {
			t.Errorf("a delivery of %d bytes", d.Size)
		}
	}
}

// Shape rules are keyed by the sidecar's shape fingerprint of the
// trace-stripped line. Computed here with the SDK's fingerprint code, which
// testdata/fingerprint_vectors.json holds equal to the backend's.
func shapeOf(t *testing.T, line string) string {
	t.Helper()
	body := egressTraceRe.ReplaceAllString(line, "<trace>")
	fp, _, err := computeFingerprint(fpSignal{Kind: "log_template", Source: "sidecar", Service: "billing", Message: body})
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

func rulesJSON(t *testing.T, rules ...egressRule) string {
	t.Helper()
	if rules == nil {
		rules = []egressRule{}
	}
	raw, err := json.Marshal(rules)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func fieldRule(name string) egressRule { return egressRule{Kind: ruleField, Target: name} }

// Learned rules, with the cases internal/sidecar/learned_test.go holds the
// sidecar to (strict mode: the SDK's only mode).
func TestLogs_LearnedRules(t *testing.T) {
	deliverOne := func(t *testing.T, f *fakeIngest, key string) string {
		t.Helper()
		got, _ := flushAsking(t, f, key)
		return strings.Join(got[key], "\n")
	}

	t.Run("none stops a shape and leaves a placeholder", func(t *testing.T) {
		f := newFakeIngest(t)
		useLogs(t, f)
		leaky := "[v1/sessL/act1/0] Approved loan for Bob"
		stdLog().Print(leaky)
		stdLog().Print("[v1/sessL/act1/0] Completed 500 Internal Server Error in 31ms")

		if before := deliverOne(t, f, "sessL/act1"); !strings.Contains(before, "Bob") {
			t.Fatalf("precondition: the heuristic already removes the single-word surname:\n%s", before)
		}
		fp := shapeOf(t, leaky)
		f.setRedaction(rulesJSON(t, egressRule{FP: fp, Egress: egressNone}))
		payload := deliverOne(t, f, "sessL/act1")
		if strings.Contains(payload, "Bob") || strings.Contains(payload, "Approved loan") {
			t.Errorf("a shape marked 'none' still sent its content:\n%s", payload)
		}
		if !strings.Contains(payload, withheld(fp)) {
			t.Errorf("no placeholder naming the shape:\n%s", payload)
		}
		if !strings.Contains(payload, "Internal Server Error") {
			t.Errorf("stopping one shape dropped the slice:\n%s", payload)
		}
	})

	t.Run("full relaxes the name heuristic only", func(t *testing.T) {
		f := newFakeIngest(t)
		useLogs(t, f)
		relaxed := "[v1/sessR/act1/0] routing payment through Acme Ledger"
		base := "[v1/sessR/act1/0] calling Acme Ledger with api_key=sk_live_abcdef123456 for alice@example.com"
		stdLog().Print(relaxed)
		stdLog().Print(base)
		if before := deliverOne(t, f, "sessR/act1"); strings.Contains(before, "Acme Ledger") {
			t.Fatalf("precondition: the heuristic does not mask the phrase:\n%s", before)
		}
		f.setRedaction(rulesJSON(t,
			egressRule{FP: shapeOf(t, relaxed), Egress: egressFull},
			egressRule{FP: shapeOf(t, base), Egress: egressFull}))
		payload := deliverOne(t, f, "sessR/act1")
		if strings.Count(payload, "Acme Ledger") != 2 {
			t.Errorf("'full' did not apply:\n%s", payload)
		}
		for _, s := range []string{"sk_live_abcdef123456", "alice@example.com"} {
			if strings.Contains(payload, s) {
				t.Errorf("'full' lifted base redaction; %q delivered:\n%s", s, payload)
			}
		}
	})

	t.Run("field rule masks the value in every form", func(t *testing.T) {
		f := newFakeIngest(t)
		useLogs(t, f)
		forms := []string{`license=ZQ77XK`, `license: ZQ77XK`, `"license": "ZQ77XK"`,
			`"license"=>"ZQ77XK"`, `:license => "ZQ77XK"`, `License=ZQ77XK`}
		for _, form := range forms {
			stdLog().Printf("[v1/sessF/act1/0] verified applicant %s for review", form)
		}
		if before := deliverOne(t, f, "sessF/act1"); strings.Count(before, "ZQ77XK") < 3 {
			t.Fatalf("precondition: base redaction already removes the value, so this shows nothing:\n%s", before)
		}
		f.setRedaction(rulesJSON(t, fieldRule("license")))
		payload := deliverOne(t, f, "sessF/act1")
		if strings.Contains(payload, "ZQ77XK") {
			t.Errorf("the field rule left the value:\n%s", payload)
		}
		if strings.Count(payload, "verified applicant") != len(forms) {
			t.Errorf("masking took more than the value:\n%s", payload)
		}
	})

	t.Run("field rule matches only that field", func(t *testing.T) {
		f := newFakeIngest(t)
		useLogs(t, f)
		stdLog().Print("[v1/sessH/act1/0] licensed=true driver_license=K99 license_type=commercial")
		f.setRedaction(rulesJSON(t, fieldRule("license")))
		payload := deliverOne(t, f, "sessH/act1")
		for _, keep := range []string{"licensed=true", "driver_license=K99", "license_type=commercial"} {
			if !strings.Contains(payload, keep) {
				t.Errorf("%q masked by a rule for another field:\n%s", keep, payload)
			}
		}
	})

	t.Run("full on a shape does not lift a field rule", func(t *testing.T) {
		f := newFakeIngest(t)
		useLogs(t, f)
		line := "[v1/sessI/act1/0] Acme Ledger checked license=ZQ77XK"
		stdLog().Print(line)
		f.setRedaction(rulesJSON(t, egressRule{FP: shapeOf(t, line), Egress: egressFull}, fieldRule("license")))
		payload := deliverOne(t, f, "sessI/act1")
		if strings.Contains(payload, "ZQ77XK") || !strings.Contains(payload, "Acme Ledger") {
			t.Errorf("payload:\n%s", payload)
		}
	})

	t.Run("term rule masks the literal", func(t *testing.T) {
		f := newFakeIngest(t)
		useLogs(t, f)
		stdLog().Print("[v1/sessJ/act1/0] escalated to Whitfield for approval")
		if before := deliverOne(t, f, "sessJ/act1"); !strings.Contains(before, "Whitfield") {
			t.Fatalf("precondition: already removed without the rule:\n%s", before)
		}
		f.setRedaction(rulesJSON(t, egressRule{Kind: ruleTerm, Target: "Whitfield"}))
		payload := deliverOne(t, f, "sessJ/act1")
		if strings.Contains(payload, "Whitfield") || !strings.Contains(payload, "for approval") {
			t.Errorf("term rule: %s", payload)
		}
	})

	t.Run("malformed rules are ignored, valid ones still apply", func(t *testing.T) {
		f := newFakeIngest(t)
		useLogs(t, f)
		stdLog().Print("[v1/sessK/act1/0] checked license=ZQ77XK ok")
		if before := deliverOne(t, f, "sessK/act1"); !strings.Contains(before, "ZQ77XK") {
			t.Fatalf("precondition: already removed without the rule:\n%s", before)
		}
		f.setRedaction(rulesJSON(t,
			fieldRule(`.*`), fieldRule(`lic(ense`), fieldRule(""),
			egressRule{Kind: ruleTerm, Target: "ok"},
			egressRule{Kind: "regex", Target: ".*"},
			fieldRule("license")))
		payload := deliverOne(t, f, "sessK/act1")
		if strings.Contains(payload, "ZQ77XK") || !strings.Contains(payload, "checked") || !strings.Contains(payload, " ok") {
			t.Errorf("payload:\n%s", payload)
		}
	})

	t.Run("replaced not merged; absent leaves rules in force", func(t *testing.T) {
		f := newFakeIngest(t)
		useLogs(t, f)
		line := "[v1/sessM/act1/0] Approved loan for Bob"
		stdLog().Print(line)
		f.setRedaction(rulesJSON(t, egressRule{FP: shapeOf(t, line), Egress: egressNone}))
		if p := deliverOne(t, f, "sessM/act1"); strings.Contains(p, "Bob") {
			t.Fatalf("precondition: the rule never applied:\n%s", p)
		}
		f.setRedaction("") // field absent: a failed lookup on ingest's side
		if p := deliverOne(t, f, "sessM/act1"); strings.Contains(p, "Bob") {
			t.Errorf("a response without a rule set dropped the rules in force:\n%s", p)
		}
		f.setRedaction("[]") // explicitly empty: every rule withdrawn
		if p := deliverOne(t, f, "sessM/act1"); !strings.Contains(p, "Bob") {
			t.Errorf("a withdrawn rule was still enforced:\n%s", p)
		}
	})
}

// Learned rules reach evidence bundles too, as the sidecar applies them in
// answerEvidenceRequests — and a rule arriving with the evidence request
// applies to that upload.
func TestLogs_LearnedRulesApplyToEvidence(t *testing.T) {
	f := newFakeIngest(t)
	useLogs(t, f)
	f.mu.Lock()
	f.askEvidence = true
	f.mu.Unlock()
	f.setRedaction(rulesJSON(t, fieldRule("license"), egressRule{Kind: ruleTerm, Target: "Whitfield"}))

	CaptureException(context.Background(), errors.New("escalated to Whitfield: checked license=ZQ77XK"))
	flushNow(t)

	_, _, puts := f.snapshot()
	if len(puts) != 1 {
		t.Fatalf("puts = %d, want 1", len(puts))
	}
	raw := string(puts[0].Raw)
	if strings.Contains(raw, "ZQ77XK") || strings.Contains(raw, "Whitfield") {
		t.Errorf("a learned rule was not applied to the evidence bundle:\n%s", raw)
	}
	if !strings.Contains(raw, "escalated to") {
		t.Errorf("the bundle lost the diagnostic words:\n%s", raw)
	}
}

// The final flush at Close answers too: a request arriving on it is not
// lost at shutdown.
func TestLogs_AnsweredOnClose(t *testing.T) {
	f := newFakeIngest(t)
	useLogs(t, f)
	stdLog().Print("[v1/sessC/act1/0] last words before shutdown")
	f.askLogs("sessC/act1")
	ReportHandled(context.Background(), errors.New("x"), "logs.Close")
	closeNow(t)
	got, _ := f.delivered()
	if len(got["sessC/act1"]) != 1 {
		t.Errorf("delivered on close: %v", got)
	}
}
