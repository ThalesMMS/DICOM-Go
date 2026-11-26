package ul

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/net/telemetry"
)

func transportTestProxy(t *testing.T, serve func(net.Conn, *bufio.Reader, *http.Request) error) (string, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	done := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		reader := bufio.NewReader(c)
		request, err := http.ReadRequest(reader)
		if err == nil {
			err = serve(c, reader, request)
		}
		done <- err
	}()
	return ln.Addr().String(), done
}
func waitTransportProxy(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("proxy did not finish")
	}
}

func TestTransportCustomDialContextAndPreflight(t *testing.T) {
	address, done := acceptOnce(t, AcceptOptions{AETitle: "ARCHIVE", RequireMatchingCalledAE: true, AcceptAnyAbstractSyntax: true, SupportedTransferSyntaxes: []string{ImplicitVRLittleEndian}})
	type key struct{}
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), key{}, "owned"), 3*time.Second)
	defer cancel()
	calls := 0
	dial := func(received context.Context, network, destination string) (net.Conn, error) {
		calls++
		if received.Value(key{}) != "owned" || network != "tcp" || destination != address {
			return nil, errors.New("wrong dial contract")
		}
		return (&net.Dialer{}).DialContext(received, network, destination)
	}
	opts := DialOptions{Transport: TransportOptions{DialContext: dial}, CalledAETitle: "ARCHIVE", CallingAETitle: "SCU", Contexts: []PresentationContext{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}}}}
	a, err := DialContext(ctx, address, opts)
	if err != nil {
		t.Fatal(err)
	}
	_ = a.Close()
	waitTransportProxy(t, done)
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
	for _, invalid := range []DialOptions{
		{Transport: TransportOptions{DialContext: dial, LocalAddress: &net.TCPAddr{IP: net.ParseIP("127.0.0.1")}}},
		{Transport: TransportOptions{DialContext: dial}, SocketBuffers: SocketBuffers{Send: 4096}},
	} {
		invalid.Contexts = opts.Contexts
		_, err := DialContext(ctx, address, invalid)
		if !errors.Is(err, ErrTransportConfiguration) {
			t.Fatalf("preflight=%v", err)
		}
	}
	if calls != 1 {
		t.Fatal("invalid policy dialed")
	}
	sentinel := errors.New("caller dial refused")
	_, err = TransportOptions{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }}.dial(canceledTransportContext(), address, SocketBuffers{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	_, err = TransportOptions{DialContext: func(context.Context, string, string) (net.Conn, error) { return nil, sentinel }}.dial(context.Background(), address, SocketBuffers{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("dial error=%v", err)
	}
	client, peer := net.Pipe()
	defer peer.Close()
	tracked := &transportTrackedConn{Conn: client}
	_, err = TransportOptions{DialContext: func(context.Context, string, string) (net.Conn, error) { return tracked, sentinel }}.dial(context.Background(), address, SocketBuffers{})
	if !errors.Is(err, sentinel) || tracked.closed.Load() != 1 {
		t.Fatal("error plus connection did not close owned stream")
	}
	_, err = TransportOptions{DialContext: func(context.Context, string, string) (net.Conn, error) { return nil, nil }}.dial(context.Background(), address, SocketBuffers{})
	if !errors.Is(err, ErrTransportConfiguration) {
		t.Fatal("nil successful stream accepted")
	}

}
func canceledTransportContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestTransportHTTPConnectResponseBoundsAuthAndBufferedBytes(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		limit          int
		status         int
		want           error
		payload        string
	}{
		{name: "accepted", response: "HTTP/1.1 200 Connected\r\nContent-Length: 900000\r\n\r\nXYZ", payload: "XYZ"},
		{name: "any2xx", response: "HTTP/1.1 201 Created\r\n\r\nXYZ", payload: "XYZ"},
		{name: "refused", response: "HTTP/1.1 407 secret-in-reason\r\nX-Secret: password\r\n\r\n", status: 407, want: ErrHTTPConnect},
		{name: "malformed", response: "garbage password\r\n\r\n", want: ErrHTTPConnect},
		{name: "earlyclose", response: "HTTP/1.1 200 Connected\r\n", want: ErrHTTPConnect},
		{name: "oversized", response: "HTTP/1.1 200 Connected\r\nX: " + strings.Repeat("a", 600) + "\r\n\r\n", limit: 256, want: ErrHTTPConnectHeaderLimit},
		{name: "longaccepted", response: "HTTP/1.1 200 Connected\r\nX: " + strings.Repeat("a", 4093) + "\r\n\r\nXYZ", payload: "XYZ"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy, done := transportTestProxy(t, func(c net.Conn, _ *bufio.Reader, r *http.Request) error {
				if r.Method != "CONNECT" || r.Host != "dicom.test:104" || r.RequestURI != "dicom.test:104" || r.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("operator:password")) {
					return errors.New("bad CONNECT authority/authentication")
				}
				_, err := io.WriteString(c, tc.response)
				return err
			})
			o := TransportOptions{HTTPConnect: &HTTPConnectOptions{Address: proxy, Username: "operator", Password: "password", MaxResponseHeaderBytes: tc.limit}}
			c, err := o.dial(context.Background(), "dicom.test:104", SocketBuffers{})
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("error=%v", err)
				}
				if strings.Contains(err.Error(), "password") || strings.Contains(err.Error(), "secret-in-reason") {
					t.Fatal("proxy text leaked")
				}
				if c != nil {
					t.Fatal("failed tunnel escaped")
				}
				var rejected *HTTPConnectError
				if tc.status != 0 && (!errors.As(err, &rejected) || rejected.StatusCode != tc.status) {
					t.Fatal("missing typed status")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				b := make([]byte, len(tc.payload))
				if _, err := io.ReadFull(c, b); err != nil || string(b) != tc.payload {
					t.Fatalf("buffered=%q %v", b, err)
				}
			}
			waitTransportProxy(t, done)
		})
	}
	for _, p := range []HTTPConnectOptions{{Address: "http://user:password@proxy:80"}, {Address: "proxy:80\r\nX: password"}, {Address: "proxy:80", Username: "bad:password"}, {Address: "proxy:80", Password: "password\r\n"}, {Address: "proxy:80", MaxResponseHeaderBytes: 1}, {Address: "proxy:80", Timeout: -1}} {
		if err := (TransportOptions{HTTPConnect: &p}).Validate(); !errors.Is(err, ErrTransportConfiguration) || strings.Contains(err.Error(), "password") {
			t.Fatalf("validation=%v", err)
		}
	}
}

