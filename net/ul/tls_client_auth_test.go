package ul

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// muteAfterFlight drops what a TLS server writes once it has written its
// handshake flight and read again: the refusal alert of a server that
// verifies the client's last flight.
type muteAfterFlight struct {
	net.Conn
	mu            sync.Mutex
	wroteFlight   bool
	readAfterward bool
}

func (c *muteAfterFlight) Read(p []byte) (int, error) {
	c.mu.Lock()
	if c.wroteFlight {
		c.readAfterward = true
	}
	c.mu.Unlock()
	return c.Conn.Read(p)
}

func (c *muteAfterFlight) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.wroteFlight = true
	mute := c.readAfterward
	c.mu.Unlock()
	if mute {
		return len(p), nil
	}
	return c.Conn.Write(p)
}

// clientAuthServer accepts one connection, runs the TLS server handshake
// with config and, whatever its result, ends the connection at once without
// answering the A-ASSOCIATE-RQ: with a TCP reset, or with a FIN after which
// it reads until the requestor closes, so that nothing it sent is lost. With
// mute, the alert of a failed handshake is not sent. The reset does not wait
// for the requestor to read what the server sent after the handshake.
func clientAuthServer(t *testing.T, config *tls.Config, mute, reset bool) (string, <-chan error) {
	t.Helper()
	return serveClientAuth(t, config, mute, reset, nil)
}

// clientAuthServerResetWhen is clientAuthServer with a reset that waits,
// after the handshake, for beforeReset to return. beforeReset receives the
// number of bytes the server sent, its alert included.
func clientAuthServerResetWhen(t *testing.T, config *tls.Config, beforeReset func(sent int64)) (string, <-chan error) {
	t.Helper()
	return serveClientAuth(t, config, false, true, beforeReset)
}

// sentCountingConn counts the bytes written to the connection.
type sentCountingConn struct {
	net.Conn
	sent atomic.Int64
}

func (c *sentCountingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.sent.Add(int64(n))
	return n, err
}

func serveClientAuth(t *testing.T, config *tls.Config, mute, reset bool, beforeReset func(sent int64)) (string, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// The listener closes after one connection, or after a while without
	// one, such as when the requestor failed to connect: the handshake
	// result then arrives all the same.
	stop := time.AfterFunc(10*time.Second, func() { _ = listener.Close() })
	t.Cleanup(func() { stop.Stop(); _ = listener.Close() })
	handshake := make(chan error, 1)
	go func() {
		raw, err := listener.Accept()
		_ = listener.Close()
		if err != nil {
			handshake <- err
			return
		}
		tcp := raw.(*net.TCPConn)
		counted := &sentCountingConn{Conn: tcp}
		var conn net.Conn = counted
		if mute {
			conn = &muteAfterFlight{Conn: counted}
		}
		server := tls.Server(conn, config)
		_ = server.SetDeadline(time.Now().Add(5 * time.Second))
		handshake <- server.Handshake()
		if beforeReset != nil {
			beforeReset(counted.sent.Load())
		}
		if reset {
			_ = tcp.SetLinger(0)
			_ = tcp.Close()
			return
		}
		_ = tcp.CloseWrite()
		_ = tcp.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _ = io.Copy(io.Discard, tcp)
		_ = tcp.Close()
	}()
	return listener.Addr().String(), handshake
}

