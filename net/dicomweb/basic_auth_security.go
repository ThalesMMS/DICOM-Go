package dicomweb

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

var (
	// ErrInsecureBasicAuth reports that Basic credentials would be used without
	// verified, origin-bound HTTPS. The error never includes credentials.
	ErrInsecureBasicAuth = errors.New("DICOMweb Basic authentication requires verified HTTPS")
)

func (c Client) usesBasicAuth() bool {
	return c.Options.BearerTokenSource == nil &&
		strings.TrimSpace(c.Options.BearerToken) == "" &&
		strings.TrimSpace(c.Options.BasicUsername) != ""
}

func validateBasicAuthTarget(target *url.URL) error {
	if target == nil || !strings.EqualFold(target.Scheme, "https") || strings.TrimSpace(target.Host) == "" {
		return fmt.Errorf("%w: endpoint must use HTTPS", ErrInsecureBasicAuth)
	}
	if target.User != nil {
		return fmt.Errorf("%w: URL user information is not allowed", ErrInsecureBasicAuth)
	}
	return nil
}

// secureBasicAuthTransport validates the caller's transport for Basic
// credentials and returns it unchanged. It is checked on every request but not
// cloned: a clone starts with an empty connection pool, so cloning per request
// would defeat keep-alive and TLS session reuse (#1242). As net/http requires,
// callers must not modify a transport (or its TLS configuration) after first
// use.
func secureBasicAuthTransport(roundTripper http.RoundTripper) (*http.Transport, error) {
	if roundTripper == nil {
		roundTripper = http.DefaultTransport
	}
	transport, ok := roundTripper.(*http.Transport)
	if !ok || transport == nil {
		return nil, fmt.Errorf("%w: Basic authentication requires a verifiable HTTP transport", ErrInsecureBasicAuth)
	}
	if transport.DialTLS != nil || transport.DialTLSContext != nil {
		return nil, fmt.Errorf("%w: custom TLS dialers are not allowed", ErrInsecureBasicAuth)
	}
	if transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify {
		return nil, fmt.Errorf("%w: TLS certificate verification is disabled", ErrInsecureBasicAuth)
	}
	return transport, nil
}
