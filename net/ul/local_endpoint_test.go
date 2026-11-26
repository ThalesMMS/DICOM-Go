package ul

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// timeWaitCollision returns a loopback listener and a local endpoint whose
// last connection to that listener the client closed first, so that it is
// still in TIME_WAIT. A new connection from that endpoint to the same listener
// fails with the operating system's address-in-use error: on Windows the
// connect fails as it does when the system reuses such a port under ephemeral
// port pressure (Tcpip event 4227).
func timeWaitCollision(t *testing.T) (listener string, local *net.TCPAddr) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	closed := make(chan struct{}, 1)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(io.Discard, conn)
				_ = conn.Close()
				closed <- struct{}{}
			}()
		}
	}()
	conn, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	local = conn.LocalAddr().(*net.TCPAddr)
	_ = conn.Close()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the listener did not close its side of the connection")
	}
	return ln.Addr().String(), &net.TCPAddr{IP: local.IP, Port: local.Port}
}

func collidingDial(t *testing.T) (string, *net.TCPAddr, error) {
	t.Helper()
	listener, local := timeWaitCollision(t)
	conn, err := (&net.Dialer{LocalAddr: local}).Dial("tcp4", listener)
	if err == nil {
		_ = conn.Close()
		t.Skip("this system lets a new connection reuse an endpoint in TIME_WAIT")
	}
	if !LocalEndpointUnavailable(err) {
		t.Fatalf("a connection from an endpoint in TIME_WAIT failed with %v, want an unavailable local endpoint", err)
	}
	return listener, local, err
}

// TestDialRetriesAnUnavailableSourcePort reproduces #2007: under ephemeral
// port pressure the system hands a C-MOVE association a source port whose
// TIME_WAIT connection went to the same peer, and the connect fails before
// the peer is reached. The dial must try again from a fresh port instead of
// failing the retrieve.
func TestDialRetriesAnUnavailableSourcePort(t *testing.T) {
	listener, local := timeWaitCollision(t)
	attempts := 0
	var first error
	conn, err := retryUnavailableSourcePort(context.Background(), nil, listener, func(ctx context.Context) (net.Conn, error) {
		attempts++
		dialer := &net.Dialer{}
		if attempts == 1 {
			// The system's choice of a port still in TIME_WAIT.
			dialer.LocalAddr = local
		}
		conn, err := dialer.DialContext(ctx, "tcp4", listener)
		if attempts == 1 {
			first = err
		}
		return conn, err
	})
	if first == nil {
		t.Skip("this system lets a new connection reuse an endpoint in TIME_WAIT")
	}
	if !LocalEndpointUnavailable(first) {
		t.Fatalf("the first attempt failed with %v, want an unavailable local endpoint", first)
	}
	if err != nil {
		t.Fatalf("dial after an unavailable source port: %v", err)
	}
	_ = conn.Close()
	// Under port pressure a later port the system chooses may collide too;
	// the dial must only have tried again and succeeded within its bound.
	if attempts < 2 || attempts > len(sourcePortRetryDelays)+1 {
		t.Fatalf("dial made %d attempts, want 2 to %d", attempts, len(sourcePortRetryDelays)+1)
	}
}

// addressNotAvailable returns the error of a dial from a source IP that is not
// on this host (TEST-NET-1).
func addressNotAvailable(t *testing.T) error {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	conn, err := (&net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1)}}).Dial("tcp4", ln.Addr().String())
	if err == nil {
		_ = conn.Close()
		t.Skip("this host has the TEST-NET-1 address 192.0.2.1")
	}
	if classifyLocalEndpointError(err) != localAddressNotAvailable {
		t.Skipf("a dial from a foreign source IP failed with %v, not an address-not-available error", err)
	}
	return err
}

