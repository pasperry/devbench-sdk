package devbench

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
)

// Frame is one stack frame of a reported exception, innermost first.
type Frame struct {
	Function string `json:"function"`
	File     string `json:"file"`
	Line     int    `json:"line"`
}

// maxFrames is the spec's bound on `frames`.
const maxFrames = 50

// requestScope is per-request state that code deeper in the request can
// update and Middleware can still read when a panic unwinds back to it.
//
// A context only flows down, so a WithUser inside a handler would be
// invisible to the recover() in Middleware without this — the same reason
// Sentry's Go SDK keeps a mutable scope on its hub.
type requestScope struct {
	// req is the innermost *http.Request ADT has seen for this request.
	// ServeMux records the matched pattern on the request it is handed, so
	// the route is read from here after the fact.
	req  atomic.Pointer[http.Request]
	user atomic.Pointer[User]

	mu       sync.Mutex
	reported []error // errors already sent as exceptions in this request
}

type scopeKey struct{}

func scopeFrom(ctx context.Context) *requestScope {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(scopeKey{}).(*requestScope)
	return s
}

// noteRequest records r as the innermost request seen, for its route pattern.
func noteRequest(r *http.Request) {
	if s := scopeFrom(r.Context()); s != nil {
		s.req.Store(r)
	}
}

// symbol is the route pattern ServeMux matched ("GET /deals/{id}"), or ""
// when there is none — never the raw path, which carries ids and would split
// one failure into one group per record.
func (s *requestScope) symbol() string {
	if s == nil {
		return ""
	}
	if r := s.req.Load(); r != nil {
		return truncateRunes(routePattern(r), maxSymbol)
	}
	return ""
}

// reportPanic sends a panic recovered by Middleware as an exception. It is
// called from the deferred function, so the panicking frames are still live.
func reportPanic(ctx context.Context, v any) {
	defer func() { _ = recover() }()

	// Captured first, while the panicking frames are still on the stack.
	frames := captureFrames(true)

	scope := scopeFrom(ctx)
	if err, ok := v.(error); ok && scope.alreadyReported(err) {
		return
	}

	m := message{
		V:       1,
		Kind:    "exception",
		Context: "request",
		Handled: new(bool),
		Symbol:  scope.symbol(),
		Frames:  frames,
		Trace:   traceField(ctx),
		User:    userField(ctx),
	}
	if err, ok := v.(error); ok {
		m.Error = errorType(err)
		m.Message = errorMessage(err)
	} else {
		m.Error = "panic"
		m.Message = truncateMessage(safeSprint(v), maxMessage)
	}
	send(m)
}

// CaptureException reports err to the sidecar as an `exception` with context
// "explicit" — the equivalent of Sentry's CaptureException.
//
//	if err := reconcile(ctx, batch); err != nil {
//	    devbench.CaptureException(ctx, err)
//	    return err
//	}
//
// Unlike ReportHandled, which marks a site that deliberately absorbed a
// failure, this is for an error worth seeing as an error in its own right.
// Frames are the caller's stack; symbol is the request's route pattern when
// called under Middleware.
//
// One error is reported once per request: capturing err and then panicking
// with it sends a single exception, as the spec's dedupe rule requires. Only
// pointer-typed errors are tracked, because a sentinel value such as io.EOF is
// legitimately reported again by a different request.
//
// Never blocks, never panics; a nil err is ignored.
func CaptureException(ctx context.Context, err error) {
	if err == nil {
		return
	}
	defer func() { _ = recover() }()

	frames := captureFrames(false)

	scope := scopeFrom(ctx)
	if !scope.markReported(err) {
		return
	}

	send(message{
		V:       1,
		Kind:    "exception",
		Context: "explicit",
		Handled: &handledTrue,
		Error:   errorType(err),
		Message: errorMessage(err),
		Symbol:  scope.symbol(),
		Frames:  frames,
		Trace:   traceField(ctx),
		User:    userField(ctx),
	})
}

var handledTrue = true

