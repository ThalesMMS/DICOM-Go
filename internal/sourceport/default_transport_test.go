package sourceport

import (
	"net/http"
	"testing"
	"time"
)

type replacedRoundTripper struct{}

func (replacedRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, http.ErrNotSupported
}

// A program that replaced http.DefaultTransport with another RoundTripper
// still gets the timeouts and pool bounds net/http gives its own default
// transport, not a zero-valued one without a TLS handshake timeout.
func TestDefaultTransportWithoutAnHTTPTransportKeepsNetHTTPDefaults(t *testing.T) {
	transport := retryingClone(replacedRoundTripper{})
	if transport.DialContext == nil || transport.Proxy == nil || !transport.ForceAttemptHTTP2 ||
		transport.MaxIdleConns != 100 || transport.IdleConnTimeout != 90*time.Second ||
		transport.TLSHandshakeTimeout != 10*time.Second || transport.ExpectContinueTimeout != time.Second {
		t.Fatalf("fallback transport: dial %t, proxy %t, HTTP/2 %t, max idle %d, idle %s, TLS handshake %s, expect-continue %s; want the net/http defaults with a retrying dial",
			transport.DialContext != nil, transport.Proxy != nil, transport.ForceAttemptHTTP2, transport.MaxIdleConns,
			transport.IdleConnTimeout, transport.TLSHandshakeTimeout, transport.ExpectContinueTimeout)
	}
	clone := retryingClone(http.DefaultTransport)
	base := http.DefaultTransport.(*http.Transport)
	if clone == base || clone.TLSHandshakeTimeout != base.TLSHandshakeTimeout || clone.MaxIdleConns != base.MaxIdleConns {
		t.Fatal("the clone of http.DefaultTransport lost its settings or is not a clone")
	}
}
