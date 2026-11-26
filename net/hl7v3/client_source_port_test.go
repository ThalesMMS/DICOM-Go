package hl7v3_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/sourceport/sourceporttest"
	"github.com/ThalesMMS/dicom-go/net/hl7v3"
)

// TestSubmitRetriesASourcePortCollision: the client connects again when the
// source port the system chose collides with a TIME_WAIT connection to the
// receiver; the connect failed before any byte was sent, so it is no replay
// (#2011).
func TestSubmitRetriesASourcePortCollision(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	client := newClient(t, server.URL+"/receive", hl7v3.Options{AllowLoopbackHTTP: true})
	ctx, collided := sourceporttest.Context(t, context.Background(), server.Listener.Addr().String())
	_, err := client.Submit(ctx, []byte(syntheticCDA))
	if !collided() || requests.Load() != 1 {
		t.Fatalf("collided = %t, requests = %d (%v); want the dial to meet the collision and the document sent once", collided(), requests.Load(), err)
	}
}
