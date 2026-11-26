package audit

import (
	"bufio"
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
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// readFrames reads n octet-counted RFC 6587 frames.
func readFrames(t *testing.T, conn net.Conn, n int) []string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(conn)
	var frames []string
	for len(frames) < n {
		length, err := reader.ReadString(' ')
		if err != nil {
			t.Fatalf("read frame length: %v", err)
		}
		size, err := strconv.Atoi(strings.TrimSpace(length))
		if err != nil {
			t.Fatalf("frame length %q", length)
		}
		frame := make([]byte, size)
		if _, err := io.ReadFull(reader, frame); err != nil {
			t.Fatal(err)
		}
		frames = append(frames, string(frame))
	}
	return frames
}

func checkRecord(t *testing.T, record string, outcome string) {
	t.Helper()
	pri := "<85>1 " // authpriv notice
	if outcome != OutcomeSuccess {
		pri = "<84>1 "
	}
	if !strings.HasPrefix(record, pri) || !strings.Contains(record, " TESTHOST twin ") || !strings.Contains(record, " IHE+RFC-3881 - \xef\xbb\xbf<?xml") {
		t.Fatalf("record header = %q", record[:min(len(record), 120)])
	}
}

func TestSyslogSpoolSurvivesNetworkFailureWithinLimits(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var down atomic.Bool
	down.Store(true)
	logger := &SyslogLogger{
		Network: SyslogTCP, Address: listener.Addr().String(), AppName: "twin", Hostname: "TESTHOST",
		MaxSpoolMessages: 3,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			if down.Load() {
				return nil, errors.New("simulated network failure")
			}
			var d net.Dialer
			return d.DialContext(ctx, network, address)
		},
	}
	messages := atnaMessages()
	names := []string{"application start", "query", "login", "logout", "security alert"}
	for _, name := range names {
		if err := logger.Log(messages[name]); err != nil {
			t.Fatal(err)
		}
		if err := logger.Flush(context.Background()); err == nil {
			t.Fatal("flush succeeded while the network is down")
		}
		if stats := logger.Stats(); stats.Spooled > 3 {
			t.Fatalf("spool grew to %d messages, limit 3", stats.Spooled)
		}
	}
	if stats := logger.Stats(); stats.Spooled != 3 || stats.Dropped != 2 {
		t.Fatalf("stats = %+v, want 3 spooled and the 2 oldest dropped", stats)
	}

	down.Store(false)
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	if err := logger.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	conn := <-accepted
	defer conn.Close()
	frames := readFrames(t, conn, 3)
	for i, name := range names[2:] {
		checkRecord(t, frames[i], messages[name].Event.OutcomeIndicator)
		if !strings.Contains(frames[i], messages[name].Event.ID.Value) {
			t.Fatalf("frame %d is not %s", i, name)
		}
	}
	if stats := logger.Stats(); stats.Spooled != 0 || stats.SpooledBytes != 0 {
		t.Fatalf("spool not drained: %+v", stats)
	}
	_ = logger.Close()
}

func TestSyslogByteLimit(t *testing.T) {
	logger := &SyslogLogger{Network: SyslogTCP, Address: "127.0.0.1:1", MaxSpoolBytes: 4000}
	for i := 0; i < 10; i++ {
		if err := logger.Log(atnaMessages()["query"]); err != nil {
			t.Fatal(err)
		}
	}
	if stats := logger.Stats(); stats.SpooledBytes > 4000 || stats.Dropped == 0 {
		t.Fatalf("stats = %+v, want at most 4000 bytes", stats)
	}
	if err := (&SyslogLogger{Network: "smtp", Address: "x"}).Log(atnaMessages()["query"]); !errors.Is(err, ErrSyslogConfig) {
		t.Fatalf("bad network error = %v", err)
	}
	if err := (&SyslogLogger{Network: SyslogTLS, Address: "x:1"}).Log(atnaMessages()["query"]); !errors.Is(err, ErrSyslogConfig) {
		t.Fatalf("TLS without config error = %v", err)
	}
}

func testCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "audit repository"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(certificate)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func TestSyslogTLSAndRun(t *testing.T) {
	certificate, pool := testCertificate(t)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	logger := &SyslogLogger{Network: SyslogTLS, Address: listener.Addr().String(), TLS: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		AppName: "twin", Hostname: "TESTHOST", RetryInterval: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- logger.Run(ctx) }()
	if err := logger.Log(atnaMessages()["instances transferred"]); err != nil {
		t.Fatal(err)
	}
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	frames := readFrames(t, conn, 1)
	checkRecord(t, frames[0], OutcomeSuccess)
	if !strings.Contains(frames[0], `csd-code="110104"`) {
		t.Fatal("not the transferred message")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v", err)
	}
}

func TestSyslogUDP(t *testing.T) {
	server, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	logger := &SyslogLogger{Network: SyslogUDP, Address: server.LocalAddr().String(), AppName: "twin", Hostname: "TESTHOST"}
	if err := logger.Log(atnaMessages()["security alert"]); err != nil {
		t.Fatal(err)
	}
	if err := logger.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, MaxSyslogUDPMessage)
	_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, _, err := server.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	checkRecord(t, string(buf[:n]), OutcomeSerious)
	_ = logger.Close()
}

func TestATNASinkMapsDIMSEEvents(t *testing.T) {
	sink := ATNASink{Source: Source{ID: "TWIN"}}
	base := Event{Time: atnaTime, LocalAETitle: "TWIN", RemoteAETitle: "MODALITY", RemoteAddr: "192.0.2.7:104", SOPClassUID: "1.2.840.10008.5.1.4.1.2.2.1"}
	tests := []struct {
		kind    EventKind
		service string
		want    string
		outcome string
	}{
		{OperationSucceeded, "C-STORE", "110104", OutcomeSuccess},
		{OperationFailed, "C-STORE", "110104", OutcomeMinorFailure},
		{OperationSucceeded, "C-FIND", "110112", OutcomeSuccess},
		{OperationSucceeded, "C-MOVE", "110102", OutcomeSuccess},
		{AssociationRejected, "", "110113", OutcomeMinorFailure},
		{OperationSucceeded, "C-ECHO", "", ""},
		{CommandReceived, "C-STORE", "", ""},
	}
	for _, tt := range tests {
		event := base
		event.Kind, event.Service = tt.kind, tt.service
		message, ok := sink.Message(event)
		if ok != (tt.want != "") {
			t.Fatalf("%s %s audited = %v", tt.kind, tt.service, ok)
		}
		if !ok {
			continue
		}
		if message.Event.ID.Value != tt.want || message.Event.OutcomeIndicator != tt.outcome {
			t.Fatalf("%s %s = %s/%s", tt.kind, tt.service, message.Event.ID.Value, message.Event.OutcomeIndicator)
		}
		if _, err := message.Marshal(); err != nil {
			t.Fatalf("%s %s: %v", tt.kind, tt.service, err)
		}
	}
	logger := &SyslogLogger{Network: SyslogTCP, Address: "127.0.0.1:1"}
	event := base
	event.Kind, event.Service = OperationSucceeded, "C-STORE"
	ATNASink{Logger: logger, Source: Source{ID: "TWIN"}}.EmitAuditEvent(context.Background(), event)
	if logger.Stats().Spooled != 1 {
		t.Fatal("sink did not spool the message")
	}
}
