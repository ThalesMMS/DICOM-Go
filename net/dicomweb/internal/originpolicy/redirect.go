// Package originpolicy contains the redirect guard shared by DICOMweb and its
// OIDC provider client.
package originpolicy

import (
	"net/http"
	"net/url"
	"strings"
)

// RestrictRedirects copies client and binds redirects to the original request
// origin. The callback still runs for eligible requests, and its outgoing
// request is validated again before any credentials or body can be sent.
func RestrictRedirects(client *http.Client, maxRequests int, originError, limitError error) *http.Client {
	guarded := *client
	previous := client.CheckRedirect
	guarded.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRequests {
			return limitError
		}
		if len(via) == 0 || via[0] == nil || !requestWithinOrigin(via[0].URL, req) {
			return originError
		}
		if previous != nil {
			// Snapshot the origin before handing the request history to the
			// callback. A change to the first request must not rebase this
			// decision or the origin used for subsequent redirects.
			origin := *via[0].URL
			if err := previous(req, via); err != nil {
				return err
			}
			if !requestWithinOrigin(&origin, req) || !requestWithinOrigin(&origin, via[0]) {
				return originError
			}
		}
		return nil
	}
	return &guarded
}

func requestWithinOrigin(origin *url.URL, req *http.Request) bool {
	if req == nil || !SameOrigin(origin, req.URL) || req.URL.User != nil || req.URL.Opaque != "" {
		return false
	}
	if req.Host == "" || strings.EqualFold(req.Host, req.URL.Host) {
		return true
	}
	// A relative redirect or a callback can supply an overridden HTTP Host.
	// Refuse another virtual host, even when the URL still identifies the
	// original socket address.
	authority, err := url.Parse(req.URL.Scheme + "://" + req.Host)
	return err == nil && authority.Host == req.Host && SameOrigin(origin, authority)
}

// SameOrigin compares scheme, hostname and effective port.
func SameOrigin(left, right *url.URL) bool {
	return left != nil && right != nil &&
		strings.EqualFold(left.Scheme, right.Scheme) &&
		strings.EqualFold(left.Hostname(), right.Hostname()) &&
		effectivePort(left) == effectivePort(right)
}

func effectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	default:
		return ""
	}
}
