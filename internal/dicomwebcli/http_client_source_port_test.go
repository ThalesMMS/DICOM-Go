package dicomwebcli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/sourceport/sourceporttest"
)

// TestTLSFlagClientRetriesASourcePortCollision: the client built for TLS
// flags keeps the source port retry of the default client (#2011).
func TestTLSFlagClientRetriesASourcePortCollision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, err := (commonOptions{tlsServerName: "pacs.example"}).httpClient()
	if err != nil || client == nil {
		t.Fatalf("client = %v, %v", client, err)
	}
	ctx, collided := sourceporttest.Context(t, context.Background(), server.Listener.Addr().String())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("request after a source port collision: %v", err)
	}
	_ = response.Body.Close()
	if !collided() {
		t.Fatal("the request did not meet the source port collision: its dial does not retry a source port")
	}
}
