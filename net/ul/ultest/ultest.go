// Package ultest supports tests of clients that use the outgoing dial policy
// of net/ul. It is not for production code.
package ultest

import (
	"context"
	"net"

	"github.com/ThalesMMS/dicom-go/internal/sourceport"
)

// WithSourcePortCollision returns a context under which the first connect
// attempt of a ul.SourcePortRetryDialContext dial that leaves the source
// endpoint to the system comes from local instead, as if the system had
// chosen it; later attempts are unaffected. With local an endpoint whose
// connection to the same server is still in TIME_WAIT, this reproduces the
// source port collision of ephemeral port pressure through a client's real
// dial path. The returned function reports whether a dial met the collision,
// which a client whose dial lacks the retry never does.
func WithSourcePortCollision(ctx context.Context, local *net.TCPAddr) (context.Context, func() bool) {
	first := &sourceport.FirstAttempt{Local: local}
	return sourceport.WithFirstAttempt(ctx, first), first.Collided
}
