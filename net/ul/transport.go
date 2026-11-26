package ul

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DialContextFunc establishes one stream. It must honor context cancellation
// and return a usable, exclusively owned connection or an error. The caller is
// responsible for concurrency safety and pre-connect socket options.
type DialContextFunc func(context.Context, string, string) (net.Conn, error)

// TransportOptions configures requestor-side transport, independently of the
// DICOM destination and TLS policy. Never discovers proxies from the environment.
type TransportOptions struct {
	DialContext  DialContextFunc
	LocalAddress *net.TCPAddr
	HTTPConnect  *HTTPConnectOptions
}

// HTTPConnectOptions explicitly selects a plain HTTP/1.1 CONNECT proxy. Basic
// credentials are sent only to that proxy. They are not encrypted on this first
// hop; use a trusted protected network. Destination TLS, if configured on Dial,
// starts AFTER CONNECT and validates the DICOM destination, not the proxy.
type HTTPConnectOptions struct {
	Address  string
	Username string
	Password string
	// Timeout bounds dial plus CONNECT; zero is 30 seconds, negative is invalid.
	Timeout time.Duration
	// MaxResponseHeaderBytes includes status line and CRLFs; zero is 32 KiB.
	// Valid nonzero values are 256..1 MiB. Bodies of rejected responses are not read.
	MaxResponseHeaderBytes int
}

var (
	ErrTransportConfiguration = errors.New("dicom ul: invalid transport configuration")
	ErrHTTPConnect            = errors.New("dicom ul: HTTP CONNECT failed")
	ErrHTTPConnectHeaderLimit = errors.New("dicom ul: HTTP CONNECT response headers exceed limit")
)

// HTTPConnectError exposes only the status code, never proxy reason phrases,
// headers, bodies, URL userinfo or authentication credentials.
type HTTPConnectError struct{ StatusCode int }

func (e *HTTPConnectError) Error() string {
	return fmt.Sprintf("%s (status %d)", ErrHTTPConnect, e.StatusCode)
}
func (e *HTTPConnectError) Unwrap() error { return ErrHTTPConnect }

// Clone freezes option values and local address bytes. The function itself is
// caller-owned and cannot be cloned; closures must obey their own lifetime policy.
func (o TransportOptions) Clone() TransportOptions {
	if o.LocalAddress != nil {
		a := *o.LocalAddress
		a.IP = append(net.IP(nil), a.IP...)
		o.LocalAddress = &a
	}
	if o.HTTPConnect != nil {
		p := *o.HTTPConnect
		o.HTTPConnect = &p
	}
	return o
}

