package devbench

import (
	"context"
	"strings"
)

// User is who was affected by a failure (SERVER_SDK_SPEC capability 5) —
// what Sentry's setUser records.
//
// Identity travels in its own `user` field on every report and nowhere else:
// never in a message, a symbol, a fingerprint, or a log line.
type User struct {
	// Email is trimmed and lowercased.
	Email string `json:"email,omitempty"`
	// Account is the application's own tenant/customer identifier.
	Account string `json:"account,omitempty"`
}

// maxUserField bounds each identity field. Longer is truncated, not rejected.
const maxUserField = 255

type userKey struct{}

// WithUser returns a context whose reports carry u.
//
//	ctx = devbench.WithUser(ctx, devbench.User{Email: u.Email, Account: d.ID})
//	r = r.WithContext(ctx)
//
// Both fields are optional; a User with neither clears identity for ctx.
//
// Inside a request wrapped by Middleware it also sets the user for the rest
// of that request, so a panic that unwinds back to Middleware — which holds
// only the context it created — is still attributed. Like Ruby's
// ADT.set_user, that request-wide identity ends with the request.
func WithUser(ctx context.Context, u User) context.Context {
	u = normalizeUser(u)
	if s := scopeFrom(ctx); s != nil {
		s.user.Store(&u)
	}
	return context.WithValue(ctx, userKey{}, u)
}

// UserFromContext returns the identity bound to ctx, if any: the nearest
// WithUser on ctx, else the one set most recently in the request.
func UserFromContext(ctx context.Context) (User, bool) {
	if ctx == nil {
		return User{}, false
	}
	u, ok := ctx.Value(userKey{}).(User)
	if !ok {
		if s := scopeFrom(ctx); s != nil {
			if p := s.user.Load(); p != nil {
				u = *p
			}
		}
	}
	if u == (User{}) {
		return User{}, false
	}
	return u, true
}

func normalizeUser(u User) User {
	return User{
		Email:   truncateRunes(strings.ToLower(strings.TrimSpace(u.Email)), maxUserField),
		Account: truncateRunes(u.Account, maxUserField),
	}
}

// truncateRunes cuts s to at most n characters, never splitting one.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s // n bytes is at most n runes
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// userField is the `user` field for a report made under ctx: nil (omitted)
// when no identity is set.
func userField(ctx context.Context) *User {
	u, ok := UserFromContext(ctx)
	if !ok {
		return nil
	}
	return &u
}