func TestTransportSourceBindingAndSocketBuffers(t *testing.T) {
	source := make(chan string, 1)
	proxy, done := transportTestProxy(t, func(c net.Conn, _ *bufio.Reader, _ *http.Request) error {
		host, _, _ := net.SplitHostPort(c.RemoteAddr().String())
		source <- host
		_, err := io.WriteString(c, "HTTP/1.1 200 Connected\r\n\r\n")
		return err
	})
	o := TransportOptions{LocalAddress: &net.TCPAddr{IP: net.ParseIP("127.0.0.1")}, HTTPConnect: &HTTPConnectOptions{Address: proxy}}
	c, err := o.dial(context.Background(), "destination.test:104", SocketBuffers{Send: 32768, Receive: 32768})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := <-source; got != "127.0.0.1" {
		t.Fatalf("source=%q", got)
	}
	tunnel := c.(*bufferedTunnel)
	sizes, err := EffectiveSocketBuffers(tunnel.Conn)
	if err != nil || sizes.Send <= 0 || sizes.Receive <= 0 {
		t.Fatalf("buffers=%+v %v", sizes, err)
	}
	if _, err := EffectiveSocketBuffers(c); !errors.Is(err, ErrSocketBuffersUnsupported) {
		t.Fatalf("wrapper buffers=%v", err)
	}
	waitTransportProxy(t, done)
}

type transportTrackedConn struct {
	net.Conn
	closed atomic.Int32
}

func (c *transportTrackedConn) Close() error { c.closed.Add(1); return c.Conn.Close() }

type connectTimeoutConn struct {
	net.Conn
	writeTimeout bool
}

func (c connectTimeoutConn) Read([]byte) (int, error) {
	return 0, fmt.Errorf("synthetic proxy credential: %w", os.ErrDeadlineExceeded)
}

func (c connectTimeoutConn) Write(p []byte) (int, error) {
	if c.writeTimeout {
		return 0, fmt.Errorf("synthetic proxy credential: %w", os.ErrDeadlineExceeded)
	}
	return len(p), nil
}

func TestHTTPConnectSocketTimeoutBeforeContextDeadline(t *testing.T) {
	for _, tc := range []struct {
		name         string
		writeTimeout bool
	}{
		{name: "read"},
		{name: "write", writeTimeout: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, peer := net.Pipe()
			defer peer.Close()
			tracked := &transportTrackedConn{Conn: connectTimeoutConn{Conn: client, writeTimeout: tc.writeTimeout}}
			opts := TransportOptions{
				DialContext: func(context.Context, string, string) (net.Conn, error) { return tracked, nil },
				HTTPConnect: &HTTPConnectOptions{Address: "proxy.test:80"},
			}

			tunnel, err := opts.dial(context.Background(), "destination.test:104", SocketBuffers{})
			if tunnel != nil || !errors.Is(err, ErrAssociationTimeout) {
				t.Fatalf("CONNECT tunnel = %v, error = %v, want association timeout", tunnel, err)
			}
			if strings.Contains(err.Error(), "synthetic proxy credential") {
				t.Fatalf("CONNECT error disclosed transport details: %v", err)
			}
			if tracked.closed.Load() != 1 {
				t.Fatal("CONNECT timeout did not close the stream exactly once")
			}
		})
	}
}