// testClientCertificate issues a self-signed client certificate that no
// server in these tests trusts.
func testClientCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "stranger"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// A TLS 1.3 server judges the client certificate after the client's
// handshake has returned. Its refusal reads as the alert, or, when it ends
// the stream without one, as a refused client certificate. A reset with
// nothing before it cannot tell a refusal from an accepted client, so it
// stays a closed connection, and an accepted client is never a refusal.
func TestDialNamesTLS13ClientCertificateRefusalHoweverTheCloseArrives(t *testing.T) {
	serverConfig, clientConfig := testTLSConfigs(t)
	stranger := testClientCertificate(t)
	roots := x509.NewCertPool()
	trusted := testClientCertificate(t)
	trustedRoots := x509.NewCertPool()
	trustedLeaf, err := x509.ParseCertificate(trusted.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	trustedRoots.AddCert(trustedLeaf)
	// The server verifies the client certificate and trusts this one.
	trustsCertificate := func(c *tls.Config) { c.ClientCAs = trustedRoots }
	presentsTrusted := func(c *tls.Config) { c.Certificates = []tls.Certificate{trusted} }
	const (
		refusedByAlert = "alert"
		refusedByClose = "close"
		// The server refused the client but sent no alert and reset the
		// connection: the requestor has no evidence of the refusal.
		refusedUnseen = "unseen"
		notRefused    = "none"
	)
	for _, tc := range []struct {
		name        string
		server      func(*tls.Config)
		client      func(*tls.Config)
		mute, reset bool
		want        string
		sent        bool
	}{
		{name: "no certificate, alert then FIN", want: refusedByAlert},
		{name: "no certificate, FIN without alert", mute: true, want: refusedByClose},
		{name: "no certificate, reset without alert", mute: true, reset: true, want: refusedUnseen},
		{name: "untrusted certificate, alert then FIN", client: func(c *tls.Config) { c.Certificates = []tls.Certificate{stranger} }, want: refusedByAlert},
		{name: "untrusted certificate, FIN without alert", client: func(c *tls.Config) { c.Certificates = []tls.Certificate{stranger} },
			mute: true, want: refusedByClose, sent: true},
		{name: "untrusted certificate, reset without alert", client: func(c *tls.Config) { c.Certificates = []tls.Certificate{stranger} },
			mute: true, reset: true, want: refusedUnseen},
		{name: "certificate chosen by the callback, FIN without alert", client: func(c *tls.Config) {
			c.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &stranger, nil }
		}, mute: true, want: refusedByClose, sent: true},
		// Controls: the server accepted the handshake, or the close is not
		// about a client certificate. The requestor's configuration is the
		// one Twin uses, without a ClientSessionCache of its own: the
		// session ticket a server sends once it accepted the client must
		// arrive all the same. The server resets the connection as soon as
		// its handshake returns, without waiting for the requestor to read
		// the ticket.
		{name: "trusted certificate accepted, then FIN", server: trustsCertificate, client: presentsTrusted, want: notRefused},
		{name: "trusted certificate accepted, then reset", server: trustsCertificate, client: presentsTrusted, reset: true, want: notRefused},
		{name: "optional certificate not sent and accepted, then FIN", server: func(c *tls.Config) { c.ClientAuth = tls.VerifyClientCertIfGiven }, want: notRefused},
		{name: "optional certificate not sent and accepted, then reset", server: func(c *tls.Config) { c.ClientAuth = tls.VerifyClientCertIfGiven },
			reset: true, want: notRefused},
		{name: "trusted certificate accepted with the caller's session cache, then FIN", server: trustsCertificate, client: func(c *tls.Config) {
			presentsTrusted(c)
			c.ClientSessionCache = tls.NewLRUClientSessionCache(1)
		}, want: notRefused},
		{name: "TLS 1.2 certificate requested, then FIN", server: func(c *tls.Config) {
			c.ClientAuth, c.MinVersion, c.MaxVersion = tls.RequestClientCert, tls.VersionTLS12, tls.VersionTLS12
		}, want: notRefused},
		{name: "no certificate requested, then FIN", server: func(c *tls.Config) { c.ClientAuth = tls.NoClientCert }, want: notRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := serverConfig.Clone()
			server.MinVersion, server.ClientAuth, server.ClientCAs = tls.VersionTLS13, tls.RequireAndVerifyClientCert, roots
			if tc.server != nil {
				tc.server(server)
			}
			client := clientConfig.Clone()
			if tc.client != nil {
				tc.client(client)
			}
			address, handshake := clientAuthServer(t, server, tc.mute, tc.reset)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			assoc, err := DialContext(ctx, address, DialOptions{
				CalledAETitle: "SCP", CallingAETitle: "SCU", TLSConfig: client, ARTIMTimeout: -1,
				Contexts: []PresentationContext{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}}},
			})
			if err == nil {
				_ = assoc.Close()
				t.Fatal("the association was established")
			}
			serverErr := <-handshake
			if (tc.want == notRefused) != (serverErr == nil) {
				t.Fatalf("server handshake error %v (Dial error %v)", serverErr, err)
			}
			var refused *TLSClientCertificateRefusedError
			isRefused := errors.As(err, &refused)
			switch tc.want {
			case refusedByAlert:
				if isRefused || !strings.Contains(err.Error(), "remote error") {
					t.Fatalf("Dial error = %v, want the server's alert", err)
				}
			case refusedByClose:
				if !isRefused {
					t.Fatalf("Dial error = %v (%T), want a TLSClientCertificateRefusedError", err, err)
				}
				if refused.CertificateSent != tc.sent {
					t.Fatalf("CertificateSent = %t, want %t", refused.CertificateSent, tc.sent)
				}
				if !closedByPeer(err) {
					t.Fatalf("the refusal %v no longer wraps the closed connection", err)
				}
			case refusedUnseen, notRefused:
				if isRefused || !closedByPeer(err) {
					t.Fatalf("Dial error = %v, want a closed connection that is not a refusal", err)
				}
			}
		})
	}
}

