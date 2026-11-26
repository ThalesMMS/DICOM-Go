package fhir

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/sourceport/sourceporttest"
)

// TestSearchRetriesASourcePortCollision: the client connects again when the
// source port the system chose collides with a TIME_WAIT connection to the
// FHIR server, before the request is sent (#2011).
func TestSearchRetriesASourcePortCollision(t *testing.T) {
	var reached atomic.Bool
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Store(true)
		http.Error(w, "synthetic", http.StatusServiceUnavailable)
	}))
	defer peer.Close()
	client, err := NewClient(peer.URL+testPath, ClientOptions{AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, collided := sourceporttest.Context(t, context.Background(), peer.Listener.Addr().String())
	_, err = client.SearchImagingStudies(ctx, ImagingSearch{Parameters: url.Values{"patient": {"Patient/synthetic"}}})
	if !collided() || !reached.Load() {
		t.Fatalf("collided = %t, reached the server = %t (%v); want the dial to meet the collision and connect again", collided(), reached.Load(), err)
	}
}
