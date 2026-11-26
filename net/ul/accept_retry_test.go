package ul

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// flakyListener fails the first failures calls to Accept with err and then
// accepts from the wrapped listener.
type flakyListener struct {
	net.Listener
	failures atomic.Int32
	err      error
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if l.failures.Add(-1) >= 0 {
		return nil, l.err
	}
	return l.Listener.Accept()
}

type temporaryTestError struct{}

func (temporaryTestError) Error() string   { return "temporary accept failure" }
func (temporaryTestError) Temporary() bool { return true }
func (temporaryTestError) Timeout() bool   { return false }

// descriptorExhaustionError is the error an accept returns when the process
// has no descriptor left: EMFILE, or WSAEMFILE on Windows.
func descriptorExhaustionError(t *testing.T) error {
	t.Helper()
	if descriptorExhaustionErrno == 0 {
		t.Skip("no descriptor exhaustion errno on this platform")
	}
	return &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept", descriptorExhaustionErrno)}
}

func TestRetryableAcceptError(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"closed listener", net.ErrClosed, false},
		{"closed listener in OpError", &net.OpError{Op: "accept", Net: "tcp", Err: net.ErrClosed}, false},
		{"permanent", errors.New("permanent accept failure"), false},
		{"canceled", context.Canceled, false},
		{"temporary", fmt.Errorf("wrapped: %w", temporaryTestError{}), true},
		{"timeout", &net.OpError{Op: "accept", Net: "tcp", Err: os.ErrDeadlineExceeded}, true},
		{"socket buffers", &connectionSetupError{err: errors.New("set receive buffer")}, true},
	} {
		if got := retryableAcceptError(test.err); got != test.want {
			t.Errorf("%s: retryableAcceptError(%v) = %v, want %v", test.name, test.err, got, test.want)
		}
	}
	if err := descriptorExhaustionError(t); !retryableAcceptError(err) {
		t.Errorf("retryableAcceptError(%v) = false, want true", err)
	}
	if descriptorBuffersErrno != 0 {
		err := &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept", descriptorBuffersErrno)}
		if !retryableAcceptError(err) {
			t.Errorf("retryableAcceptError(%v) = false, want true", err)
		}
	}
}

func TestNextAcceptRetryDelayDoublesUpToOneSecond(t *testing.T) {
	want := []time.Duration{
		5 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond,
		80 * time.Millisecond, 160 * time.Millisecond, 320 * time.Millisecond, 640 * time.Millisecond,
		time.Second, time.Second,
	}
	var delay time.Duration
	for i, expected := range want {
		delay = nextAcceptRetryDelay(delay)
		if delay != expected {
			t.Fatalf("delay %d = %v, want %v", i, delay, expected)
		}
	}
}

