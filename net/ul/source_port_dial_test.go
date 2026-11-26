package ul

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/sourceport/sourceporttest"
)

// TestSourcePortRetryDialContextGivesAnHTTPTransportTheRetry: the dial
// function UL exports for other TCP clients (#2011), set on a clone of
// http.DefaultTransport, connects again after a source port collision.
func TestSourcePortRetryDialContextGivesAnHTTPTransportTheRetry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = SourcePortRetryDialContext(nil)
	defer transport.CloseIdleConnections()
	ctx, collided := sourceporttest.Context(t, context.Background(), server.Listener.Addr().String())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		t.Fatalf("request after a source port collision: %v", err)
	}
	_ = response.Body.Close()
	if !collided() {
		t.Fatal("the request did not meet the source port collision")
	}
}

// A configured source port stays a single attempt.
func TestSourcePortRetryDialContextKeepsAConfiguredSourcePort(t *testing.T) {
	address, _ := sourceporttest.Server(t)
	local := sourceporttest.Collision(t, address)
	_, err := SourcePortRetryDialContext(&net.Dialer{LocalAddr: local})(context.Background(), "tcp4", address)
	if !LocalEndpointUnavailable(err) {
		t.Fatalf("dial from a configured source port = %v, want an unavailable local endpoint", err)
	}
}
