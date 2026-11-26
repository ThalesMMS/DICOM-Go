// Package sourceport retries an outgoing TCP connect that failed before it
// reached the peer because the source endpoint the system chose for it could
// not be used. Under ephemeral port pressure Windows can choose a port whose
// TIME_WAIT connection went to the same remote endpoint, and the connect fails
// with WSAEADDRINUSE (Tcpip event 4227); an exhausted range fails with no
// buffer space. Another attempt, from a port the system chooses afresh, can
// succeed. net/ul (#2007) and the other clients of this module share the
// policy (#2011): a bounded number of further attempts within the dial
// context, never with a configured source port, and never for an error that
// another attempt does not change.
package sourceport

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Kind is the kind of a dial error that names the local side of the
// connection.
type Kind int

const (
	NotLocalEndpoint Kind = iota
	// AddressInUse: the source endpoint is in use. On Windows this is also
	// the connect failure behind Tcpip event 4227: under ephemeral port
	// pressure the system reuses a port whose TIME_WAIT connection went to
	// the same remote endpoint, and the connect fails with WSAEADDRINUSE.
	AddressInUse
	// AddressNotAvailable: the address is not available. It names an
	// exhausted ephemeral range or a source IP that is not on this host, and
	// on Windows also an invalid remote address, such as port 0.
	AddressNotAvailable
	// NoBuffers: no socket resource is left, as when the ephemeral range is
	// exhausted.
	NoBuffers
)

// Classify returns the kind of err.
func Classify(err error) Kind {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return NotLocalEndpoint
	}
	return errnoKind(errno)
}

// Unavailable reports whether a TCP dial failed with an error that names the
// local side of the connection: address in use, address not available, or no
// buffer space left.
func Unavailable(err error) bool {
	return Classify(err) != NotLocalEndpoint
}

// RetryDelays are the pauses before the further connect attempts when the
// source endpoint the system chose could not be used. Each attempt lets the
// system choose another ephemeral port.
var RetryDelays = []time.Duration{10 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond}

// Retryable reports whether a fresh ephemeral source port can avoid err,
// given the configured local address and the destination.
func Retryable(err error, local *net.TCPAddr, destination string) bool {
	if local != nil && local.Port != 0 {
		// A configured source port would only fail again.
		return false
	}
	switch Classify(err) {
	case AddressInUse, NoBuffers:
		return true
	case AddressNotAvailable:
		// A source IP that is not on this host, or an invalid remote
		// address, stays unavailable however often the dial is repeated.
		return (local == nil || len(local.IP) == 0 || local.IP.IsUnspecified()) && ValidRemoteAddress(destination)
	}
	return false
}

// ValidRemoteAddress reports whether destination can be connected to: a
// nonzero port and, for an IP literal, an address other than the unspecified
// one. A host name is taken as valid.
func ValidRemoteAddress(destination string) bool {
	host, port, err := net.SplitHostPort(destination)
	if err != nil {
		return false
	}
	if n, err := strconv.Atoi(port); err != nil || n <= 0 || n > 65535 {
		return false
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		return false
	}
	return true
}

// Retry runs dial again, within ctx and up to len(RetryDelays) more times,
// while it fails with an error a fresh ephemeral source port can avoid
// (Retryable). Any other error, such as a refusal by the peer, is returned at
// once, and so is the last error when ctx ends during a pause.
func Retry(ctx context.Context, local *net.TCPAddr, destination string, dial func(context.Context) (net.Conn, error)) (net.Conn, error) {
	for attempt := 0; ; attempt++ {
		conn, err := dial(ctx)
		if err == nil || attempt >= len(RetryDelays) || !Retryable(err, local, destination) {
			return conn, err
		}
		if conn != nil {
			_ = conn.Close()
		}
		timer := time.NewTimer(RetryDelays[attempt])
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, err
		case <-timer.C:
		}
	}
}

// DialContext returns a dial function, for an http.Transport or any other
// TCP client, that dials with d and applies Retry to the "tcp", "tcp4" and
// "tcp6" networks. Other networks, such as UDP, are dialed once. A dialer
// with a LocalAddr other than a *net.TCPAddr is dialed once, and a configured
// source port is never retried. A nil d is the dialer of
// http.DefaultTransport: a 30 second connect timeout and 30 second keep-alive.
func DialContext(d *net.Dialer) func(ctx context.Context, network, address string) (net.Conn, error) {
	if d == nil {
		d = &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		local, isTCP := d.LocalAddr.(*net.TCPAddr)
		if !tcpNetwork(network) || (d.LocalAddr != nil && !isTCP) {
			return d.DialContext(ctx, network, address)
		}
		first, _ := ctx.Value(firstAttemptKey{}).(*FirstAttempt)
		attempt := 0
		return Retry(ctx, local, address, func(ctx context.Context) (net.Conn, error) {
			attempt++
			if attempt == 1 && first != nil && local == nil {
				forced := *d
				forced.LocalAddr = first.Local
				conn, err := forced.DialContext(ctx, network, address)
				if Unavailable(err) {
					first.collided.Store(true)
				}
				return conn, err
			}
			return d.DialContext(ctx, network, address)
		})
	}
}

func tcpNetwork(network string) bool {
	switch network {
	case "tcp", "tcp4", "tcp6":
		return true
	}
	return false
}

type firstAttemptKey struct{}

// FirstAttempt makes the first connect attempt of a DialContext dial that
// leaves the source endpoint to the system use Local instead, as if the system
// had chosen it. Tests use it, through WithFirstAttempt, to make a client's
// real dial path meet a source port collision, such as an endpoint still in
// TIME_WAIT to the same listener; later attempts are unaffected.
type FirstAttempt struct {
	Local    *net.TCPAddr
	collided atomic.Bool
}

// Collided reports whether a dial made its first attempt from Local and that
// attempt failed because the endpoint could not be used.
func (f *FirstAttempt) Collided() bool { return f.collided.Load() }

// WithFirstAttempt returns a context that carries first to the DialContext
// dials made under it.
func WithFirstAttempt(ctx context.Context, first *FirstAttempt) context.Context {
	return context.WithValue(ctx, firstAttemptKey{}, first)
}

var defaultTransport = sync.OnceValue(func() *http.Transport { return retryingClone(http.DefaultTransport) })

// retryingClone returns a clone of the default transport whose dial applies
// Retry.
func retryingClone(defaultTransport http.RoundTripper) *http.Transport {
	base, ok := defaultTransport.(*http.Transport)
	if !ok || base == nil {
		// A program replaced http.DefaultTransport with another
		// RoundTripper: start from the settings net/http gives its own.
		base = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
	}
	transport := base.Clone()
	transport.DialContext = DialContext(nil)
	return transport
}

var defaultClient = sync.OnceValue(func() *http.Client {
	return &http.Client{Transport: defaultTransport()}
})

// HTTPTransport returns the shared transport of the library's default
// clients: a clone of http.DefaultTransport whose dial applies Retry. The
// clone is taken at first use, so later changes to http.DefaultTransport do
// not reach it, and it keeps a connection pool of its own.
func HTTPTransport() *http.Transport { return defaultTransport() }

// HTTPClient returns the shared client, like http.DefaultClient, whose
// transport is HTTPTransport. It is not http.DefaultClient: changes a program
// makes to http.DefaultClient do not reach it. Callers copy it before
// changing a field.
func HTTPClient() *http.Client { return defaultClient() }