// dialClientAuthServer runs one association request against clientAuthServer
// and returns the requestor's error and the server's handshake error.
func dialClientAuthServer(t *testing.T, server, client *tls.Config, reset bool) (error, error) {
	t.Helper()
	address, handshake := clientAuthServer(t, server, false, reset)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	assoc, err := DialContext(ctx, address, DialOptions{
		CalledAETitle: "SCP", CallingAETitle: "SCU", TLSConfig: client, ARTIMTimeout: -1,
		Contexts: []PresentationContext{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}}},
	})
	if err == nil {
		_ = assoc.Close()
		t.Fatal("the association was established")
	}
	return err, <-handshake
}

// A server that accepted the client and resets the connection as soon as its
// handshake returns races its session ticket against the reset. Whichever
// arrives first, the requestor never reports a refused client certificate.
func TestDialNeverReportsAnAcceptedClientResetAsARefusal(t *testing.T) {
	serverConfig, clientConfig := testTLSConfigs(t)
	trusted := testClientCertificate(t)
	leaf, err := x509.ParseCertificate(trusted.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	server := serverConfig.Clone()
	server.MinVersion, server.ClientAuth, server.ClientCAs = tls.VersionTLS13, tls.RequireAndVerifyClientCert, roots
	optional := server.Clone()
	optional.ClientAuth = tls.VerifyClientCertIfGiven
	for attempt := range 100 {
		server, client := server, clientConfig.Clone()
		if attempt%2 == 0 {
			client.Certificates = []tls.Certificate{trusted}
		} else {
			server = optional
		}
		dialErr, serverErr := dialClientAuthServer(t, server, client, true)
		if serverErr != nil {
			t.Fatalf("attempt %d: server handshake error %v (Dial error %v)", attempt, serverErr, dialErr)
		}
		var refused *TLSClientCertificateRefusedError
		if errors.As(dialErr, &refused) || !closedByPeer(dialErr) {
			t.Fatalf("attempt %d: Dial error = %v, want a closed connection that is not a refusal", attempt, dialErr)
		}
	}
}

// writingUntilReset holds the requestor's writes after its last handshake
// flight, the A-ASSOCIATE-RQ first, until the server resets the connection,
// so that the requestor is busy writing, not reading, while what the server
// sent after the handshake arrives and until the reset. It counts what the
// requestor's reads received.
type writingUntilReset struct {
	net.Conn
	certificateRequested *atomic.Bool
	writesAfterRequest   atomic.Int32
	resetting            <-chan struct{}
	received             atomic.Int64
	// receivedMore is signaled after each read that received something.
	receivedMore chan struct{}
}

func (c *writingUntilReset) Write(p []byte) (int, error) {
	if c.certificateRequested.Load() && c.writesAfterRequest.Add(1) > 1 {
		select {
		case <-c.resetting:
		case <-time.After(10 * time.Second):
		}
	}
	return c.Conn.Write(p)
}

func (c *writingUntilReset) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.received.Add(int64(n))
		select {
		case c.receivedMore <- struct{}{}:
		default:
		}
	}
	return n, err
}

