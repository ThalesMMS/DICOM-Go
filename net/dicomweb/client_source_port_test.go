package dicomweb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/sourceport/sourceporttest"
)

// TestClientWithoutHTTPClientRetriesASourcePortCollision reproduces #2011:
// under ephemeral port pressure the system hands a QIDO-RS or WADO-RS
// connection a source port whose TIME_WAIT connection went to the same
// server, and the connect fails before the server is reached. A client
// without an HTTPClient of its own must connect again from a fresh port.
func TestClientWithoutHTTPClientRetriesASourcePortCollision(t *testing.T) {
	var reached atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Store(true)
		w.Header().Set("Content-Type", "application/dicom+json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer server.Close()
	ctx, collided := sourceporttest.Context(t, context.Background(), server.Listener.Addr().String())
	client := Client{Endpoint: Endpoint{BaseURL: server.URL}}
	if _, err := client.SearchStudies(ctx, nil); err != nil {
		t.Fatalf("search after a source port collision: %v", err)
	}
	if !collided() || !reached.Load() {
		t.Fatalf("collided = %t, reached the server = %t; want the dial to meet the collision and connect again", collided(), reached.Load())
	}
}

// The UPS-RS event channel dials its own WebSocket connection; without a
// transport dial of its own it takes the same retry.
func TestEventChannelDialRetriesASourcePortCollision(t *testing.T) {
	address, accepted := sourceporttest.Server(t)
	ctx, collided := sourceporttest.Context(t, context.Background(), address)
	target, err := url.Parse("http://" + address + "/ws")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dialEventChannel(ctx, &http.Transport{}, target)
	if err != nil {
		t.Fatalf("event channel dial after a source port collision: %v", err)
	}
	_ = conn.Close()
	if !collided() || !accepted(2) {
		t.Fatalf("collided = %t, connected = %t; want the dial to meet the collision and connect again", collided(), accepted(2))
	}
}
