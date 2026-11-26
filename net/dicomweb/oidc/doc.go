// Package oidc is the optional OpenID Connect companion of net/dicomweb.
//
// The core dicomweb package only injects bearer tokens and caches them through
// dicomweb.TokenManager; it never speaks OAuth 2.0 itself. This package adds
// the protocol pieces a native application or an embedded server needs for a
// DICOMweb endpoint protected by an OpenID Provider:
//
//   - Discover fetches and validates OpenID Provider metadata
//     (/.well-known/openid-configuration) over HTTPS, requiring the literal
//     issuer and HTTPS endpoints.
//   - Client.BeginLogin runs the native-app authorization code flow with PKCE
//     (S256) on a 127.0.0.1 loopback redirect (RFC 8252, RFC 7636) or the
//     device authorization grant (RFC 8628), validating state, nonce and the
//     signed ID token.
//   - Client.Refresh, Client.Revoke and Client.TokenRefreshFunc exchange and
//     revoke refresh tokens; TokenRefreshFunc plugs directly into
//     dicomweb.NewTokenManager.
//   - Verifier checks compact JWS tokens (RS256/384/512, PS256/384/512,
//     ES256/384/512, EdDSA) against a JSON Web Key Set, with key rotation,
//     issuer, audience, expiry and not-before validation. "none" and HMAC
//     algorithms are always refused.
//   - Verifier.Authorizer turns verified bearer access tokens into a
//     dicomweb.Authorizer for Server.
//
// Every HTTP client used by the package refuses redirects to another origin
// and stops after five same-origin redirects, so tokens and codes are never
// forwarded to an unexpected host. Response bodies are bounded, provider error
// descriptions are discarded, and tokens never appear in String, GoString or
// JSON output.
//
// The package uses only the Go standard library. The core dicomweb package does
// not import it, so programs that do not need OpenID Connect do not link it.
// Persisting refresh tokens (for example in an OS keyring) remains the
// caller's responsibility through RefreshTokenStore.
package oidc
