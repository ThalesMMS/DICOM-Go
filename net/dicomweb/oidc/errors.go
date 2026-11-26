package oidc

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrNotConfigured reports an incomplete or unsafe client configuration.
	ErrNotConfigured = errors.New("oidc: not configured")
	// ErrDiscovery reports unusable OpenID Provider metadata.
	ErrDiscovery = errors.New("oidc: provider discovery failed")
	// ErrLogin reports a failed interactive or device login.
	ErrLogin = errors.New("oidc: login failed")
	// ErrRefresh reports a failed refresh-token exchange.
	ErrRefresh = errors.New("oidc: token refresh failed")
	// ErrRevocation reports a failed token revocation.
	ErrRevocation = errors.New("oidc: token revocation failed")
	// ErrNoRefreshToken reports that no refresh token is available.
	ErrNoRefreshToken = errors.New("oidc: no refresh token")

	// ErrInvalidToken reports a JWT that failed verification. The more
	// specific errors below wrap it.
	ErrInvalidToken = errors.New("oidc: invalid token")
	// ErrMalformedToken reports a token that is not a well-formed compact JWS.
	ErrMalformedToken = fmt.Errorf("%w: malformed", ErrInvalidToken)
	// ErrUnsupportedAlgorithm reports a signature algorithm that is not
	// allowed, including "none" and every HMAC algorithm.
	ErrUnsupportedAlgorithm = fmt.Errorf("%w: unsupported algorithm", ErrInvalidToken)
	// ErrKeyNotFound reports that no key of the key set matches the token.
	ErrKeyNotFound = fmt.Errorf("%w: signing key not found", ErrInvalidToken)
	// ErrInvalidSignature reports a signature that does not verify.
	ErrInvalidSignature = fmt.Errorf("%w: signature mismatch", ErrInvalidToken)
	// ErrTokenExpired reports an exp claim in the past.
	ErrTokenExpired = fmt.Errorf("%w: expired", ErrInvalidToken)
	// ErrTokenNotYetValid reports an nbf or iat claim in the future.
	ErrTokenNotYetValid = fmt.Errorf("%w: not yet valid", ErrInvalidToken)
	// ErrIssuerMismatch reports an iss claim other than the expected issuer.
	ErrIssuerMismatch = fmt.Errorf("%w: issuer mismatch", ErrInvalidToken)
	// ErrAudienceMismatch reports an aud or azp claim that excludes the client.
	ErrAudienceMismatch = fmt.Errorf("%w: audience mismatch", ErrInvalidToken)
	// ErrNonceMismatch reports an ID token whose nonce differs from the request.
	ErrNonceMismatch = fmt.Errorf("%w: nonce mismatch", ErrInvalidToken)
	// ErrSubjectMismatch reports a refreshed ID token or UserInfo response for
	// another subject.
	ErrSubjectMismatch = fmt.Errorf("%w: subject mismatch", ErrInvalidToken)
	// ErrAccessTokenHashMismatch reports an at_hash claim that does not match
	// the access token issued with the ID token.
	ErrAccessTokenHashMismatch = fmt.Errorf("%w: access token hash mismatch", ErrInvalidToken)
)

// ErrorKind classifies a protocol failure without exposing provider text.
type ErrorKind string

const (
	ErrorConfiguration ErrorKind = "configuration"
	ErrorDiscovery     ErrorKind = "discovery"
	ErrorLogin         ErrorKind = "login"
	ErrorRefresh       ErrorKind = "refresh"
	ErrorRevocation    ErrorKind = "revocation"
)

// Error is a redacted protocol failure. ProviderCode is the OAuth "error"
// value only when it is a short token of safe characters; descriptions and
// URIs supplied by the provider are never retained.
type Error struct {
	Kind         ErrorKind
	Operation    string
	StatusCode   int
	ProviderCode string
	// Err is the sentinel of the kind (ErrDiscovery, ErrLogin, ...).
	Err error
	// Cause is an optional non-secret detail such as ErrTokenExpired.
	Cause error
}

func (e *Error) Error() string {
	if e == nil {
		return "oidc: <nil>"
	}
	var b strings.Builder
	b.WriteString("oidc: ")
	operation := strings.TrimSpace(e.Operation)
	if operation == "" {
		operation = string(e.Kind)
	}
	b.WriteString(operation)
	b.WriteString(" failed")
	if e.StatusCode != 0 {
		fmt.Fprintf(&b, " (HTTP %d)", e.StatusCode)
	}
	if e.ProviderCode != "" {
		b.WriteString(" (provider error ")
		b.WriteString(e.ProviderCode)
		b.WriteString(")")
	}
	if e.Cause != nil {
		b.WriteString(": ")
		b.WriteString(e.Cause.Error())
	}
	return b.String()
}

// Unwrap exposes both the kind sentinel and the optional cause to errors.Is.
func (e *Error) Unwrap() []error {
	if e == nil {
		return nil
	}
	out := make([]error, 0, 2)
	if e.Err != nil {
		out = append(out, e.Err)
	}
	if e.Cause != nil {
		out = append(out, e.Cause)
	}
	return out
}

func sentinelFor(kind ErrorKind) error {
	switch kind {
	case ErrorConfiguration:
		return ErrNotConfigured
	case ErrorDiscovery:
		return ErrDiscovery
	case ErrorRefresh:
		return ErrRefresh
	case ErrorRevocation:
		return ErrRevocation
	default:
		return ErrLogin
	}
}

func newError(kind ErrorKind, operation string, status int, providerCode string, cause error) error {
	return &Error{
		Kind:         kind,
		Operation:    operation,
		StatusCode:   status,
		ProviderCode: SafeProviderCode(providerCode),
		Err:          sentinelFor(kind),
		Cause:        safeCause(cause),
	}
}

// contextError keeps cancellation and deadlines distinguishable while
// redacting every other transport failure.
func contextError(kind ErrorKind, operation string, err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	default:
		return newError(kind, operation, 0, "", nil)
	}
}

// safeCause keeps only package verification sentinels; arbitrary errors could
// carry URLs or provider text.
func safeCause(err error) error {
	if err == nil {
		return nil
	}
	for _, known := range []error{
		ErrMalformedToken, ErrUnsupportedAlgorithm, ErrKeyNotFound, ErrInvalidSignature,
		ErrTokenExpired, ErrTokenNotYetValid, ErrIssuerMismatch, ErrAudienceMismatch,
		ErrNonceMismatch, ErrSubjectMismatch, ErrAccessTokenHashMismatch, ErrInvalidToken,
		ErrNoRefreshToken,
	} {
		if errors.Is(err, known) {
			return known
		}
	}
	return nil
}

// SafeProviderCode returns an OAuth error code when it is at most 64
// characters of [A-Za-z0-9_.-]; anything else is discarded.
func SafeProviderCode(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 {
		return ""
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || strings.ContainsRune("_-.", r) {
			continue
		}
		return ""
	}
	return value
}
