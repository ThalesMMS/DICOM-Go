package jpip

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/sourceport/sourceporttest"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// TestDefaultClientRetriesASourcePortCollision: without an HTTPClient of its
// own the JPIP client connects again when the source port the system chose
// collides with a TIME_WAIT connection to the server (#2011).
func TestDefaultClientRetriesASourcePortCollision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jp2")
		_, _ = w.Write(jp2Fixture)
	}))
	defer server.Close()
	rule, err := RuleFromURL(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(Config{Policy: Policy{Rules: []Rule{rule}}, MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, collided := sourceporttest.Context(t, context.Background(), server.Listener.Addr().String())
	if _, err := client.Retrieve(ctx, Request{
		Reference: pixeldata.JPIPReference{PixelDataProviderURL: server.URL + "/jpip?target=fixture"},
	}); err != nil {
		t.Fatalf("retrieve after a source port collision: %v", err)
	}
	if !collided() {
		t.Fatal("the request did not meet the source port collision: its dial does not retry a source port")
	}
}
