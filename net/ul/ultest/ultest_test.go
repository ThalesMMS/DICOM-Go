package ultest_test

import (
	"context"
	"net"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/sourceport/sourceporttest"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/net/ul/ultest"
)

// The collision reaches the dial of ul.SourcePortRetryDialContext, which
// connects again, and never a plain dialer.
func TestWithSourcePortCollisionReachesTheRetryingDial(t *testing.T) {
	address, _ := sourceporttest.Server(t)
	ctx, collided := ultest.WithSourcePortCollision(context.Background(), sourceporttest.Collision(t, address))
	conn, err := ul.SourcePortRetryDialContext(nil)(ctx, "tcp4", address)
	if err != nil {
		t.Fatalf("dial after a source port collision: %v", err)
	}
	_ = conn.Close()
	if !collided() {
		t.Fatal("the retrying dial did not meet the source port collision")
	}

	ctx, collided = ultest.WithSourcePortCollision(context.Background(), sourceporttest.Collision(t, address))
	conn, err = (&net.Dialer{}).DialContext(ctx, "tcp4", address)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if collided() {
		t.Fatal("a plain dial reported the collision")
	}
}