// The server refuses the client with its alert and resets the connection
// while the requestor is still writing its A-ASSOCIATE-RQ, once a read of
// the requestor has received everything the server sent, the alert
// included. Only the waiting read can receive it while the requestor
// writes, and what a read received is not discarded by the reset: the
// failure is the alert every time. Without the waiting read nothing reads
// the alert before the reset, which then discards it, and the failure was a
// bare reset.
func TestDialReadsTheTLS13ClientCertificateAlertBeforeTheReset(t *testing.T) {
	serverConfig, clientConfig := testTLSConfigs(t)
	server := serverConfig.Clone()
	server.MinVersion, server.ClientAuth, server.ClientCAs = tls.VersionTLS13, tls.RequireAndVerifyClientCert, x509.NewCertPool()
	for attempt := range 10 {
		var requested atomic.Bool
		client := clientConfig.Clone()
		client.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			requested.Store(true)
			return new(tls.Certificate), nil
		}
		resetting := make(chan struct{})
		var unread atomic.Bool
		conn := make(chan *writingUntilReset, 1)
		// The reset waits for the requestor to receive the alert, not for a
		// time: an abortive close discards what the server has not sent yet,
		// and the requestor's what it has not read yet. A requestor that
		// does not read the alert while it writes is reset after a while.
		address, handshake := clientAuthServerResetWhen(t, server, func(sent int64) {
			defer close(resetting)
			var watched *writingUntilReset
			select {
			case watched = <-conn:
			case <-time.After(5 * time.Second):
				unread.Store(true)
				return
			}
			gaveUp := time.After(2 * time.Second)
			for watched.received.Load() < sent {
				select {
				case <-watched.receivedMore:
				case <-gaveUp:
					unread.Store(true)
					return
				}
			}
		})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := DialContext(ctx, address, DialOptions{
			CalledAETitle: "SCP", CallingAETitle: "SCU", TLSConfig: client, ARTIMTimeout: -1,
			Transport: TransportOptions{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				var dialer net.Dialer
				tcp, err := dialer.DialContext(ctx, network, address)
				if err != nil {
					return nil, err
				}
				watched := &writingUntilReset{Conn: tcp, certificateRequested: &requested, resetting: resetting, receivedMore: make(chan struct{}, 1)}
				select {
				case conn <- watched:
				default:
				}
				return watched, nil
			}},
			Contexts: []PresentationContext{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}}},
		})
		cancel()
		if serverErr := <-handshake; serverErr == nil {
			t.Fatal("the server accepted a client without a certificate")
		}
		select {
		case <-resetting:
		case <-time.After(10 * time.Second):
			t.Fatalf("attempt %d: the server never reset the connection (Dial error %v)", attempt, err)
		}
		if unread.Load() {
			t.Fatalf("attempt %d: the requestor did not read the alert while it wrote (Dial error %v)", attempt, err)
		}
		if err == nil || !strings.Contains(err.Error(), "remote error: tls: certificate required") {
			t.Fatalf("attempt %d: Dial error = %v, want the server's alert", attempt, err)
		}
	}
}

// When the server sends its alert and resets the connection at once, the
// requestor reads the alert, or, when the reset discarded it, a bare closed
// connection, which is never reported as a refusal.
func TestDialReportsARefusalResetAtOnceAsTheAlertOrAClosedConnection(t *testing.T) {
	serverConfig, clientConfig := testTLSConfigs(t)
	server := serverConfig.Clone()
	server.MinVersion, server.ClientAuth, server.ClientCAs = tls.VersionTLS13, tls.RequireAndVerifyClientCert, x509.NewCertPool()
	lost := 0
	for attempt := range 100 {
		dialErr, serverErr := dialClientAuthServer(t, server, clientConfig.Clone(), true)
		if serverErr == nil {
			t.Fatalf("the server accepted a client without a certificate (Dial error %v)", dialErr)
		}
		var refused *TLSClientCertificateRefusedError
		switch {
		case strings.Contains(dialErr.Error(), "remote error: tls: certificate required"):
		case !errors.As(dialErr, &refused) && closedByPeer(dialErr):
			lost++
		default:
			t.Fatalf("attempt %d: Dial error = %v, want the alert or a bare closed connection", attempt, dialErr)
		}
	}
	t.Logf("the reset discarded the alert %d times in 100", lost)
}

// slowToReturnRead returns a read that received something after the server
// asked for a client certificate only after a pause, once it has closed
// received: the read holds the data but the requestor has not counted it.
type slowToReturnRead struct {
	net.Conn
	certificateRequested *atomic.Bool
	received             chan struct{}
	once                 sync.Once
}

func (c *slowToReturnRead) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && c.certificateRequested.Load() {
		c.once.Do(func() { close(c.received) })
		time.Sleep(200 * time.Millisecond)
	}
	return n, err
}

// A refusal alert that arrived while the requestor was still writing its
// A-ASSOCIATE-RQ, and that TLS has not read because the write failed first,
// is what the negotiation reports, even when the read that received the
// alert has not returned yet when the write fails.
func TestRefusalReadsTheAlertTheFailedWriteLeftUnread(t *testing.T) {
	serverConfig, clientConfig := testTLSConfigs(t)
	server := serverConfig.Clone()
	server.MinVersion, server.ClientAuth, server.ClientCAs = tls.VersionTLS13, tls.RequireAndVerifyClientCert, x509.NewCertPool()
	address, handshake := clientAuthServer(t, server, false, false)
	tcp, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	var requested atomic.Bool
	conn := &slowToReturnRead{Conn: tcp, certificateRequested: &requested, received: make(chan struct{})}
	defer conn.Close()
	client := clientConfig.Clone()
	client.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		requested.Store(true)
		return new(tls.Certificate), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, watch, err := clientTLSConn(ctx, conn, address, client)
	if err != nil {
		t.Fatal(err)
	}
	if serverErr := <-handshake; serverErr == nil {
		t.Fatal("the server accepted a client without a certificate")
	}
	select {
	case <-conn.received:
	case <-ctx.Done():
		t.Fatal("the alert never arrived")
	}
	writeErr := &net.OpError{Op: "write", Net: "tcp", Err: syscall.ECONNRESET}
	got := watch.refusal(writeErr)
	var alert *net.OpError
	if !errors.As(got, &alert) || alert.Op != "remote error" || !strings.Contains(got.Error(), "certificate required") {
		t.Fatalf("refusal = %v, want the server's alert", got)
	}
	if !errors.Is(got, syscall.ECONNRESET) {
		t.Fatalf("refusal = %v no longer wraps the failed write", got)
	}
}

