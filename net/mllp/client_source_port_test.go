package mllp

import (
	"context"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/sourceport/sourceporttest"
)

// TestDialRetriesASourcePortCollision: Dial connects again when the source
// port the system chose collides with a TIME_WAIT connection to the same
// receiver (#2011).
func TestDialRetriesASourcePortCollision(t *testing.T) {
	address, accepted := sourceporttest.Server(t)
	ctx, collided := sourceporttest.Context(t, context.Background(), address)
	client, err := Dial(ctx, address, ClientOptions{})
	if err != nil {
		t.Fatalf("dial after a source port collision: %v", err)
	}
	_ = client.Close()
	if !collided() || !accepted(2) {
		t.Fatalf("collided = %t, connected = %t; want the dial to meet the collision and connect again", collided(), accepted(2))
	}
}
