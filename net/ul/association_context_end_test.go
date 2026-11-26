package ul

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// nilContextOperations are the association calls whose I/O runs under the
// association's own context because the caller passed none: Receive and Send
// with a nil context, and ReadPDU and WritePDU, which take no context.
var nilContextOperations = []struct {
	name string
	call func(*Association) error
}{
	{"Receive(nil)", func(a *Association) error {
		//nolint:staticcheck // A nil context selects the association's own context.
		_, err := a.Receive(nil)
		return err
	}},
	{"Send(nil)", func(a *Association) error {
		//nolint:staticcheck // A nil context selects the association's own context.
		return a.Send(nil, &ReleaseRQ{})
	}},
	{"ReadPDU", func(a *Association) error {
		_, err := a.ReadPDU()
		return err
	}},
	{"WritePDU", func(a *Association) error {
		return a.WritePDU(&ReleaseRQ{})
	}},
}

// associationContextEnds are the ways the association's own context ends and
// the error each must surface instead of the closed connection.
var associationContextEnds = []struct {
	name   string
	end    func() (context.Context, context.CancelFunc)
	target error
}{
	{
		name:   "canceled",
		end:    func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
		target: context.Canceled,
	},
	{
		name: "deadline",
		end: func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		},
		target: ErrAssociationTimeout,
	},
}

// Without a caller context the association's own context governs the I/O, so
// its end must take the precedence a caller context's end has (#2006): a
// server that cancels the association context and then closes the connection
// must not see net.ErrClosed from the call that reaches the association after
// the close, when an interrupted read or write of the same call reports the
// cancellation (#2010).
func TestNilContextCallsAfterAssociationContextEndThenCloseReportContextError(t *testing.T) {
	for _, end := range associationContextEnds {
		for _, op := range nilContextOperations {
			t.Run(end.name+"/"+op.name, func(t *testing.T) {
				local, remote := net.Pipe()
				cleanupClose(t, "remote pipe", remote)
				associationCtx, cancel := end.end()
				assoc := &Association{Conn: local, MaxPDU: DefaultMaxPDU, Context: associationCtx}

				cancel()
				if err := assoc.Close(); err != nil {
					t.Fatalf("Close() error = %v", err)
				}

				if err := op.call(assoc); !errors.Is(err, end.target) {
					t.Fatalf("%s error = %v, want %v", op.name, err, end.target)
				}
			})
		}
	}
}

// The close can also land after the call took the connection and before the
// I/O starts, failing the call that applies the association context's
// deadline to the connection; the ended association context still decides.
func TestNilContextCallsClosedWhileSettingAssociationDeadlineReportContextError(t *testing.T) {
	for _, op := range nilContextOperations {
		t.Run(op.name, func(t *testing.T) {
			local, remote := net.Pipe()
			cleanupClose(t, "remote pipe", remote)
			associationCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			assoc := &Association{MaxPDU: DefaultMaxPDU, Context: associationCtx}
			assoc.Conn = &closeOnDeadlineConn{Conn: local, before: func() {
				cancel()
				_ = assoc.Close()
			}}

			if err := op.call(assoc); !errors.Is(err, context.Canceled) {
				t.Fatalf("%s error = %v, want context.Canceled", op.name, err)
			}
		})
	}
}

// A cancel and close that interrupt a call already in its I/O report the
// cancellation too. The cancel and close are issued only once the call has
// taken the connection and entered its Read or Write, so this exercises the
// in-flight path, not the closed-before-the-call one above. This path already
// reported the cancellation before #2010; it is the behavior the closed-before
// path now matches, kept here so the two cannot drift apart again.
func TestNilContextCallsInterruptedByAssociationContextEndThenCloseReportContextError(t *testing.T) {
	for _, op := range nilContextOperations {
		t.Run(op.name, func(t *testing.T) {
			// Nobody reads or writes the other end, so the call blocks on the
			// pipe until the cancel and close interrupt it.
			local, remote := net.Pipe()
			cleanupClose(t, "remote pipe", remote)
			associationCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			conn := &ioEntryConn{Conn: local, entered: make(chan struct{})}
			assoc := &Association{Conn: conn, MaxPDU: DefaultMaxPDU, Context: associationCtx}

			result := make(chan error, 1)
			go func() { result <- op.call(assoc) }()
			select {
			case <-conn.entered:
			case err := <-result:
				t.Fatalf("%s returned %v before entering its I/O", op.name, err)
			case <-time.After(10 * time.Second):
				t.Fatalf("%s did not enter its I/O", op.name)
			}
			cancel()
			if err := assoc.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}

			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("%s error = %v, want context.Canceled", op.name, err)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("%s did not return after the cancel and close", op.name)
			}
		})
	}
}

// ioEntryConn closes entered when the first Read or Write starts, so a test
// can act while a call is provably inside its I/O on the connection. The
// association registers its context watcher before the I/O starts, so a
// cancel issued after entered interrupts that I/O.
type ioEntryConn struct {
	net.Conn
	entered chan struct{}
	once    sync.Once
}

func (c *ioEntryConn) Read(p []byte) (int, error) {
	c.once.Do(func() { close(c.entered) })
	return c.Conn.Read(p)
}

func (c *ioEntryConn) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.entered) })
	return c.Conn.Write(p)
}

// While the association's context is live a closed association is still
// reported as net.ErrClosed, and a caller context that is live decides over an
// association context that ended.
func TestClosedAssociationReportsErrClosedWhileTheGoverningContextIsLive(t *testing.T) {
	for _, op := range nilContextOperations {
		t.Run(op.name, func(t *testing.T) {
			local, remote := net.Pipe()
			cleanupClose(t, "remote pipe", remote)
			assoc := &Association{Conn: local, MaxPDU: DefaultMaxPDU, Context: context.Background()}
			if err := assoc.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
			if err := op.call(assoc); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("%s error = %v, want net.ErrClosed", op.name, err)
			}
		})
	}

	local, remote := net.Pipe()
	cleanupClose(t, "remote pipe", remote)
	associationCtx, cancel := context.WithCancel(context.Background())
	assoc := &Association{Conn: local, MaxPDU: DefaultMaxPDU, Context: associationCtx}
	cancel()
	if err := assoc.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := assoc.Receive(context.Background()); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Receive(live caller context) error = %v, want net.ErrClosed", err)
	}
	if err := assoc.Send(context.Background(), &ReleaseRQ{}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Send(live caller context) error = %v, want net.ErrClosed", err)
	}
}
