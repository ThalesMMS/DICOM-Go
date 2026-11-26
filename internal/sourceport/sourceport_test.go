package sourceport_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/sourceport"
	"github.com/ThalesMMS/dicom-go/internal/sourceport/sourceporttest"
)

// countingDialer counts the sockets a dial creates, one per connect attempt.
func countingDialer(attempts *atomic.Int32) *net.Dialer {
	return &net.Dialer{Control: func(string, string, syscall.RawConn) error {
		attempts.Add(1)
		return nil
	}}
}

// TestDialContextRetriesASourcePortCollision reproduces #2011: under
// ephemeral port pressure the system hands an outgoing connection a source
// port whose TIME_WAIT connection went to the same peer, and the connect
// fails before the peer is reached. The dial must try again from a fresh port.
func TestDialContextRetriesASourcePortCollision(t *testing.T) {
	server, _ := sourceporttest.Server(t)
	ctx, collided := sourceporttest.Context(t, context.Background(), server)
	var attempts atomic.Int32
	conn, err := sourceport.DialContext(countingDialer(&attempts))(ctx, "tcp4", server)
	if err != nil {
		t.Fatalf("dial after a source port collision: %v", err)
	}
	_ = conn.Close()
	if !collided() {
		t.Fatal("the first attempt did not meet the source port collision")
	}
	if n := attempts.Load(); n < 2 || n > int32(len(sourceport.RetryDelays))+1 {
		t.Fatalf("dial made %d attempts, want 2 to %d", n, len(sourceport.RetryDelays)+1)
	}
}

func TestDialContextKeepsAConfiguredSourcePort(t *testing.T) {
	server, _ := sourceporttest.Server(t)
	local := sourceporttest.Collision(t, server)
	var attempts atomic.Int32
	d := countingDialer(&attempts)
	d.LocalAddr = local
	_, err := sourceport.DialContext(d)(context.Background(), "tcp4", server)
	if !sourceport.Unavailable(err) || attempts.Load() != 1 {
		t.Fatalf("dial from a configured source port = %v after %d attempts, want an unavailable endpoint after 1", err, attempts.Load())
	}
}

// Only TCP connects are retried; a UDP "connect" never reaches the peer and
// is dialed once.
func TestDialContextDialsOtherNetworksOnce(t *testing.T) {
	server, _ := sourceporttest.Server(t)
	ctx, _ := sourceporttest.Context(t, context.Background(), server)
	var attempts atomic.Int32
	conn, err := sourceport.DialContext(countingDialer(&attempts))(ctx, "udp4", server)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if attempts.Load() != 1 {
		t.Fatalf("UDP dial made %d attempts, want 1", attempts.Load())
	}
}

func TestDialContextStopsWithTheContext(t *testing.T) {
	server, _ := sourceporttest.Server(t)
	local := sourceporttest.Collision(t, server)
	ctx, cancel := context.WithCancel(sourceport.WithFirstAttempt(context.Background(), &sourceport.FirstAttempt{Local: local}))
	var attempts atomic.Int32
	d := countingDialer(&attempts)
	d.Control = func(string, string, syscall.RawConn) error {
		attempts.Add(1)
		cancel()
		return nil
	}
	_, err := sourceport.DialContext(d)(ctx, "tcp4", server)
	if err == nil || attempts.Load() != 1 {
		t.Fatalf("cancelled dial = %v after %d attempts, want a failure after 1", err, attempts.Load())
	}
}

func TestDialContextDoesNotRetryARefusal(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := ln.Addr().String()
	_ = ln.Close()
	var attempts atomic.Int32
	_, err = sourceport.DialContext(countingDialer(&attempts))(context.Background(), "tcp4", address)
	if err == nil || sourceport.Unavailable(err) || attempts.Load() != 1 {
		t.Fatalf("dial to a closed port = %v after %d attempts, want a refusal after 1", err, attempts.Load())
	}
}

// The shared HTTP client, which library clients use when the caller supplies
// none, takes the retry through net/http.
func TestHTTPClientRetriesASourcePortCollision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	address := server.Listener.Addr().String()
	ctx, collided := sourceporttest.Context(t, context.Background(), address)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := sourceport.HTTPClient().Do(request)
	if err != nil {
		t.Fatalf("request after a source port collision: %v", err)
	}
	_ = response.Body.Close()
	if !collided() {
		t.Fatal("the request did not meet the source port collision")
	}
	if sourceport.HTTPClient().Transport != sourceport.HTTPTransport() {
		t.Fatal("the shared client does not use the shared transport")
	}
}
