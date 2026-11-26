package ldapconfig

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"
)

// Socket deadlines are enforced independently from the context's timer.
// Model the instant after the absolute deadline when Err is not published yet.
type pendingDeadlineContext struct {
	context.Context
	deadline time.Time
}

func (c pendingDeadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }

func TestLDAPDeadlineWinsBeforeContextTimerPublication(t *testing.T) {
	deadline := time.Now().Add(-time.Second)
	ctx := pendingDeadlineContext{Context: context.Background(), deadline: deadline}
	session := &ldapSession{ctx: ctx, guard: &boundedConn{}}
	socketTimeout := &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
	if ctx.Err() != nil {
		t.Fatal("fixture must model the unpublished context timer")
	}
	if err := session.normalizeError(socketTimeout); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired absolute deadline classified as %v", err)
	}
}

func TestLDAPUnexpiredTransportAndExplicitCancellation(t *testing.T) {
	future := pendingDeadlineContext{Context: context.Background(), deadline: time.Now().Add(time.Hour)}
	socketTimeout := &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
	for _, ctx := range []context.Context{future, context.Background()} {
		session := &ldapSession{ctx: ctx, guard: &boundedConn{}}
		if err := session.normalizeError(socketTimeout); !errors.Is(err, ErrTransport) {
			t.Fatalf("transport error before transaction deadline classified as %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	session := &ldapSession{ctx: ctx, guard: &boundedConn{}}
	if err := session.normalizeError(socketTimeout); !errors.Is(err, context.Canceled) {
		t.Fatalf("explicit cancellation classified as %v", err)
	}
}