func TestListenerAcceptContextRetriesTemporaryErrorThenAccepts(t *testing.T) {
	listener, err := Listen(ListenOptions{Address: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer func() { _ = listener.Close() }()
	flaky := &flakyListener{Listener: listener.listener, err: temporaryTestError{}}
	flaky.failures.Store(1)
	listener.listener = flaky

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial listener: %v", err)
	}
	defer func() { _ = client.Close() }()
	var reported []error
	conn, err := listener.AcceptContext(ctx, func(err error) { reported = append(reported, err) })
	if err != nil {
		t.Fatalf("AcceptContext() error = %v", err)
	}
	defer func() { _ = conn.Close() }()
	if len(reported) != 1 || !errors.As(reported[0], new(temporaryTestError)) {
		t.Fatalf("reported accept errors = %v, want the one temporary failure", reported)
	}
	if conn.RemoteAddr().String() != client.LocalAddr().String() {
		t.Fatalf("accepted %v, want the client at %v", conn.RemoteAddr(), client.LocalAddr())
	}
}

func TestListenerAcceptContextStopsRetryingWhenContextEnds(t *testing.T) {
	listener, err := Listen(ListenOptions{Address: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer func() { _ = listener.Close() }()
	flaky := &flakyListener{Listener: listener.listener, err: temporaryTestError{}}
	flaky.failures.Store(1 << 30)
	listener.listener = flaky

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failures := 0
	_, err = listener.AcceptContext(ctx, func(error) {
		// The context ends during the pause after the second failure.
		if failures++; failures == 2 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("AcceptContext() error = %v, want context.Canceled", err)
	}
	if failures != 2 {
		t.Fatalf("reported failures = %d, want 2", failures)
	}
}

func TestListenerAcceptContextReturnsClosedListener(t *testing.T) {
	listener, err := Listen(ListenOptions{Address: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	_, err = listener.AcceptContext(context.Background(), func(err error) {
		t.Errorf("closed listener reported as retryable: %v", err)
	})
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("AcceptContext() error = %v, want net.ErrClosed", err)
	}
}

func TestAssociationServerRetriesTemporaryAcceptErrorThenAccepts(t *testing.T) {
	listener, err := Listen(ListenOptions{Address: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	// Two descriptor exhaustion failures before the real accept exercise the
	// pause and its doubling.
	flaky := &flakyListener{Listener: listener.listener, err: descriptorExhaustionError(t)}
	flaky.failures.Store(2)
	listener.listener = flaky
	handled := make(chan struct{})
	server, err := NewAssociationServer(listener, AssociationServerOptions{
		Accept: AcceptOptions{
			AETitle:                   "RETRY_SCP",
			SupportedAbstractSyntaxes: []string{verificationSOPClassUID},
			SupportedTransferSyntaxes: []string{ImplicitVRLittleEndian},
		},
		Handler: func(context.Context, *Association) error {
			close(handled)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewAssociationServer() error = %v", err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(context.Background()) }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	assoc, err := DialContext(ctx, listener.Addr().String(), DialOptions{
		CalledAETitle: "RETRY_SCP",
		Contexts: []PresentationContext{{
			AbstractSyntaxUID:  verificationSOPClassUID,
			TransferSyntaxUIDs: []string{ImplicitVRLittleEndian},
		}},
	})
	if err != nil {
		select {
		case serveErr := <-serveDone:
			t.Fatalf("Dial() error = %v; Serve() returned %v after a temporary accept error", err, serveErr)
		default:
			t.Fatalf("Dial() error = %v", err)
		}
	}
	defer func() { _ = assoc.Close() }()
	select {
	case <-handled:
	case <-ctx.Done():
		t.Fatal("handler did not run after the accept failures")
	}
	if got := server.Snapshot().AcceptErrors; got != 2 {
		t.Fatalf("AcceptErrors = %d, want 2", got)
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if err := <-serveDone; err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
}

// sendGarbageAndWaitForClose opens a connection, sends bytes that are not an
// A-ASSOCIATE-RQ, and reads until the acceptor closes its side.
func sendGarbageAndWaitForClose(t *testing.T, ctx context.Context, address string) {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		t.Fatalf("dial for garbage: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: dicom\r\n\r\n")); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	// The acceptor answers with an A-P-ABORT and half-closes; reaching EOF
	// (or a reset) shows it gave up on this connection.
	_, _ = io.Copy(io.Discard, conn)
}

func TestAssociationServerKeepsServingAfterInvalidNegotiation(t *testing.T) {
	listener, err := Listen(ListenOptions{Address: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	handled := make(chan struct{})
	server, err := NewAssociationServer(listener, AssociationServerOptions{
		Accept: AcceptOptions{
			AETitle:                   "GARBAGE_SCP",
			SupportedAbstractSyntaxes: []string{verificationSOPClassUID},
			SupportedTransferSyntaxes: []string{ImplicitVRLittleEndian},
		},
		Handler: func(context.Context, *Association) error {
			close(handled)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewAssociationServer() error = %v", err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(context.Background()) }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sendGarbageAndWaitForClose(t, ctx, listener.Addr().String())
	assoc, err := DialContext(ctx, listener.Addr().String(), DialOptions{
		CalledAETitle: "GARBAGE_SCP",
		Contexts: []PresentationContext{{
			AbstractSyntaxUID:  verificationSOPClassUID,
			TransferSyntaxUIDs: []string{ImplicitVRLittleEndian},
		}},
	})
	if err != nil {
		t.Fatalf("Dial() after an invalid negotiation error = %v", err)
	}
	defer func() { _ = assoc.Close() }()
	select {
	case <-handled:
	case <-ctx.Done():
		t.Fatal("handler did not run after the invalid negotiation")
	}
	// The server counts the failure once its drain sees the garbage
	// connection close, which can be after the next association; wait for it.
	for server.Snapshot().NegotiationErrors != 1 && ctx.Err() == nil {
		time.Sleep(time.Millisecond)
	}
	metrics := server.Snapshot()
	if metrics.NegotiationErrors != 1 || metrics.Rejected != 0 || metrics.Accepted != 1 {
		t.Fatalf("metrics = %+v, want one negotiation error and one accepted association", metrics)
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if err := <-serveDone; err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
}
