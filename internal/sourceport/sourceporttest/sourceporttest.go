// Package sourceporttest makes a real source port collision for the tests of
// the clients that use internal/sourceport.
package sourceporttest

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/sourceport"
)

// Collision returns a loopback local endpoint whose last connection to the
// TCP server at address this side closed first, so that it is still in
// TIME_WAIT. A new connection from that endpoint to the same server fails with
// the operating system's address-in-use error: on Windows the connect fails as
// it does when the system reuses such a port under ephemeral port pressure
// (Tcpip event 4227). The server must close a connection whose client ends
// its sending side, as net/http servers do. The test is skipped where the
// system lets a new connection reuse the endpoint.
func Collision(t testing.TB, address string) *net.TCPAddr {
	t.Helper()
	conn, err := net.DialTimeout("tcp4", address, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tcp := conn.(*net.TCPConn)
	local := tcp.LocalAddr().(*net.TCPAddr)
	if err := tcp.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = tcp.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.Copy(io.Discard, tcp); err != nil {
		t.Fatalf("the server did not close its side of the connection: %v", err)
	}
	_ = tcp.Close()
	endpoint := &net.TCPAddr{IP: local.IP, Port: local.Port}
	probe, err := (&net.Dialer{LocalAddr: endpoint}).Dial("tcp4", address)
	if err == nil {
		_ = probe.Close()
		t.Skip("this system lets a new connection reuse an endpoint in TIME_WAIT")
	}
	if !sourceport.Unavailable(err) {
		t.Fatalf("a connection from an endpoint in TIME_WAIT failed with %v, want an unavailable local endpoint", err)
	}
	return endpoint
}

// Context returns a context under which the first connect attempt of a
// sourceport dial to the server at address comes from a source endpoint that
// collides (Collision), as if the system had chosen it under port pressure.
// The returned function reports whether a dial met that collision; a client
// whose dial does not apply the source port retry never does.
func Context(t testing.TB, ctx context.Context, address string) (context.Context, func() bool) {
	t.Helper()
	first := &sourceport.FirstAttempt{Local: Collision(t, address)}
	return sourceport.WithFirstAttempt(ctx, first), first.Collided
}

// Server accepts connections on a loopback listener, discards what they send
// and closes each when its client ends its sending side. It returns the
// listener address and a function that reports, waiting up to five seconds,
// whether the server has accepted at least want connections. Collision makes
// one of them.
func Server(t testing.TB) (string, func(want int) bool) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var accepted atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			go func() {
				_, _ = io.Copy(io.Discard, conn)
				_ = conn.Close()
			}()
		}
	}()
	return ln.Addr().String(), func(want int) bool {
		for deadline := time.Now().Add(5 * time.Second); int(accepted.Load()) < want; {
			if time.Now().After(deadline) {
				return false
			}
			time.Sleep(time.Millisecond)
		}
		return true
	}
}
