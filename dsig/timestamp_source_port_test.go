package dsig

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/sourceport/sourceporttest"
)

// TestHTTPTimestamperRetriesASourcePortCollision: without a client of its own
// the timestamper connects again when the source port the system chose
// collides with a TIME_WAIT connection to the authority (#2011).
func TestHTTPTimestamperRetriesASourcePortCollision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/timestamp-reply")
		_, _ = w.Write([]byte{0x30, 0x00})
	}))
	defer server.Close()
	ctx, collided := sourceporttest.Context(t, context.Background(), server.Listener.Addr().String())
	if _, err := (HTTPTimestamper{URL: server.URL}).Timestamp(ctx, []byte{0x30, 0x00}); err != nil {
		t.Fatalf("timestamp after a source port collision: %v", err)
	}
	if !collided() {
		t.Fatal("the request did not meet the source port collision: its dial does not retry a source port")
	}
}