// The waiting reads queue what they receive, let a passed read deadline end
// a read without ending later ones, and hand the connection back to TLS after
// the first read that completes once the handshake has returned.
func TestWatchedConnReadsAheadUntilAfterTheHandshake(t *testing.T) {
	local, peer := net.Pipe()
	defer peer.Close()
	conn := newWatchedConn(local)
	defer conn.Close()
	conn.arm()
	buffer := make([]byte, 8)

	go func() { _, _ = peer.Write([]byte("hello")) }()
	if n, err := conn.Read(buffer[:2]); err != nil || string(buffer[:n]) != "he" {
		t.Fatalf("Read = %q, %v", buffer[:n], err)
	}
	if n, err := conn.Read(buffer); err != nil || string(buffer[:n]) != "llo" {
		t.Fatalf("Read = %q, %v", buffer[:n], err)
	}

	conn.settled.Store(true)
	go func() { _, _ = peer.Write([]byte("ticket")) }()
	if n, err := conn.Read(buffer); err != nil || string(buffer[:n]) != "ticket" {
		t.Fatalf("Read = %q, %v", buffer[:n], err)
	}
	conn.mu.Lock()
	done := conn.done
	conn.mu.Unlock()
	if !done {
		t.Fatal("the reads went on after the first one past the handshake")
	}
	go func() { _, _ = peer.Write([]byte("AC")) }()
	if n, err := conn.Read(buffer); err != nil || string(buffer[:n]) != "AC" {
		t.Fatalf("Read = %q, %v", buffer[:n], err)
	}
	go func() { _ = peer.Close() }()
	if _, err := conn.Read(buffer); !errors.Is(err, io.EOF) {
		t.Fatalf("Read after the peer closed: %v, want io.EOF", err)
	}
	if conn.received.Load() != int64(len("helloticketAC")) || conn.delivered.Load() != conn.received.Load() {
		t.Fatalf("received %d, delivered %d", conn.received.Load(), conn.delivered.Load())
	}

	// A deadline that passes ends the waiting read; the next read waits
	// under the deadline in force then.
	local, peer = net.Pipe()
	defer peer.Close()
	conn = newWatchedConn(local)
	defer conn.Close()
	conn.arm()
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	var timeout net.Error
	if _, err := conn.Read(buffer); !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("Read past the deadline: %v, want a timeout", err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	go func() { _, _ = peer.Write([]byte("late")) }()
	if n, err := conn.Read(buffer); err != nil || string(buffer[:n]) != "late" {
		t.Fatalf("Read after the deadline was lifted = %q, %v", buffer[:n], err)
	}
}

// The requestor accepts session tickets without resuming sessions, and
// leaves a caller's own cache or refusal of tickets alone.
func TestAcceptSessionTicketsKeepsNoSessionAndRespectsTheCaller(t *testing.T) {
	config := &tls.Config{}
	acceptSessionTickets(config)
	cache, ok := config.ClientSessionCache.(discardSessionCache)
	if !ok {
		t.Fatalf("ClientSessionCache = %T, want discardSessionCache", config.ClientSessionCache)
	}
	cache.Put("peer", &tls.ClientSessionState{})
	if session, found := cache.Get("peer"); found || session != nil {
		t.Fatal("the cache kept a session")
	}

	own := tls.NewLRUClientSessionCache(1)
	config = &tls.Config{ClientSessionCache: own}
	acceptSessionTickets(config)
	if config.ClientSessionCache != own {
		t.Fatal("the caller's ClientSessionCache was replaced")
	}

	config = &tls.Config{SessionTicketsDisabled: true}
	acceptSessionTickets(config)
	if config.ClientSessionCache != nil {
		t.Fatal("a ClientSessionCache was set although the caller disabled session tickets")
	}
}
