package dicomweb

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/ThalesMMS/dicom-go/internal/sourceport"
	"github.com/ThalesMMS/dicom-go/net/dicomweb/internal/originpolicy"
)

// RedirectPolicy configures the origin guard for DICOMweb HTTP requests.
// Basic auth and BulkData retrieval always enforce the original origin.
type RedirectPolicy uint8

const (
	// RedirectPolicyDefault retains the HTTP client's redirect policy for
	// requests without a mandatory origin guard.
	RedirectPolicyDefault RedirectPolicy = iota
	// RedirectPolicyAuthenticated also guards requests with a bearer token
	// or token source.
	RedirectPolicyAuthenticated
	// RedirectPolicySameOrigin guards all requests, including anonymous ones.
	RedirectPolicySameOrigin
)

var (
	// ErrRedirectOrigin reports a redirect outside the original HTTP(S) origin
	// or with an unsafe destination authority.
	ErrRedirectOrigin      = errors.New("DICOMweb redirect must use the original origin")
	errRedirectLimit       = errors.New("DICOMweb redirect limit exceeded")
	errBasicRedirectOrigin = fmt.Errorf("%w: %w", ErrInsecureBasicAuth, ErrRedirectOrigin)
	errBasicRedirectLimit  = fmt.Errorf("%w: %w", ErrInsecureBasicAuth, errRedirectLimit)
)

const maxRedirectRequests = 10

func (c Client) httpClientForRequest(target *url.URL, sameOriginRedirect bool) (*http.Client, error) {
	httpClient := c.Options.HTTPClient
	if httpClient == nil {
		httpClient = sourceport.HTTPClient()
	}

	basicAuth := c.usesBasicAuth()
	originBound, err := c.redirectsRequireSameOrigin(basicAuth)
	if err != nil {
		return nil, err
	}
	if basicAuth {
		if err := validateBasicAuthTarget(target); err != nil {
			return nil, err
		}
		transport, err := secureBasicAuthTransport(httpClient.Transport)
		if err != nil {
			return nil, err
		}

		// Reuse the verified transport and its connection pool. Only the
		// returned client copy receives a new transport or redirect callback.
		secured := originpolicy.RestrictRedirects(httpClient, maxRedirectRequests, errBasicRedirectOrigin, errBasicRedirectLimit)
		secured.Transport = transport
		return secured, nil
	}
	if sameOriginRedirect || originBound {
		return originpolicy.RestrictRedirects(httpClient, maxRedirectRequests, ErrRedirectOrigin, errRedirectLimit), nil
	}
	return httpClient, nil
}

func (c Client) redirectsRequireSameOrigin(basicAuth bool) (bool, error) {
	switch c.Options.RedirectPolicy {
	case RedirectPolicyDefault:
		return basicAuth, nil
	case RedirectPolicyAuthenticated:
		return basicAuth || c.Options.BearerTokenSource != nil ||
			strings.TrimSpace(c.Options.BearerToken) != "", nil
	case RedirectPolicySameOrigin:
		return true, nil
	default:
		return false, errors.New("invalid DICOMweb redirect policy")
	}
}
