package ul

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// A peer that accepts the connection, never answers the association request
// and never closes must not hold the caller past its deadline: the ARTIM
// wait after the abort happens without the caller.
func TestDialReturnsAtTheCallerDeadlineWithoutTheARTIMWait(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	held := make(chan net.Conn, 1)
	go func() {
		if conn, err := listener.Accept(); err == nil {
			held <- conn
		}
	}()
	defer func() {
		select {
		case conn := <-held:
			_ = conn.Close()
		case <-time.After(time.Second):
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	assoc, err := DialContext(ctx, listener.Addr().String(), DialOptions{
		CalledAETitle:  "SILENT_SCP",
		CallingAETitle: "WAITING_SCU",
		Contexts:       []PresentationContext{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}}},
	})
	returned := time.Now()
	if err == nil {
		_ = assoc.Close()
		t.Fatal("the association with a silent peer was established")
	}
	assertReturnedAtTheDeadline(t, ctx, returned)
}

// assertReturnedAtTheDeadline checks that a dial returned at the deadline of
// ctx, not before it and not with the ARTIM wait on top. The deadline itself
// is the reference: a start time taken after the context was created is later
// than the instant the deadline counts from, so the time elapsed since it can
// read below the timeout although the deadline had passed.
func assertReturnedAtTheDeadline(t *testing.T, ctx context.Context, returned time.Time) {
	t.Helper()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("the context has no deadline")
	}
	if late := returned.Sub(deadline); late < 0 || late > DefaultARTIMTimeout/2 {
		t.Fatalf("the dial returned %s after the caller's deadline, want it at the deadline and not the %s ARTIM wait on top", late, DefaultARTIMTimeout)
	}
}

// lateDeadlineContext has a deadline that has passed before its Done channel
// closes, as a context.WithTimeout does between its deadline and the run of
// its timer. A socket deadline taken from Deadline can expire inside that
// window, and a loaded scheduler widens it.
type lateDeadlineContext struct {
	context.Context
	deadline time.Time
	done     chan struct{}
	once     sync.Once
}

func newLateDeadlineContext(after time.Duration) *lateDeadlineContext {
	return &lateDeadlineContext{Context: context.Background(), deadline: time.Now().Add(after), done: make(chan struct{})}
}

func (c *lateDeadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }
func (c *lateDeadlineContext) Done() <-chan struct{}       { return c.done }
func (c *lateDeadlineContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// expire closes Done, as the late timer eventually does.
func (c *lateDeadlineContext) expire() { c.once.Do(func() { close(c.done) }) }

// silentPeer accepts one association request, never answers it and never
// closes; it reports the PDU, or the error, that follows the request.
func silentPeer(t *testing.T) (string, <-chan any) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	next := make(chan any, 1)
	held := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			next <- err
			return
		}
		held <- conn
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := ReadPDU(conn, DefaultMaxPDU); err != nil {
			next <- err
			return
		}
		pdu, err := ReadPDU(conn, DefaultMaxPDU)
		if err != nil {
			next <- err
			return
		}
		next <- pdu
	}()
	t.Cleanup(func() {
		select {
		case conn := <-held:
			_ = conn.Close()
		case <-time.After(time.Second):
		}
	})
	return listener.Addr().String(), next
}

// The socket deadline taken from the caller's context can expire before the
// context reports it. The dial still returns at the caller's deadline instead
// of running the ARTIM wait on top (#1995).
func TestDialReturnsAtTheCallerDeadlineBeforeTheContextReportsIt(t *testing.T) {
	address, _ := silentPeer(t)
	ctx := newLateDeadlineContext(300 * time.Millisecond)
	defer ctx.expire()
	assoc, err := DialContext(ctx, address, DialOptions{
		CalledAETitle:  "SILENT_SCP",
		CallingAETitle: "WAITING_SCU",
		Contexts:       []PresentationContext{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}}},
	})
	returned := time.Now()
	if err == nil {
		_ = assoc.Close()
		t.Fatal("the association with a silent peer was established")
	}
	if !errors.Is(err, ErrAssociationTimeout) {
		t.Fatalf("dial error %v, want ErrAssociationTimeout", err)
	}
	if ctx.Err() != nil {
		t.Fatal("the context reported its deadline during the dial; the test no longer covers the late report")
	}
	assertReturnedAtTheDeadline(t, ctx, returned)
}

// A dial ended by the caller's deadline still sends the A-ABORT, under a
// deadline of its own, instead of only closing the connection.
func TestDialSendsTheAbortAfterTheCallerDeadline(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  func() (context.Context, func())
	}{
		{"context timer ran", func() (context.Context, func()) {
			return context.WithTimeout(context.Background(), 200*time.Millisecond)
		}},
		{"context timer late", func() (context.Context, func()) {
			ctx := newLateDeadlineContext(200 * time.Millisecond)
			return ctx, ctx.expire
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			address, next := silentPeer(t)
			ctx, cancel := tc.ctx()
			defer cancel()
			assoc, err := DialContext(ctx, address, DialOptions{
				CalledAETitle:  "SILENT_SCP",
				CallingAETitle: "WAITING_SCU",
				Contexts:       []PresentationContext{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}}},
			})
			if err == nil {
				_ = assoc.Close()
				t.Fatal("the association with a silent peer was established")
			}
			select {
			case got := <-next:
				abort, ok := got.(*AbortRQ)
				if !ok || abort.Source != AbortSourceServiceUser || abort.Reason != AbortReasonNotSpecified {
					t.Fatalf("the peer received %#v after the request, want a service-user A-ABORT", got)
				}
			case <-time.After(DefaultARTIMTimeout + time.Second):
				t.Fatal("the peer received nothing after the request")
			}
		})
	}
}