// markReported records err as reported in this request. It returns false if
// it already was, meaning the caller should not send it again.
func (s *requestScope) markReported(err error) bool {
	if s == nil || !isPointer(err) {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.reported {
		if e == err {
			return false
		}
	}
	if len(s.reported) < 64 { // bounded; a request capturing more is unusual
		s.reported = append(s.reported, err)
	}
	return true
}

// alreadyReported reports whether err was sent as an exception earlier in
// this request (see markReported). Only pointer-typed errors are tracked: a
// value is not an identity, and comparing some values panics.
func (s *requestScope) alreadyReported(err error) bool {
	if s == nil || !isPointer(err) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.reported {
		if e == err {
			return true
		}
	}
	return false
}

func isPointer(err error) bool {
	if err == nil {
		return false
	}
	return reflect.TypeOf(err).Kind() == reflect.Pointer
}

func safeSprint(v any) (s string) {
	defer func() {
		if recover() != nil {
			s = fmt.Sprintf("%T", v)
		}
	}()
	return fmt.Sprint(v)
}

// captureFrames returns the current goroutine's stack, innermost first,
// without ADT's own frames, the runtime, or the standard library.
//
// fromPanic starts the stack at the panic site: everything inside
// runtime.gopanic and above (the deferred function, this function) is ADT
// and runtime machinery, not the application.
//
// Standard-library frames are dropped rather than sent: the sidecar's
// fingerprint filter recognises GOROOT only at /usr/local/go, and on any other
// install net/http's frames would be counted as the application's.
// Dependencies keep their absolute paths (…/go/pkg/mod/…, …/vendor/…), which
// that filter does recognise.
func captureFrames(fromPanic bool) []Frame {
	var pcs [128]uintptr
	n := runtime.Callers(2, pcs[:]) // skip runtime.Callers and captureFrames
	iter := runtime.CallersFrames(pcs[:n])

	var all []runtime.Frame
	for {
		f, more := iter.Next()
		all = append(all, f)
		if !more {
			break
		}
	}

	if fromPanic {
		for i, f := range all {
			if f.Function == "runtime.gopanic" {
				all = all[i+1:]
				break
			}
		}
	}

	out := make([]Frame, 0, maxFrames)
	for _, f := range all {
		if len(out) == maxFrames {
			break
		}
		pkg := funcPackage(f.Function)
		if skipFrame(pkg, f) {
			continue
		}
		out = append(out, Frame{Function: f.Function, File: relFile(pkg, f.File), Line: f.Line})
	}
	return out
}

func skipFrame(pkg string, f runtime.Frame) bool {
	switch {
	case f.Function == "":
		return true
	case pkg == "runtime" || strings.HasPrefix(pkg, "runtime/"):
		return true
	case (pkg == selfPkg || strings.HasPrefix(pkg, selfPkg+"/")) && !strings.HasSuffix(f.File, "_test.go"):
		return true // the SDK and its adapters (…/gin); its own tests are application code here
	case isStdlib(pkg):
		return true
	}
	return false
}

// funcPackage extracts the import path from a runtime function name, e.g.
// "github.com/acme/billing/inv.(*Invoicer).Persist" -> "github.com/acme/billing/inv".
func funcPackage(fn string) string {
	slash := strings.LastIndexByte(fn, '/')
	dot := strings.IndexByte(fn[slash+1:], '.')
	if dot < 0 {
		return fn
	}
	return fn[:slash+1+dot]
}

// isStdlib reports whether pkg is part of the Go distribution: by
// convention, import paths whose first element has no dot.
func isStdlib(pkg string) bool {
	if pkg == "main" || inMainModule(pkg) {
		return false
	}
	first, _, _ := strings.Cut(pkg, "/")
	return !strings.Contains(first, ".")
}

func inMainModule(pkg string) bool {
	return mainModule != "" && (pkg == mainModule || strings.HasPrefix(pkg, mainModule+"/"))
}

// relFile makes file relative to the main module's root, when the frame
// belongs to the main module and the path agrees with the package's import
// path. Anything else is left as the runtime reported it.
//
// Works with and without -trimpath: the package's directory within the
// module is its import path minus the module path, whatever the absolute
// prefix in front of it.
func relFile(pkg, file string) string {
	if pkg == "main" {
		pkg = mainPackage
	}
	if !inMainModule(pkg) {
		return file
	}

	rel := strings.TrimPrefix(strings.TrimPrefix(pkg, mainModule), "/")
	slash := strings.LastIndexByte(file, '/')
	if slash < 0 {
		return file
	}
	dir, base := file[:slash], file[slash+1:]

	if rel == "" {
		return base
	}
	if dir == rel || strings.HasSuffix(dir, "/"+rel) {
		return rel + "/" + base
	}
	return file
}

var (
	// selfPkg is this package's import path, read at runtime so that a copy
	// vendored under another path still recognises its own frames.
	selfPkg = funcPackage(runtime.FuncForPC(reflect.ValueOf(pkgAnchor).Pointer()).Name())

	mainModule, mainPackage = buildPaths()
)

// pkgAnchor exists only to have its name looked up.
func pkgAnchor() {}

func buildPaths() (module, mainPkg string) {
	bi, ok := debug.ReadBuildInfo()
	if !ok || bi == nil {
		return "", ""
	}
	return bi.Main.Path, bi.Path
}
