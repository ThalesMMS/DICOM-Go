package oidc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/sourceport/sourceporttest"
)

// TestDefaultHTTPClientRetriesASourcePortCollision: the default client of
// discovery, token and key set requests connects again when the source port
// the system chose collides with a TIME_WAIT connection to the provider (#2011).
func TestDefaultHTTPClientRetriesASourcePortCollision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	ctx, collided := sourceporttest.Context(t, context.Background(), server.Listener.Addr().String())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := SecureHTTPClient(nil).Do(request)
	if err != nil {
		t.Fatalf("request after a source port collision: %v", err)
	}
	_ = response.Body.Close()
	if !collided() {
		t.Fatal("the request did not meet the source port collision: its dial does not retry a source port")
	}
}