func TestTransportCancellationConnectAndTLSClosesStreams(t *testing.T) {
	for _, phase := range []string{"CONNECT-cancel", "CONNECT-timeout", "TLS-cancel", "TLS-timeout"} {
		t.Run(phase, func(t *testing.T) {
			client, server := net.Pipe()
			tracked := &transportTrackedConn{Conn: client}
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := context.Canceled
			if strings.HasSuffix(phase, "timeout") {
				ctx, cancel = context.WithTimeout(ctx, 40*time.Millisecond)
				defer cancel()
				want = ErrAssociationTimeout
			}
			entered := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				r, err := http.ReadRequest(bufio.NewReader(server))
				_ = r
				if err != nil {
					done <- err
					return
				}
				if strings.HasPrefix(phase, "TLS") {
					if _, err = io.WriteString(server, "HTTP/1.1 200 Connected\r\n\r\n"); err != nil {
						done <- err
						return
					}
				}
				if strings.HasPrefix(phase, "TLS") {
					var first [1]byte
					_, err = io.ReadFull(server, first[:])
					if err != nil {
						done <- err
						return
					}
				}
				close(entered)
				_, err = io.Copy(io.Discard, server)
				done <- err
			}()
			go func() {
				select {
				case <-entered:
					if strings.HasSuffix(phase, "cancel") {
						cancel()
					}
				case <-ctx.Done():
				}
			}()
			options := DialOptions{Context: ctx, ARTIMTimeout: -1, Transport: TransportOptions{DialContext: func(context.Context, string, string) (net.Conn, error) { return tracked, nil }, HTTPConnect: &HTTPConnectOptions{Address: "proxy.test:80"}}, Contexts: []PresentationContext{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}}}}
			if strings.HasPrefix(phase, "TLS") {
				options.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
			}
			a, err := Dial("destination.test:104", options)
			if a != nil || !errors.Is(err, want) {
				t.Fatalf("phase error = %v, want %v", err, want)
			}
			if tracked.closed.Load() == 0 {
				t.Fatal("failed phase retained socket")
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("peer/socket not drained")
			}
		})
	}
}

func TestTransportTLSValidatesLogicalDestinationThroughConnect(t *testing.T) {
	fixture := newTLSFixture(t)
	for _, name := range []string{"scp.test", "wrong.test"} {
		t.Run(name, func(t *testing.T) {
			observed := make(chan string, 1)
			events := make(chan telemetry.Event, 32)
			proxy, done := transportTestProxy(t, func(c net.Conn, _ *bufio.Reader, r *http.Request) error {
				observed <- r.Host
				if _, err := io.WriteString(c, "HTTP/1.1 200 Connected\r\n\r\n"); err != nil {
					return err
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				a, err := Accept(c, AcceptOptions{Context: ctx, AETitle: "ARCHIVE", RequireMatchingCalledAE: true, AcceptAnyAbstractSyntax: true, SupportedTransferSyntaxes: []string{ImplicitVRLittleEndian}, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{fixture.server.Certificate}}})
				if a != nil {
					_ = a.Close()
				}
				if name == "wrong.test" && err != nil {
					return nil
				}
				return err
			})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			a, err := DialContext(ctx, name+":11112", DialOptions{Observability: &ObservabilityOptions{EventQueueDepth: 32, EndpointPolicy: telemetry.EndpointPolicy{Addresses: telemetry.EndpointPlaintext, AETitles: telemetry.EndpointPlaintext}, Sink: telemetry.SinkFunc(func(_ context.Context, e telemetry.Event) { events <- e })}, CalledAETitle: "ARCHIVE", CallingAETitle: "SCU", ARTIMTimeout: -1, Transport: TransportOptions{HTTPConnect: &HTTPConnectOptions{Address: proxy}}, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: fixture.pki.CAPool}, Contexts: []PresentationContext{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}}}})
			if name == "scp.test" {
				if err != nil {
					t.Fatal(err)
				}
				_ = a.Close()
				found := false
				for !found {
					select {
					case e := <-events:
						if e.Kind == telemetry.AssociationEstablished {
							found = true
							if e.RemoteAddress != "scp.test:11112" || e.RemoteAETitle != "ARCHIVE" {
								t.Fatalf("logical telemetry=%+v", e)
							}
						}
					case <-ctx.Done():
						t.Fatal("missing established telemetry")
					}
				}
			} else if err == nil {
				if a != nil {
					_ = a.Close()
				}
				t.Fatal("wrong destination certificate accepted")
			}
			if target := <-observed; target != name+":11112" {
				t.Fatalf("CONNECT target=%s", target)
			}
			waitTransportProxy(t, done)
		})
	}
}