// Validate checks finite policy before opening a socket. Custom dialers own
// binding and pre-connect buffers: combining them with these UL fields refuses
// explicitly instead of silently ignoring either policy.
func (o TransportOptions) Validate() error { return o.validate(SocketBuffers{}) }
func (o TransportOptions) validate(buffers SocketBuffers) error {
	if o.DialContext != nil && (o.LocalAddress != nil || !buffers.IsZero()) {
		return fmt.Errorf("%w: custom dialer owns local binding and socket buffers", ErrTransportConfiguration)
	}
	if a := o.LocalAddress; a != nil {
		if a.Port < 0 || a.Port > 65535 || len(a.IP) != 0 && a.IP.To16() == nil || strings.ContainsAny(a.Zone, "\r\n") {
			return ErrTransportConfiguration
		}
	}
	if p := o.HTTPConnect; p != nil {
		if !validTransportAuthority(p.Address) || p.Timeout < 0 || p.MaxResponseHeaderBytes != 0 && (p.MaxResponseHeaderBytes < 256 || p.MaxResponseHeaderBytes > 1<<20) || len(p.Username)+len(p.Password) > 8192 || strings.ContainsAny(p.Username, ":\r\n") || strings.ContainsAny(p.Password, "\r\n") {
			return ErrTransportConfiguration
		}
	}
	return nil
}
func validTransportAuthority(address string) bool {
	if len(address) > 1024 || strings.ContainsAny(address, " \t\r\n/?#@") {
		return false
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n > 0 && n <= 65535
}

func (o TransportOptions) dial(ctx context.Context, address string, buffers SocketBuffers) (net.Conn, error) {
	o = o.Clone()
	if err := o.validate(buffers); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	destination := address
	if o.HTTPConnect != nil {
		if !validTransportAuthority(address) {
			return nil, ErrTransportConfiguration
		}
		destination = o.HTTPConnect.Address
		timeout := o.HTTPConnect.Timeout
		if timeout == 0 {
			timeout = 30 * time.Second
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var conn net.Conn
	var err error
	if o.DialContext != nil {
		conn, err = o.DialContext(ctx, "tcp", destination)
	} else {
		d := &net.Dialer{LocalAddr: o.LocalAddress}
		if !buffers.IsZero() {
			d.Control = buffers.control
		}
		conn, err = retryUnavailableSourcePort(ctx, o.LocalAddress, destination, func(ctx context.Context) (net.Conn, error) {
			return d.DialContext(ctx, "tcp", destination)
		})
	}
	if err != nil {
		if conn != nil {
			_ = conn.Close()
		}
		return nil, associationContextError(ctx, err)
	}
	if conn == nil {
		return nil, fmt.Errorf("%w: dialer returned no connection", ErrTransportConfiguration)
	}
	if err := ctx.Err(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if o.HTTPConnect == nil {
		return conn, nil
	}
	tunnel, err := establishHTTPConnect(ctx, conn, address, *o.HTTPConnect)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return tunnel, nil
}

// bufferedTunnel retains bytes prefetched beyond the final response headers.
// RemoteAddr stays the physical socket address; telemetry uses the logical
// destination separately. Deadlines, Close and writes reach the underlying socket.
type bufferedTunnel struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedTunnel) Read(p []byte) (int, error) { return c.reader.Read(p) }

func establishHTTPConnect(ctx context.Context, conn net.Conn, address string, p HTTPConnectOptions) (net.Conn, error) {
	max := p.MaxResponseHeaderBytes
	if max == 0 {
		max = 32 << 10
	}
	request := "CONNECT " + address + " HTTP/1.1\r\nHost: " + address + "\r\n"
	if p.Username != "" || p.Password != "" {
		request += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(p.Username+":"+p.Password)) + "\r\n"
	}
	request += "\r\n"
	reader := bufio.NewReaderSize(conn, 4096)
	err := withConnDeadline(ctx, conn.SetDeadline, func() error {
		if _, err := io.Copy(conn, strings.NewReader(request)); err != nil {
			return httpConnectStreamError(err, "write request")
		}
		// Consume only a finite header section, not a rejected response body. Reading
		// headers separately prevents net/http from allocating an unbounded field.
		var header bytes.Buffer
		lineStart := 0
		for {
			line, err := reader.ReadSlice('\n')
			if header.Len()+len(line) > max {
				return ErrHTTPConnectHeaderLimit
			}
			header.Write(line)
			if err == bufio.ErrBufferFull {
				continue
			}
			if err != nil {
				return httpConnectStreamError(err, "incomplete response")
			}
			if header.Len() < 2 || header.Bytes()[header.Len()-2] != '\r' {
				return fmt.Errorf("%w: malformed response", ErrHTTPConnect)
			}
			if header.Len()-lineStart == 2 {
				break
			}
			lineStart = header.Len()
		}
		response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(header.Bytes())), &http.Request{Method: "CONNECT"})
		if err != nil || response.ProtoMajor != 1 || response.ProtoMinor > 1 {
			return fmt.Errorf("%w: malformed response", ErrHTTPConnect)
		}
		// No body is consulted: RFC 9110 9.3.6 switches any 2xx to tunnel mode
		// immediately and forbids applying Content-Length/Transfer-Encoding to it.
		if response.StatusCode < 200 || response.StatusCode > 299 {
			return &HTTPConnectError{StatusCode: response.StatusCode}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &bufferedTunnel{Conn: conn, reader: reader}, nil
}

func httpConnectStreamError(err error, detail string) error {
	// The socket deadline can elapse before the context reports its deadline.
	// Preserve the classification without exposing proxy or dialer details.
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return ErrAssociationTimeout
	}
	return fmt.Errorf("%w: %s", ErrHTTPConnect, detail)
}
