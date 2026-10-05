package devbench

import (
	"context"
	"reflect"
	"unicode/utf8"
)

const (
	// maxMessage is the spec's bound on `message`, in characters.
	maxMessage = 2000
	// maxSymbol bounds `symbol` so one report can never approach the
	// sidecar's line limit. Symbols are names, not prose.
	maxSymbol = 1000
	// maxUnwrap bounds how far errorType follows a chain, in case one cycles.
	maxUnwrap = 32
)

// anonymousErrorTypes say how an error was built, not what went wrong.
//
// Every errors.New and every fmt.Errorf without %w is an *errors.errorString;
// every fmt.Errorf with %w is a *fmt.wrapError. Grouping on those would put
// every wrapped failure at a site into one bucket, which is what Sentry's Go
// SDK does when it reports the outermost %T.
var anonymousErrorTypes = map[string]bool{
	"*errors.errorString": true,
	"*errors.joinError":   true,
	"*fmt.wrapError":      true,
	"*fmt.wrapErrors":     true,
}

// errorType names err for the `error` field, which is part of the
// fingerprint.
//
// The rule: walk the Unwrap chain from the outside in and take the first type
// that is not an anonymous wrapper (anonymousErrorTypes). So
//
//	fmt.Errorf("persist: %w", &pgconn.PgError{...})   -> "*pgconn.PgError"
//	&url.Error{Err: &net.OpError{...}}                -> "*url.Error"
//	fmt.Errorf("load: %w", fs.ErrNotExist)            -> "*errors.errorString"
//
// Outermost-meaningful rather than innermost: the innermost error is often a
// syscall.Errno or a sentinel shared by unrelated failures, while the first
// named type is the one the code that failed chose to return. When every
// layer is anonymous the innermost type is used, which is as good as Go can
// say. For a multi-error (errors.Join, several %w) the first branch is
// followed.
//
// %T formatting is reflect.TypeOf(err).String(), so types read the way a Go
// programmer writes them.
func errorType(err error) (name string) {
	if err == nil {
		return ""
	}
	defer func() {
		// A custom Unwrap that panics (a nil receiver, say) still leaves the
		// best name found so far.
		_ = recover()
	}()

	for depth := 0; err != nil && depth < maxUnwrap; depth++ {
		name = reflect.TypeOf(err).String()
		if !anonymousErrorTypes[name] {
			return name
		}
		switch u := err.(type) {
		case interface{ Unwrap() error }:
			err = u.Unwrap()
		case interface{ Unwrap() []error }:
			errs := u.Unwrap()
			if len(errs) == 0 {
				return name
			}
			err = errs[0]
		default:
			return name
		}
	}
	return name
}

// errorMessage is err.Error(), bounded, and empty if Error itself panics: the
// type and site are the fingerprint, the message is detail.
func errorMessage(err error) (msg string) {
	defer func() {
		if recover() != nil {
			msg = ""
		}
	}()
	return truncateMessage(err.Error(), maxMessage)
}

// truncateMessage cuts s to at most n characters, marking the cut with an
// ellipsis inside the limit.
func truncateMessage(s string, n int) string {
	if len(s) <= n || utf8.RuneCountInString(s) <= n {
		return s
	}
	return truncateRunes(s, n-1) + "…"
}

// traceField is the `trace` field for a report made under ctx.
func traceField(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if t, ok := FromContext(ctx); ok {
		return t.String()
	}
	return ""
}
