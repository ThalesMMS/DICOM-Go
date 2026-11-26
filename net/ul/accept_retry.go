package ul

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"
)

// The pause before another accept after a failure that leaves the listener
// usable starts at acceptRetryInitialDelay and doubles up to
// acceptRetryMaxDelay, as in net/http.Server.Serve. A connection accepted
// after the failure ends the pauses; the next failure starts again at the
// initial delay.
const (
	acceptRetryInitialDelay = 5 * time.Millisecond
	acceptRetryMaxDelay     = time.Second
)

// connectionSetupError reports a connection the listener accepted and then
// closed because it could not apply the socket options to it. Only that
// connection is lost; the listener stays usable.
type connectionSetupError struct{ err error }

func (e *connectionSetupError) Error() string { return e.err.Error() }
func (e *connectionSetupError) Unwrap() error { return e.err }

// AcceptContext waits for the next connection until ctx ends or the listener
// is closed, as a server's accept loop does. An accept failure that leaves the
// listener usable is passed to onError, when it is not nil, and accepting
// resumes after a pause that starts at 5 ms and doubles up to 1 s. Such
// failures are running out of descriptors or buffers (EMFILE, ENFILE, ENOBUFS
// and ENOMEM; WSAEMFILE and WSAENOBUFS on Windows), errors that report
// themselves as temporary or as a timeout, and a connection closed because
// its socket buffers could not be set. Once ctx ends AcceptContext returns
// ctx's error, wrapping ErrAssociationTimeout after a deadline; any other
// failure, such as net.ErrClosed, is returned as the listener reported it.
// onError runs on the calling goroutine and must return promptly. As with
// AcceptAssociation, a listener that is not a TCP listener is closed when ctx
// ends during the wait.
func (l *Listener) AcceptContext(ctx context.Context, onError func(error)) (net.Conn, error) {
	if l == nil || l.listener == nil {
		return nil, net.ErrClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return acceptWithBackoff(ctx, func() (net.Conn, error) {
		conn, err := acceptWithContext(ctx, l.listener)
		if err != nil {
			return nil, err
		}
		if err := l.applyBuffers(conn); err != nil {
			_ = conn.Close()
			return nil, &connectionSetupError{err: err}
		}
		return conn, nil
	}, onError)
}

// acceptWithBackoff calls accept until it returns a connection or an error
// that ends accepting. A retryable failure goes to onError and is followed by
// a pause; ctx ends the pause and the loop.
func acceptWithBackoff(ctx context.Context, accept func() (net.Conn, error), onError func(error)) (net.Conn, error) {
	var delay time.Duration
	for {
		conn, err := accept()
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, acceptContextError(ctx)
		}
		if !retryableAcceptError(err) {
			return nil, err
		}
		if onError != nil {
			onError(err)
		}
		delay = nextAcceptRetryDelay(delay)
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, acceptContextError(ctx)
		}
	}
}

func nextAcceptRetryDelay(delay time.Duration) time.Duration {
	if delay <= 0 {
		return acceptRetryInitialDelay
	}
	delay *= 2
	if delay > acceptRetryMaxDelay {
		delay = acceptRetryMaxDelay
	}
	return delay
}

func acceptContextError(ctx context.Context) error {
	err := ctx.Err()
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %v", ErrAssociationTimeout, err)
	}
	return err
}

// retryableAcceptError reports whether an accept failure leaves the listener
// usable, so that accepting again can succeed.
func retryableAcceptError(err error) bool {
	if err == nil || errors.Is(err, net.ErrClosed) {
		return false
	}
	var setup *connectionSetupError
	if errors.As(err, &setup) {
		return true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) && acceptResourceErrno(errno) {
		return true
	}
	var temporary interface{ Temporary() bool }
	if errors.As(err, &temporary) && temporary.Temporary() {
		return true
	}
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}
