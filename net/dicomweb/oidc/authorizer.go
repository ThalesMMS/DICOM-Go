package oidc

import (
	"context"
	"net/http"
	"strings"

	"github.com/ThalesMMS/dicom-go/net/dicomweb"
)

// AccessPolicy decides whether a verified token may perform operation. Return
// dicomweb.ErrForbidden to refuse; nil allows the request.
type AccessPolicy func(ctx context.Context, token *IDToken, request *http.Request, operation dicomweb.Operation) error

type verifiedTokenKey struct{}

// TokenFromContext returns the verified bearer token that Authorizer attached
// to the request context, when the policy needs it again downstream.
func TokenFromContext(ctx context.Context) (*IDToken, bool) {
	token, ok := ctx.Value(verifiedTokenKey{}).(*IDToken)
	return token, ok
}

// Authorizer returns a dicomweb.Authorizer that requires an
// "Authorization: Bearer <JWT>" header whose token v verifies (signature,
// issuer, audience, expiry), then applies policy. A missing or invalid token
// yields dicomweb.ErrUnauthorized (HTTP 401); policy errors pass through.
// A nil policy allows every verified token.
//
// Configure the Verifier with the resource audience the provider puts in its
// JWT access tokens (RFC 9068), not the client ID of a viewer.
func (v *Verifier) Authorizer(policy AccessPolicy) dicomweb.Authorizer {
	return func(ctx context.Context, request *http.Request, operation dicomweb.Operation) error {
		raw, ok := BearerToken(request)
		if !ok {
			return dicomweb.ErrUnauthorized
		}
		token, err := v.Verify(ctx, raw)
		if err != nil {
			if ctxErr := contextOrBackground(ctx).Err(); ctxErr != nil {
				return ctxErr
			}
			return dicomweb.ErrUnauthorized
		}
		if policy == nil {
			return nil
		}
		return policy(context.WithValue(ctx, verifiedTokenKey{}, token), token, request, operation)
	}
}

// BearerToken extracts the token of a single "Authorization: Bearer" header.
func BearerToken(request *http.Request) (string, bool) {
	if request == nil {
		return "", false
	}
	values := request.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	scheme, token, ok := strings.Cut(strings.TrimSpace(values[0]), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}
