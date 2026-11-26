package mllp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/hl7"
	"github.com/ThalesMMS/dicom-go/internal/testutil"
)

// This peer's byte framing is independently authored. It never calls the
// implementation's Reader, AppendFrame, WriteFrame or confirmation helpers.
func rawFrameBytes(p []byte) []byte {
	return append(append([]byte{0x0b}, p...), 0x1c, 0x0d)
}
func rawReadFrame(r *bufio.Reader) ([]byte, error) {
	b, err := r.ReadByte()
	if err != nil || b != 0x0b {
		return nil, fmt.Errorf("peer expected frame start: %w", err)
	}
	var p []byte
	for len(p) < 4<<20 {
		b, err = r.ReadByte()
		if err != nil {
			return nil, err
		}
		if b == 0x1c {
			b, err = r.ReadByte()
			if err != nil || b != 0x0d {
				return nil, errors.New("peer expected frame trailer")
			}
			return p, nil
		}
		p = append(p, b)
	}
	return nil, errors.New("peer frame ceiling")
}
func rawWrite(c net.Conn, wire []byte) error {
	for len(wire) > 0 {
		n, err := c.Write(wire)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		wire = wire[n:]
	}
	return nil
}
func rawApplicationACK(code, id string) []byte {
	return []byte(`MSH|^~\&|PEER|LAB|GO|RAD|20261009||ACK^A08^ACK|PEER-ACK|P|2.5.1` + "\rMSA|" + code + "|" + id + "\r")
}
func expectRawACK(r *bufio.Reader) error {
	p, err := rawReadFrame(r)
	if err != nil || !bytes.Equal(p, []byte{0x06}) {
		return errors.New("peer did not receive transport ACK")
	}
	return nil
}
func startRawPeer(t *testing.T, fn func(net.Conn, *bufio.Reader) error) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var peerErr error
	go func() {
		defer close(done)
		c, err := listener.Accept()
		if err != nil {
			peerErr = err
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		peerErr = fn(c, bufio.NewReader(c))
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-done:
			if peerErr != nil {
				t.Errorf("independent peer: %v", peerErr)
			}
		case <-time.After(4 * time.Second):
			t.Error("independent peer not reaped")
		}
	})
	return listener.Addr().String()
}
func testMessage(t *testing.T, id string) *hl7.Message {
	t.Helper()
	m, err := hl7.Parse(adt(id, "SYNTHETIC-MRN"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMLLPReleasesWithIndependentPeer(t *testing.T) {
	for _, release := range []Release{MLLP1, MLLP2} {
		for _, code := range []hl7.AckCode{hl7.AckAccept, hl7.AckError, hl7.AckReject} {
			t.Run(fmt.Sprintf("release%d/%s", release+1, code), func(t *testing.T) {
				address := startRawPeer(t, func(c net.Conn, r *bufio.Reader) error {
					for i := 0; i < 2; i++ {
						if _, err := rawReadFrame(r); err != nil {
							return err
						}
						wire := rawFrameBytes(rawApplicationACK(string(code), fmt.Sprint(i)))
						if release == MLLP2 {
							wire = append(rawFrameBytes([]byte{0x06}), wire...)
						}
						// Coalesced confirmation and application frames, fragmented
						// across odd boundaries, must share the same buffered reader.
						for len(wire) > 0 {
							n := min(7, len(wire))
							if err := rawWrite(c, wire[:n]); err != nil {
								return err
							}
							wire = wire[n:]
						}
						if release == MLLP2 {
							if err := expectRawACK(r); err != nil {
								return err
							}
						}
					}
					return nil
				})
				c, err := Dial(context.Background(), address, ClientOptions{Release: release, ResponseTimeout: time.Second})
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				for i := 0; i < 2; i++ {
					response, err := c.Send(context.Background(), testMessage(t, fmt.Sprint(i)))
					if err != nil || ackOf(t, response).Code != code {
						t.Fatalf("transport/application distinction: %v", err)
					}
				}
			})
		}
	}
}

func TestMLLP2ClientNegativeTransportOutcomes(t *testing.T) {
	tests := []struct {
		name  string
		wire  []byte
		phase ExchangePhase
		state RequestTransportState
		cause error
		wait  bool
	}{
		{"NAK", rawFrameBytes([]byte{0x15}), PhaseRequestConfirmation, RequestRejected, ErrTransportNAK, false},
		{"unframed ACK", []byte{0x06}, PhaseRequestConfirmation, RequestUnknown, ErrTransportConfirmation, false},
		{"two-byte confirmation", rawFrameBytes([]byte{0x06, 0x06}), PhaseRequestConfirmation, RequestUnknown, ErrTransportConfirmation, false},
		{"invalid confirmation", rawFrameBytes([]byte{'X'}), PhaseRequestConfirmation, RequestUnknown, ErrTransportConfirmation, false},
		{"application before confirmation", rawFrameBytes(rawApplicationACK("AA", "N")), PhaseRequestConfirmation, RequestUnknown, ErrTransportConfirmation, false},
		{"closed before ACK", nil, PhaseRequestConfirmation, RequestUnknown, io.EOF, false},
		{"truncated ACK", []byte{0x0b, 0x06, 0x1c}, PhaseRequestConfirmation, RequestUnknown, io.ErrUnexpectedEOF, false},
		{"ACK timeout", nil, PhaseRequestConfirmation, RequestUnknown, nil, true},
		{"closed after ACK", rawFrameBytes([]byte{0x06}), PhaseReadResponse, RequestAcknowledged, io.EOF, false},
		{"response timeout", rawFrameBytes([]byte{0x06}), PhaseReadResponse, RequestAcknowledged, nil, true},
		{"bad response frame", append(rawFrameBytes([]byte{0x06}), []byte{0x0b, 'X', 0x1c, 'X'}...), PhaseReadResponse, RequestAcknowledged, ErrFraming, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			address := startRawPeer(t, func(c net.Conn, r *bufio.Reader) error {
				if _, err := rawReadFrame(r); err != nil {
					return err
				}
				if len(tc.wire) > 0 {
					if err := rawWrite(c, tc.wire); err != nil {
						return err
					}
				}
				if tc.wait {
					_, _ = io.Copy(io.Discard, c) // client deadline closes; no sleep/replay
				}
				return nil
			})
			c, err := Dial(context.Background(), address, ClientOptions{Release: MLLP2, ResponseTimeout: 150 * time.Millisecond, TransportACKTimeout: 40 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			_, err = c.Send(context.Background(), testMessage(t, "N"))
			var exchange *ExchangeError
			if !errors.As(err, &exchange) || exchange.Phase != tc.phase || exchange.RequestState != tc.state || (tc.cause != nil && !errors.Is(err, tc.cause)) {
				t.Fatalf("typed outcome: %v %#v", err, exchange)
			}
			if tc.wait {
				var timeout net.Error
				if !errors.As(err, &timeout) || !timeout.Timeout() {
					t.Fatalf("deadline cause: %v", err)
				}
			}
			if strings.Contains(err.Error(), secretName) {
				t.Fatal("message content in diagnostic")
			}
			if _, again := c.Send(context.Background(), testMessage(t, "N")); again != err {
				t.Fatalf("broken connection attempted another exchange: %v", again)
			}
		})
	}
}

func TestMLLP2ApplicationFailureRemainsDistinct(t *testing.T) {
	for _, kind := range []string{"parse", "correlate"} {
		t.Run(kind, func(t *testing.T) {
			address := startRawPeer(t, func(c net.Conn, r *bufio.Reader) error {
				for i := 0; i < 2; i++ {
					if _, err := rawReadFrame(r); err != nil {
						return err
					}
					id := "A"
					if i == 1 {
						id = "B"
					}
					app := rawApplicationACK("AA", id)
					if i == 0 {
						if kind == "parse" {
							app = []byte("PID|synthetic\r")
						} else {
							app = rawApplicationACK("AA", "OTHER")
						}
					}
					if err := rawWrite(c, append(rawFrameBytes([]byte{0x06}), rawFrameBytes(app)...)); err != nil {
						return err
					}
					if err := expectRawACK(r); err != nil {
						return err
					}
				}
				return nil
			})
			c, err := Dial(context.Background(), address, ClientOptions{Release: MLLP2, ResponseTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			response, err := c.Send(context.Background(), testMessage(t, "A"))
			var exchange *ExchangeError
			if !errors.As(err, &exchange) || exchange.RequestState != RequestAcknowledged {
				t.Fatalf("application failure without transport knowledge: %v", err)
			}
			if kind == "parse" && exchange.Phase != PhaseParseResponse || kind == "correlate" && (!errors.Is(err, ErrResponseMismatch) || response == nil || exchange.Phase != PhaseCorrelateResponse) {
				t.Fatalf("application phase: %v", err)
			}
			// The user explicitly sends a new operation; the implementation never
			// retries the failed application's message on its own.
			if response, err := c.Send(context.Background(), testMessage(t, "B")); err != nil || ackOf(t, response).Code != hl7.AckAccept {
				t.Fatalf("aligned connection unusable after application failure: %v", err)
			}
		})
	}
}

func TestMLLP2ServerWithIndependentClient(t *testing.T) {
	address, _, _ := startServer(t, ServerOptions{Release: MLLP2, TransportACKTimeout: time.Second})
	c, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	r := bufio.NewReader(c)
	for i := 0; i < 2; i++ {
		if err := rawWrite(c, rawFrameBytes(adt(fmt.Sprint(i), "MRN"))); err != nil {
			t.Fatal(err)
		}
		if err := expectRawACK(r); err != nil {
			t.Fatal(err)
		}
		app, err := rawReadFrame(r)
		if err != nil || !bytes.Contains(app, []byte("MSA|AA|"+fmt.Sprint(i))) {
			t.Fatalf("application ACK: %v", err)
		}
		if err := rawWrite(c, rawFrameBytes([]byte{0x06})); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMLLP2ServerRejectsAndWaitsForConfirmation(t *testing.T) {
	for _, kind := range []string{"NAK", "malformed", "timeout", "closed", "oversized request", "unframed request", "unparseable body"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			address, log, _ := startServer(t, ServerOptions{Release: MLLP2, MaxFrameBytes: 1024, TransportACKTimeout: 40 * time.Millisecond, Handler: HandlerFunc(func(_ context.Context, r *Request) *hl7.Message {
				calls.Add(1)
				return hl7.NewACK(r.Message, hl7.AckError, hl7.AckOptions{})
			})})
			c, err := net.Dial("tcp", address)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(time.Second))
			r := bufio.NewReader(c)
			wire := rawFrameBytes(adt("S", "MRN"))
			switch kind {
			case "oversized request":
				wire = rawFrameBytes(bytes.Repeat([]byte{'x'}, 2048))
			case "unframed request":
				wire = []byte{'X'}
			case "unparseable body":
				wire = rawFrameBytes([]byte("MSH|^~\\&|PEER|LAB|GO|RAD|20261009||ADT^A08|S|P|2.5\rpid|bad\r"))
			}
			if err := rawWrite(c, wire); err != nil {
				t.Fatal(err)
			}
			confirmation, err := rawReadFrame(r)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "oversized request" || kind == "unframed request" {
				if !bytes.Equal(confirmation, []byte{0x15}) || calls.Load() != 0 {
					t.Fatal("refused frame was ACKed or delivered")
				}
			} else {
				if !bytes.Equal(confirmation, []byte{0x06}) {
					t.Fatal("complete frame not transport ACKed")
				}
				app, err := rawReadFrame(r)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "unparseable body" {
					if !bytes.Contains(app, []byte("MSA|AR|S")) || calls.Load() != 0 {
						t.Fatal("transport admission was confused with HL7 acceptance")
					}
					_ = rawWrite(c, rawFrameBytes([]byte{0x06}))
					return
				}
				if !bytes.Contains(app, []byte("MSA|AE|S")) || calls.Load() != 1 {
					t.Fatal("application rejection not kept distinct")
				}
				switch kind {
				case "NAK":
					_ = rawWrite(c, rawFrameBytes([]byte{0x15}))
				case "malformed":
					_ = rawWrite(c, rawFrameBytes([]byte{0x06, 0x06}))
				case "closed":
					_ = c.(*net.TCPConn).CloseWrite()
				}
			}
			if _, err := r.ReadByte(); err == nil {
				t.Fatal("refused/timed out connection remained active")
			}
			// Receiving EOF establishes that serveConn already reported and closed.
			if len(log.all()) != 1 {
				t.Fatalf("expected one connection failure, got %v", log.all())
			}
		})
	}
}

func TestMLLP2CancellationAndLimits(t *testing.T) {
	t.Run("queued cancellation", func(t *testing.T) {
		entered := make(chan struct{})
		address := startRawPeer(t, func(c net.Conn, r *bufio.Reader) error {
			if _, err := rawReadFrame(r); err != nil {
				return err
			}
			close(entered)
			_, _ = io.Copy(io.Discard, c)
			return nil
		})
		c, err := Dial(context.Background(), address, ClientOptions{Release: MLLP2, ResponseTimeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		active, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { _, err := c.Send(active, testMessage(t, "Q")); done <- err }()
		<-entered
		queued, cancelQueued := context.WithCancel(context.Background())
		cancelQueued()
		if _, err := c.Send(queued, testMessage(t, "Q2")); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		} else {
			var exchange *ExchangeError
			if !errors.As(err, &exchange) || exchange.RequestState != RequestNotSent {
				t.Fatalf("queued cancellation lost no-send result: %v", err)
			}
		}
		cancel()
		select {
		case err := <-done:
			var exchange *ExchangeError
			if !errors.Is(err, context.Canceled) || !errors.As(err, &exchange) || exchange.RequestState != RequestUnknown {
				t.Fatalf("active cancellation outcome: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("active exchange ignored cancellation")
		}
	})
	t.Run("local size admission", func(t *testing.T) {
		client, peer := net.Pipe()
		defer peer.Close()
		c := NewClient(client, ClientOptions{Release: MLLP2, MaxFrameBytes: 1})
		defer c.Close()
		_, err := c.Send(context.Background(), testMessage(t, "L"))
		var exchange *ExchangeError
		if !errors.Is(err, ErrFrameTooLarge) || !errors.As(err, &exchange) || exchange.RequestState != RequestNotSent || c.broken != nil {
			t.Fatalf("local admission poisoned or sent: %v", err)
		}
	})
	t.Run("unsupported release", func(t *testing.T) {
		if _, err := Dial(context.Background(), "invalid", ClientOptions{Release: 99}); !errors.Is(err, ErrUnsupportedRelease) {
			t.Fatal(err)
		}
		client, peer := net.Pipe()
		defer peer.Close()
		c := NewClient(client, ClientOptions{Release: 99})
		defer c.Close()
		if _, err := c.Send(context.Background(), testMessage(t, "I")); !errors.Is(err, ErrUnsupportedRelease) {
			t.Fatal(err)
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		if err := Serve(context.Background(), listener, ServerOptions{Release: 99}); !errors.Is(err, ErrUnsupportedRelease) {
			t.Fatal(err)
		}
	})
}

type progressingWriter struct {
	bytes.Buffer
	limit int
	err   error
	zero  bool
}

func (w *progressingWriter) Write(p []byte) (int, error) {
	if w.zero {
		return 0, nil
	}
	n, _ := w.Buffer.Write(p[:min(w.limit, len(p))])
	return n, w.err
}
func TestFrameWriterHonorsPartialProgress(t *testing.T) {
	payload := []byte("MSH|synthetic")
	w := &progressingWriter{limit: 2}
	if err := WriteFrame(w, payload); err != nil || !bytes.Equal(w.Bytes(), rawFrameBytes(payload)) {
		t.Fatalf("short progressing writes lost frame bytes: %v", err)
	}
	w = &progressingWriter{zero: true}
	if err := WriteFrame(w, payload); !errors.Is(err, io.ErrShortWrite) || w.Len() != 0 {
		t.Fatalf("zero progress: %v", err)
	}
	cause := errors.New("synthetic write refusal")
	w = &progressingWriter{limit: 2, err: cause}
	if err := WriteFrame(w, payload); !errors.Is(err, cause) || w.Len() != 2 {
		t.Fatalf("partial refusal: %v", err)
	}
	// No overflow for a large declared ceiling on a small actual frame.
	if p, err := NewReader(bytes.NewReader(rawFrameBytes(payload)), int(^uint(0)>>1)).ReadFrame(); err != nil || !bytes.Equal(p, payload) {
		t.Fatalf("large logical ceiling: %v", err)
	}
}

type faultConnection struct {
	net.Conn
	writeFault    func([]byte) (int, error)
	deadlineFault error
}

func (c *faultConnection) Write(p []byte) (int, error) {
	if c.writeFault != nil {
		return c.writeFault(p)
	}
	return c.Conn.Write(p)
}
func (c *faultConnection) SetDeadline(d time.Time) error {
	if c.deadlineFault != nil {
		return c.deadlineFault
	}
	return c.Conn.SetDeadline(d)
}
func TestMLLP2WriteAndDeadlineFailureKnowledge(t *testing.T) {
	cause := errors.New("synthetic I/O refusal")
	for _, kind := range []string{"deadline", "zero", "partial"} {
		t.Run(kind, func(t *testing.T) {
			conn, peer := net.Pipe()
			defer peer.Close()
			wrapped := &faultConnection{Conn: conn}
			wantState := RequestNotSent
			if kind == "deadline" {
				wrapped.deadlineFault = cause
			} else if kind == "zero" {
				wrapped.writeFault = func([]byte) (int, error) { return 0, nil }
			} else {
				// A synthetic writer reports accepted partial bytes and an error;
				// the sender cannot infer the remote application's outcome.
				wrapped.writeFault = func(p []byte) (int, error) { return min(2, len(p)), cause }
				wantState = RequestUnknown
			}
			c := NewClient(wrapped, ClientOptions{Release: MLLP2})
			defer c.Close()
			_, err := c.Send(context.Background(), testMessage(t, "F"))
			var exchange *ExchangeError
			wantCause := cause
			if kind == "zero" {
				wantCause = io.ErrShortWrite
			}
			if !errors.As(err, &exchange) || exchange.Phase != PhaseWriteRequest || exchange.RequestState != wantState || !errors.Is(err, wantCause) {
				t.Fatalf("write knowledge: %v", err)
			}
		})
	}
}

func TestMLLP2InvalidWriterProgressLeavesUnknownResult(t *testing.T) {
	for _, forwarded := range []bool{false, true} {
		t.Run(fmt.Sprintf("forwarded=%v", forwarded), func(t *testing.T) {
			var requests atomic.Int32
			observed := make(chan struct{})
			address := startRawPeer(t, func(c net.Conn, r *bufio.Reader) error {
				defer close(observed)
				if forwarded {
					if _, err := rawReadFrame(r); err != nil {
						return err
					}
					requests.Add(1)
				}
				_, _ = io.Copy(io.Discard, c)
				return nil
			})
			conn, err := net.Dial("tcp", address)
			if err != nil {
				t.Fatal(err)
			}
			wrapped := &faultConnection{Conn: conn}
			wrapped.writeFault = func(p []byte) (int, error) {
				if !forwarded {
					return -1, nil
				}
				n, err := conn.Write(p)
				if err != nil || n != len(p) {
					return n, err
				}
				return n + 1, nil
			}
			c := NewClient(wrapped, ClientOptions{Release: MLLP2, ResponseTimeout: time.Second})
			defer c.Close()
			_, err = c.Send(context.Background(), testMessage(t, "INVALID-WRITER"))
			var exchange *ExchangeError
			if !errors.Is(err, io.ErrShortWrite) || !errors.As(err, &exchange) || exchange.RequestState != RequestUnknown || exchange.Phase != PhaseWriteRequest {
				t.Fatalf("invalid progress falsely asserted not sent: %v", err)
			}
			if _, again := c.Send(context.Background(), testMessage(t, "INVALID-WRITER")); again != err {
				t.Fatal("invalid progress replayed a request")
			}
			t.Cleanup(func() {
				select {
				case <-observed:
				case <-time.After(time.Second):
					t.Fatal("peer did not finish observing the poisoned connection")
				}
				if forwarded && requests.Load() != 1 {
					t.Fatal("peer did not observe the forwarded request")
				}
			})
		})
	}
}

func TestMLLP2AuthoredOutputIsBoundedBeforeWire(t *testing.T) {
	large := strings.Repeat("|\\\nSYNTHETIC", 1<<16)
	t.Run("client refuses before writing", func(t *testing.T) {
		conn, peer := net.Pipe()
		defer peer.Close()
		var writes atomic.Int32
		wrapped := &faultConnection{Conn: conn, writeFault: func(p []byte) (int, error) {
			writes.Add(1)
			return len(p), nil
		}}
		c := NewClient(wrapped, ClientOptions{Release: MLLP2, MaxFrameBytes: 1024})
		defer c.Close()
		message := testMessage(t, "BOUNDED")
		message.Add("ZXY").SetValue(1, large)
		_, err := c.Send(context.Background(), message)
		var exchange *ExchangeError
		if !errors.Is(err, ErrFrameTooLarge) || !errors.Is(err, hl7.ErrLimit) || !errors.As(err, &exchange) || exchange.RequestState != RequestNotSent || writes.Load() != 0 || c.broken != nil {
			t.Fatalf("authored output escaped local budget: %v / writes=%d", err, writes.Load())
		}
	})
	t.Run("server refuses authored response", func(t *testing.T) {
		var calls atomic.Int32
		address, log, _ := startServer(t, ServerOptions{Release: MLLP2, MaxFrameBytes: 1024,
			Handler: HandlerFunc(func(_ context.Context, r *Request) *hl7.Message {
				calls.Add(1)
				response := hl7.NewACK(r.Message, hl7.AckAccept, hl7.AckOptions{})
				shared := hl7.Text(large)
				response.Add("ZXY").Fields = []hl7.Field{shared, shared, shared}
				return response
			}),
		})
		c, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(time.Second))
		if err := rawWrite(c, rawFrameBytes(adt("BOUNDED", "MRN"))); err != nil {
			t.Fatal(err)
		}
		r := bufio.NewReader(c)
		if err := expectRawACK(r); err != nil {
			t.Fatal(err)
		}
		if _, err := r.ReadByte(); !errors.Is(err, io.EOF) {
			t.Fatalf("server emitted oversized application output: %v", err)
		}
		if failures := log.all(); len(failures) != 1 || failures[0] != ErrFrameTooLarge.Error() || calls.Load() != 1 {
			t.Fatalf("bounded handler output refusal: %v / calls=%d", failures, calls.Load())
		}
	})
}

func TestMLLP2MutualTLSAndServerCancellation(t *testing.T) {
	t.Run("mutual TLS", func(t *testing.T) {
		pki := testutil.NewTestPKI(t)
		server := pki.Issue(t, "localhost", "p256")
		client := pki.Issue(t, "client", "p256")
		address, _, _ := startServer(t, ServerOptions{Release: MLLP2, TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{server.Certificate}, ClientCAs: pki.CAPool,
			ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12,
		}})
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		c, err := Dial(ctx, address, ClientOptions{Release: MLLP2, TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{client.Certificate}, RootCAs: pki.CAPool,
			ServerName: "localhost", MinVersion: tls.VersionTLS12,
		}})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if response, err := c.Send(ctx, testMessage(t, "TLS2")); err != nil || ackOf(t, response).Code != hl7.AckAccept {
			t.Fatalf("MLLP2 over mutual TLS: %v", err)
		}
	})
	for _, stage := range []string{"partial request", "response confirmation"} {
		t.Run(stage, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			var calls atomic.Int32
			go func() {
				done <- Serve(ctx, listener, ServerOptions{Release: MLLP2, Handler: HandlerFunc(func(_ context.Context, r *Request) *hl7.Message {
					calls.Add(1)
					return hl7.NewACK(r.Message, hl7.AckAccept, hl7.AckOptions{})
				})})
			}()
			c, err := net.Dial("tcp", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(time.Second))
			r := bufio.NewReader(c)
			if stage == "partial request" {
				_ = rawWrite(c, []byte{0x0b, 'M'})
			} else {
				_ = rawWrite(c, rawFrameBytes(adt("CANCEL", "MRN")))
				if err := expectRawACK(r); err != nil {
					t.Fatal(err)
				}
				if _, err := rawReadFrame(r); err != nil {
					t.Fatal(err)
				}
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("server did not close and join pending MLLP2 I/O")
			}
			if _, err := r.ReadByte(); err == nil {
				t.Fatal("cancellation sent another frame")
			}
			if stage == "partial request" && calls.Load() != 0 || stage == "response confirmation" && calls.Load() != 1 {
				t.Fatal("cancellation replayed or admitted partial content")
			}
		})
	}
}

func TestMLLP2ConfirmationWriteFailureAndWholeDeadline(t *testing.T) {
	t.Run("response confirmation write", func(t *testing.T) {
		address := startRawPeer(t, func(c net.Conn, r *bufio.Reader) error {
			if _, err := rawReadFrame(r); err != nil {
				return err
			}
			if err := rawWrite(c, append(rawFrameBytes([]byte{0x06}), rawFrameBytes(rawApplicationACK("AA", "W"))...)); err != nil {
				return err
			}
			_, _ = io.Copy(io.Discard, c)
			return nil
		})
		conn, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		wrapped := &faultConnection{Conn: conn}
		cause := errors.New("synthetic response-confirmation write refusal")
		writes := 0
		wrapped.writeFault = func(p []byte) (int, error) {
			writes++
			if writes > 1 {
				return 0, cause
			}
			return conn.Write(p)
		}
		c := NewClient(wrapped, ClientOptions{Release: MLLP2, ResponseTimeout: time.Second})
		defer c.Close()
		_, err = c.Send(context.Background(), testMessage(t, "W"))
		var exchange *ExchangeError
		if !errors.Is(err, cause) || !errors.As(err, &exchange) || exchange.Phase != PhaseResponseConfirmation || exchange.RequestState != RequestAcknowledged {
			t.Fatalf("response confirmation failure lost request transport result: %v", err)
		}
	})
	t.Run("whole exchange caps confirmation", func(t *testing.T) {
		address := startRawPeer(t, func(c net.Conn, r *bufio.Reader) error {
			if _, err := rawReadFrame(r); err != nil {
				return err
			}
			_, _ = io.Copy(io.Discard, c)
			return nil
		})
		c, err := Dial(context.Background(), address, ClientOptions{Release: MLLP2, ResponseTimeout: 40 * time.Millisecond, TransportACKTimeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_, err = c.Send(context.Background(), testMessage(t, "DEADLINE"))
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatal(err)
		}
	})
}

func TestMLLPFrameStartDeadlineAndFatalListenerCleanup(t *testing.T) {
	t.Run("frame start deadline refusal", func(t *testing.T) {
		cause := errors.New("synthetic frame deadline refusal")
		r := NewReader(bytes.NewReader(rawFrameBytes([]byte("synthetic"))), 64)
		r.onStart = func() error { return cause }
		if p, err := r.ReadFrame(); !errors.Is(err, cause) || p != nil {
			t.Fatalf("frame read ignored deadline refusal: %v", err)
		}
	})
	t.Run("listener fails with pending transport ACK", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		done := make(chan error, 1)
		go func() {
			done <- Serve(context.Background(), listener, ServerOptions{Release: MLLP2,
				Handler: HandlerFunc(func(_ context.Context, r *Request) *hl7.Message {
					return hl7.NewACK(r.Message, hl7.AckAccept, hl7.AckOptions{})
				}),
			})
		}()
		c, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(time.Second))
		r := bufio.NewReader(c)
		if err := rawWrite(c, rawFrameBytes(adt("LISTENER", "MRN"))); err != nil {
			t.Fatal(err)
		}
		if err := expectRawACK(r); err != nil {
			t.Fatal(err)
		}
		if _, err := rawReadFrame(r); err != nil {
			t.Fatal(err)
		}
		_ = listener.Close()
		select {
		case err := <-done:
			if !errors.Is(err, net.ErrClosed) {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("fatal listener error waited for the idle confirmation instead of canceling I/O")
		}
		if _, err := r.ReadByte(); err == nil {
			t.Fatal("accepted connection remained open after fatal listener error")
		}
	})
}
