package devbench

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Session tokens prove a browser was served by the customer's real backend.
//
// The ingest key is public — it ships in a JavaScript bundle, so anyone can
// read it and post with it. A session token closes that gap without asking the
// customer to write anything: the middleware they already install for trace
// propagation mints one and injects it into HTML responses, and the browser
// SDK reads it back.
//
// What it deliberately does *not* do is identify a user. It only asserts "this
// page was served by the real application", which is the thing that separates a
// genuine sensor from a bot hitting the endpoint directly — and, unlike user
// identity, it is something middleware can know without understanding the
// customer's authentication at all. A subject can be attached optionally for
// per-user attribution.
//
// Honest limits: an attacker who scripts a page load can scrape a token, so
// this raises the cost from "copy a key once, spam forever" to "fetch a fresh
// token on a schedule". And if the customer's application is compromised, an
// attacker mints tokens freely — at which point ADT receiving junk is the least
// of anyone's problems.
const (
	// SessionHeader carries the token from the browser to ingest.
	SessionHeader = "x-adt-session"

	// SessionMetaName is the meta tag the middleware injects and the browser
	// SDK reads.
	SessionMetaName = "adt-session"

	tokenPrefix = "adts1"

	// DefaultSessionTTL is deliberately long.
	//
	// The token asserts only "served by the real app", so a long life costs
	// little, and a single-page application does full page loads rarely — a
	// short TTL would expire mid-session and force a refresh endpoint, which is
	// more surface in the customer's app for no security gain.
	DefaultSessionTTL = time.Hour

	// maxSubjectLen bounds the optional subject, so a token stays small enough
	// to sit in a meta tag and a header without thought.
	maxSubjectLen = 128
)

var (
	// ErrSessionExpired means the token was valid but is past its expiry.
	ErrSessionExpired = errors.New("adt: session token expired")
	// ErrSessionInvalid covers every other failure. Deliberately one error:
	// a malformed token, a wrong signature, and a token minted for another
	// tenant are the same answer to a caller, and distinguishing them tells
	// someone probing which guess was closest.
	ErrSessionInvalid = errors.New("adt: session token invalid")
)

// SessionToken is a verified token's contents.
type SessionToken struct {
	Tenant  string
	Subject string
	Expires time.Time
}

// MintSession creates a token for a tenant.
//
// secret is the tenant's session signing secret, shared between the customer's
// backend and ingest. subject is optional: pass a stable user or session
// identifier for per-user attribution, or leave it empty.
func MintSession(secret, tenant, subject string, ttl time.Duration) (string, error) {
	if strings.TrimSpace(secret) == "" {
		return "", errors.New("adt: session secret is required")
	}
	if strings.TrimSpace(tenant) == "" {
		return "", errors.New("adt: tenant is required")
	}
	if len(subject) > maxSubjectLen {
		subject = subject[:maxSubjectLen]
	}
	if strings.ContainsAny(tenant, "|") || strings.ContainsAny(subject, "|") {
		// The payload is pipe-delimited, so a pipe in a field would let a
		// crafted subject forge a different tenant.
		return "", errors.New("adt: tenant and subject must not contain '|'")
	}
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}

	payload := fmt.Sprintf("%s|%d|%s", tenant, time.Now().Add(ttl).Unix(), subject)
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))

	return tokenPrefix + "." + encoded + "." + signPayload(secret, encoded), nil
}

// VerifySession checks a token and returns its contents.
func VerifySession(secret, token string) (*SessionToken, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, ErrSessionInvalid
	}

	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 || parts[0] != tokenPrefix {
		return nil, ErrSessionInvalid
	}

	// Constant time: a signature check that leaks timing is one that can be
	// brute-forced a byte at a time.
	if !hmac.Equal([]byte(parts[2]), []byte(signPayload(secret, parts[1]))) {
		return nil, ErrSessionInvalid
	}

	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, ErrSessionInvalid
	}

	fields := strings.SplitN(string(raw), "|", 3)
	if len(fields) != 3 {
		return nil, ErrSessionInvalid
	}

	expUnix, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return nil, ErrSessionInvalid
	}
	expires := time.Unix(expUnix, 0)

	// Expiry is checked after the signature, so an unsigned token can never
	// reveal whether a guessed expiry was plausible.
	if time.Now().After(expires) {
		return nil, ErrSessionExpired
	}

	return &SessionToken{Tenant: fields[0], Subject: fields[2], Expires: expires}, nil
}

func signPayload(secret, encodedPayload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(encodedPayload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