// An address that is not available is retried only when the system chose the
// source address and the remote address is a valid one: a configured source
// IP missing from the host, or (on Windows) a remote port 0, fails again on
// every attempt.
func TestDialRetriesAnUnavailableAddressOnlyWithoutASourceIP(t *testing.T) {
	notAvailable := addressNotAvailable(t)
	for _, c := range []struct {
		name        string
		local       *net.TCPAddr
		destination string
		want        bool
	}{
		{"system source", nil, "127.0.0.1:104", true},
		{"unspecified source IP", &net.TCPAddr{IP: net.IPv4zero}, "127.0.0.1:104", true},
		{"configured source IP", &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1)}, "127.0.0.1:104", false},
		{"remote port 0", nil, "127.0.0.1:0", false},
		{"unspecified remote", nil, "0.0.0.0:104", false},
		{"host name", nil, "pacs.example:104", true},
	} {
		if got := retryableSourceEndpointError(notAvailable, c.local, c.destination); got != c.want {
			t.Errorf("%s: retryable = %t, want %t", c.name, got, c.want)
		}
	}

	attempts := 0
	foreign := &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1)}
	_, err := retryUnavailableSourcePort(context.Background(), foreign, "127.0.0.1:104", func(context.Context) (net.Conn, error) {
		attempts++
		return nil, notAvailable
	})
	if !errors.Is(err, notAvailable) || attempts != 1 {
		t.Fatalf("dial from a source IP missing from the host = %v after %d attempts, want it after 1", err, attempts)
	}
}

// A dial to remote port 0 fails at once, whatever error the system reports
// for it (WSAEADDRNOTAVAIL on Windows).
func TestDialDoesNotRetryAnInvalidRemoteAddress(t *testing.T) {
	attempts := 0
	start := time.Now()
	_, err := retryUnavailableSourcePort(context.Background(), nil, "127.0.0.1:0", func(ctx context.Context) (net.Conn, error) {
		attempts++
		return (&net.Dialer{}).DialContext(ctx, "tcp4", "127.0.0.1:0")
	})
	if err == nil || attempts != 1 {
		t.Fatalf("dial to port 0 = %v after %d attempts (%s), want a failure after 1", err, attempts, time.Since(start))
	}
}

func TestDialKeepsAConfiguredSourcePort(t *testing.T) {
	listener, local, collision := collidingDial(t)
	attempts := 0
	_, err := retryUnavailableSourcePort(context.Background(), local, listener, func(ctx context.Context) (net.Conn, error) {
		attempts++
		return (&net.Dialer{LocalAddr: local}).DialContext(ctx, "tcp4", listener)
	})
	if !LocalEndpointUnavailable(err) || attempts != 1 {
		t.Fatalf("dial from a configured source port = %v after %d attempts, want %v after 1", err, attempts, collision)
	}
}

func TestDialRetriesAnUnavailableSourcePortAFewTimes(t *testing.T) {
	listener, _, collision := collidingDial(t)
	attempts := 0
	_, err := retryUnavailableSourcePort(context.Background(), &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}, listener, func(context.Context) (net.Conn, error) {
		attempts++
		return nil, collision
	})
	if !errors.Is(err, collision) || attempts != len(sourcePortRetryDelays)+1 {
		t.Fatalf("dial = %v after %d attempts, want the collision after %d", err, attempts, len(sourcePortRetryDelays)+1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	attempts = 0
	_, err = retryUnavailableSourcePort(ctx, nil, listener, func(context.Context) (net.Conn, error) {
		attempts++
		cancel()
		return nil, collision
	})
	if !errors.Is(err, collision) || attempts != 1 {
		t.Fatalf("cancelled dial = %v after %d attempts, want the collision after 1", err, attempts)
	}
}

func TestDialDoesNotRetryOtherFailures(t *testing.T) {
	refused := errors.New("connection refused by the peer")
	attempts := 0
	_, err := retryUnavailableSourcePort(context.Background(), nil, "127.0.0.1:104", func(context.Context) (net.Conn, error) {
		attempts++
		return nil, refused
	})
	if !errors.Is(err, refused) || attempts != 1 {
		t.Fatalf("dial = %v after %d attempts, want the refusal after 1", err, attempts)
	}
	if LocalEndpointUnavailable(refused) || LocalEndpointUnavailable(nil) {
		t.Fatal("an error without an unavailable local endpoint was classified as one")
	}
}
