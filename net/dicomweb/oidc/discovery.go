package oidc

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// WellKnownPath is the OpenID Connect Discovery 1.0 metadata suffix.
const WellKnownPath = "/.well-known/openid-configuration"

// Flow selects the native-app login grant.
type Flow string

const (
	// FlowAuthorizationCode is the authorization code grant with PKCE S256
	// and a 127.0.0.1 loopback redirect.
	FlowAuthorizationCode Flow = "authorization_code"
	// FlowDeviceCode is the OAuth 2.0 device authorization grant.
	FlowDeviceCode Flow = "device_code"
)

// Metadata is the subset of OpenID Provider metadata used by this package.
type Metadata struct {
	Issuer                      string   `json:"issuer"`
	AuthorizationEndpoint       string   `json:"authorization_endpoint,omitempty"`
	TokenEndpoint               string   `json:"token_endpoint"`
	DeviceAuthorizationEndpoint string   `json:"device_authorization_endpoint,omitempty"`
	RevocationEndpoint          string   `json:"revocation_endpoint,omitempty"`
	UserInfoEndpoint            string   `json:"userinfo_endpoint,omitempty"`
	JWKSURI                     string   `json:"jwks_uri"`
	CodeChallengeMethods        []string `json:"code_challenge_methods_supported,omitempty"`
	IDTokenSigningAlgs          []string `json:"id_token_signing_alg_values_supported,omitempty"`
}

// NormalizeIssuer validates an HTTPS issuer identifier without query or
// fragment and removes one trailing slash.
func NormalizeIssuer(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" {
		return "", ErrNotConfigured
	}
	return strings.TrimSuffix(raw, "/"), nil
}

// Discover fetches and validates the provider metadata of issuer. The client
// is wrapped by SecureHTTPClient. flow selects which login endpoints are
// mandatory; pass "" when only verification or refresh is needed.
func Discover(ctx context.Context, client *http.Client, issuer string, flow Flow) (Metadata, error) {
	issuer, err := NormalizeIssuer(issuer)
	if err != nil {
		return Metadata{}, newError(ErrorDiscovery, "discovery", 0, "", nil)
	}
	return discover(ctx, SecureHTTPClient(client), issuer, flow)
}

func discover(ctx context.Context, client *http.Client, issuer string, flow Flow) (Metadata, error) {
	var metadata Metadata
	status, err := getJSON(ctx, client, issuer+WellKnownPath, "", &metadata)
	if err != nil {
		return Metadata{}, contextError(ErrorDiscovery, "discovery", err)
	}
	if status < 200 || status >= 300 {
		return Metadata{}, newError(ErrorDiscovery, "discovery", status, "", nil)
	}
	if err := ValidateMetadata(issuer, flow, metadata); err != nil {
		return Metadata{}, err
	}
	return metadata, nil
}

// ValidateMetadata checks that the issuer matches literally, that the
// required endpoints for flow are present, that every endpoint uses HTTPS,
// that the authorization code flow advertises PKCE S256, and that any
// advertised ID token algorithm list contains a supported asymmetric one.
func ValidateMetadata(issuer string, flow Flow, metadata Metadata) error {
	if metadata.Issuer != issuer {
		return newError(ErrorDiscovery, "validate issuer", 0, "", nil)
	}
	required := []string{metadata.TokenEndpoint, metadata.JWKSURI}
	switch flow {
	case FlowDeviceCode:
		required = append(required, metadata.DeviceAuthorizationEndpoint)
	case FlowAuthorizationCode:
		required = append(required, metadata.AuthorizationEndpoint)
		if !containsFold(metadata.CodeChallengeMethods, "S256") {
			return newError(ErrorDiscovery, "validate PKCE", 0, "", nil)
		}
	case "":
	default:
		return newError(ErrorConfiguration, "validate flow", 0, "", nil)
	}
	optional := []string{
		metadata.AuthorizationEndpoint, metadata.DeviceAuthorizationEndpoint,
		metadata.RevocationEndpoint, metadata.UserInfoEndpoint,
	}
	for _, endpoint := range required {
		if ValidateSecureEndpoint(endpoint) != nil {
			return newError(ErrorDiscovery, "validate endpoint", 0, "", nil)
		}
	}
	for _, endpoint := range optional {
		if strings.TrimSpace(endpoint) != "" && ValidateSecureEndpoint(endpoint) != nil {
			return newError(ErrorDiscovery, "validate endpoint", 0, "", nil)
		}
	}
	if len(metadata.IDTokenSigningAlgs) > 0 && len(SupportedAlgorithms(metadata.IDTokenSigningAlgs)) == 0 {
		return newError(ErrorDiscovery, "validate ID token signing algorithms", 0, "", nil)
	}
	return nil
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), want) {
			return true
		}
	}
	return false
}
