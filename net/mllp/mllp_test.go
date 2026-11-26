package mllp

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/ThalesMMS/dicom-go/hl7"
	"github.com/ThalesMMS/dicom-go/internal/testutil"
)

// Synthetic messages: no real patients.
const secretName = "SYNTHETIC^SECRET"

func adt(controlID, pid3 string) []byte {
	return []byte(strings.Join([]string{
		`MSH|^~\&|ADT1|HOSP|TWIN|RAD|20261007120000||ADT^A08^ADT_A01|` + controlID + `|P|2.5.1`,
		`EVN|A08|20261007115900`,
		`PID|1||` + pid3 + `||` + secretName + `||19700101|F`,
	}, "\r") + "\r")
}

func frame(payload []byte) []byte {
	out, err := AppendFrame(nil, payload)
	if err != nil {
		panic(err)
	}
	return out
}

func TestReaderFrames(t *testing.T) {
	stream := append(frame([]byte("MSH|first")), frame([]byte("MSH|second"))...)
	reader := NewReader(iotest.OneByteReader(bytes.NewReader(stream)), 0)
	for _, want := range []string{"MSH|first", "MSH|second"} {
		got, err := reader.ReadFrame()
		if err != nil || string(got) != want {
			t.Fatalf("ReadFrame = %q, %v; want %q", got, err, want)
		}
	}
	if _, err := reader.ReadFrame(); !errors.Is(err, io.EOF) {
		t.Fatalf("end of stream: %v", err)
	}
	big := bytes.Repeat([]byte("x"), 100<<10)
	if got, err := NewReader(bytes.NewReader(frame(big)), 200<<10).ReadFrame(); err != nil || !bytes.Equal(got, big) {
		t.Fatalf("large frame: %d bytes, %v", len(got), err)
	}
}

func TestReaderRejectsBrokenFraming(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream []byte
		max    int
		want   error
	}{
		{"bytes before the start block", []byte("MSH|x\x0b\x1c\r"), 0, ErrFraming},
		{"no CR after the end block", []byte("\x0bMSH|x\x1cX"), 0, ErrFraming},
		{"start block inside", []byte("\x0bMSH|\x0bx\x1c\r"), 0, ErrFraming},
		{"empty frame", []byte("\x0b\x1c\r"), 0, ErrFraming},
		{"too large", append(append([]byte{StartBlock}, bytes.Repeat([]byte("x"), 100)...), EndBlock, CarriageReturn), 10, ErrFrameTooLarge},
		{"too large, unterminated", append([]byte{StartBlock}, bytes.Repeat([]byte("x"), 100<<10)...), 1 << 10, ErrFrameTooLarge},
		{"end of stream inside", []byte("\x0bMSH|x"), 0, io.ErrUnexpectedEOF},
		{"end of stream after the end block", []byte("\x0bMSH|x\x1c"), 0, io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewReader(bytes.NewReader(tc.stream), tc.max).ReadFrame(); !errors.Is(err, tc.want) {
				t.Fatalf("ReadFrame error %v, want %v", err, tc.want)
			}
		})
	}
	if err := WriteFrame(io.Discard, []byte("MSH|\x1cx")); !errors.Is(err, ErrFraming) {
		t.Fatalf("WriteFrame with an end block in the payload: %v", err)
	}
}

type errorLog struct {
	mu     sync.Mutex
	errors []string
}

func (l *errorLog) add(_ net.Addr, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errors = append(l.errors, err.Error())
}

func (l *errorLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.errors...)
}

// startServer serves an ADT handler: A08 and A40 are typed and
// acknowledged with ACKFor.
func startServer(t *testing.T, options ServerOptions) (string, *errorLog, *[]*hl7.ADTMessage) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var received []*hl7.ADTMessage
	log := &errorLog{}
	if options.Handler == nil && options.OriginalACKHandler == nil {
		options.Handler = HandlerFunc(func(_ context.Context, request *Request) *hl7.Message {
			typed, err := hl7.ParseADT(request.Message)
			if err == nil {
				mu.Lock()
				received = append(received, typed)
				mu.Unlock()
			}
			return hl7.ACKFor(request.Message, err, hl7.AckOptions{})
		})
	}
	options.OnError = log.add
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, listener, options) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return listener.Addr().String(), log, &received
}

func ackOf(t *testing.T, response *hl7.Message) hl7.Acknowledgment {
	t.Helper()
	ack, err := hl7.ParseAcknowledgment(response)
	if err != nil {
		t.Fatal(err)
	}
	return ack
}

func TestServerAndClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	address, log, received := startServer(t, ServerOptions{})
	client, err := Dial(ctx, address, ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	send := func(payload []byte) *hl7.Message {
		t.Helper()
		message, err := hl7.Parse(payload)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Send(ctx, message)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	if ack := ackOf(t, send(adt("C1", "MRN1^^^HOSP^MR"))); ack.Code != hl7.AckAccept || ack.ControlID != "C1" {
		t.Fatalf("A08: %+v", ack)
	}
	if ack := ackOf(t, send(adt("C2", ""))); ack.Code != hl7.AckError || ack.ErrorCode != hl7.ErrorCodeRequiredFieldMissing || ack.ControlID != "C2" {
		t.Fatalf("A08 without PID-3: %+v", ack)
	}
	if len(*received) != 1 || (*received)[0].Patient.Identifiers[0].ID != "MRN1" {
		t.Fatalf("handler received %+v", *received)
	}
	for _, line := range log.all() {
		if strings.Contains(line, "SECRET") {
			t.Fatalf("server error quotes the message: %q", line)
		}
	}
}

func rawExchange(t *testing.T, address string, stream []byte) ([]byte, error) {
	t.Helper()
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(stream); err != nil {
		t.Fatal(err)
	}
	return NewReader(conn, 0).ReadFrame()
}

func TestServerRejectsWhatItCannotProcess(t *testing.T) {
	address, log, _ := startServer(t, ServerOptions{MaxFrameBytes: 4 << 10})

	// A body that does not parse, under a readable MSH: AR.
	body := []byte("MSH|^~\\&|ADT1|HOSP|TWIN|RAD|20261007||ADT^A08|C3|P|2.5\rpid|" + secretName + "\r")
	payload, err := rawExchange(t, address, frame(body))
	if err != nil {
		t.Fatal(err)
	}
	response, err := hl7.Parse(payload)
	if err != nil {
		t.Fatal(err)
	}
	if ack := ackOf(t, response); ack.Code != hl7.AckReject || ack.ControlID != "C3" || ack.ErrorCode != hl7.ErrorCodeDataType {
		t.Fatalf("unparseable body: %+v", ack)
	}
	// No MSH, a broken frame, an oversized frame: the connection closes.
	for name, stream := range map[string][]byte{
		"no MSH":      frame([]byte("PID|1||" + secretName)),
		"unframed":    []byte("MSH|^~\\&|" + secretName + "\r"),
		"bad trailer": []byte("\x0bMSH|^~\\&|" + secretName + "\x1cX"),
		"oversized":   frame(bytes.Repeat([]byte("x"), 8<<10)),
	} {
		if _, err := rawExchange(t, address, stream); err == nil {
			t.Fatalf("%s: the server answered", name)
		}
	}
	time.Sleep(50 * time.Millisecond)
	errorsSeen := log.all()
	if len(errorsSeen) < 4 {
		t.Fatalf("OnError saw %d errors: %q", len(errorsSeen), errorsSeen)
	}
	for _, line := range errorsSeen {
		if strings.Contains(line, "SECRET") {
			t.Fatalf("server error quotes the message: %q", line)
		}
	}
}

func TestServerHandlerPanicAndNilResponse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	calls := 0
	address, _, _ := startServer(t, ServerOptions{Handler: HandlerFunc(func(context.Context, *Request) *hl7.Message {
		calls++
		if calls == 1 {
			panic("boom")
		}
		return nil
	})})
	client, err := Dial(ctx, address, ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	message, _ := hl7.Parse(adt("C4", "MRN1"))
	response, err := client.Send(ctx, message)
	if err != nil {
		t.Fatal(err)
	}
	if ack := ackOf(t, response); ack.Code != hl7.AckReject || ack.ErrorCode != hl7.ErrorCodeApplicationInternalError {
		t.Fatalf("panic: %+v", ack)
	}
	if response, err = client.Send(ctx, message); err != nil {
		t.Fatal(err)
	}
	if ack := ackOf(t, response); ack.Code != hl7.AckAccept {
		t.Fatalf("nil response: %+v", ack)
	}
}

func TestClientDetectsMismatchAndTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	other, _ := hl7.Parse(adt("OTHER", "MRN1"))
	address, _, _ := startServer(t, ServerOptions{Handler: HandlerFunc(func(context.Context, *Request) *hl7.Message {
		return hl7.NewACK(other, hl7.AckAccept, hl7.AckOptions{})
	})})
	client, err := Dial(ctx, address, ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	message, _ := hl7.Parse(adt("MINE", "MRN1"))
	if _, err := client.Send(ctx, message); !errors.Is(err, ErrResponseMismatch) {
		t.Fatalf("mismatched ACK: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			_, _ = io.Copy(io.Discard, conn) // never answers
		}
	}()
	silent, err := Dial(ctx, listener.Addr().String(), ClientOptions{ResponseTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	if _, err := silent.Send(ctx, message); err == nil {
		t.Fatal("silent server: no error")
	}
	if _, err := silent.Send(ctx, message); err == nil {
		t.Fatal("a broken client sent again")
	}
}

func TestServerOverTLS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pki := testutil.NewTestPKI(t)
	server := pki.Issue(t, "localhost", "p256")
	client := pki.Issue(t, "client", "p256")
	address, _, _ := startServer(t, ServerOptions{TLSConfig: &tls.Config{
		Certificates: []tls.Certificate{server.Certificate}, ClientCAs: pki.CAPool,
		ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12,
	}})
	c, err := Dial(ctx, address, ClientOptions{TLSConfig: &tls.Config{
		Certificates: []tls.Certificate{client.Certificate}, RootCAs: pki.CAPool, ServerName: "localhost", MinVersion: tls.VersionTLS12,
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	message, _ := hl7.Parse(adt("T1", "MRN1"))
	response, err := c.Send(ctx, message)
	if err != nil {
		t.Fatal(err)
	}
	if ack := ackOf(t, response); ack.Code != hl7.AckAccept {
		t.Fatalf("TLS: %+v", ack)
	}
	// Without a client certificate the handshake fails.
	if anonymous, err := Dial(ctx, address, ClientOptions{TLSConfig: &tls.Config{RootCAs: pki.CAPool, ServerName: "localhost", MinVersion: tls.VersionTLS13}}); err == nil {
		_, err = anonymous.Send(ctx, message)
		anonymous.Close()
		if err == nil {
			t.Fatal("a client without a certificate was served")
		}
	}
}

func FuzzReadFrame(f *testing.F) {
	f.Add(frame([]byte("MSH|^~\\&|A")))
	f.Add([]byte("\x0bMSH|\x1c\x1c\r"))
	f.Add([]byte("\x0b\x0b\x1c\r\x0bX\x1c\r"))
	f.Fuzz(func(t *testing.T, stream []byte) {
		reader := NewReader(bytes.NewReader(stream), 64)
		for i := 0; i < 8; i++ {
			payload, err := reader.ReadFrame()
			if err != nil {
				return
			}
			if len(payload) == 0 || len(payload) > 64 || bytes.IndexByte(payload, StartBlock) >= 0 {
				t.Fatalf("accepted payload %q", payload)
			}
		}
	})
}

func TestServerClosesIdleAndStalledConnections(t *testing.T) {
	address, _, _ := startServer(t, ServerOptions{IdleTimeout: 100 * time.Millisecond, ReadTimeout: 100 * time.Millisecond})
	for name, stream := range map[string][]byte{
		"idle":    nil,
		"stalled": []byte("\x0bMSH|^~\\&|partial"),
	} {
		conn, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		if len(stream) > 0 {
			if _, err := conn.Write(stream); err != nil {
				t.Fatal(err)
			}
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("%s connection: read %v, want the server to close it", name, err)
		}
		conn.Close()
	}
}

func TestServerConnectionLimit(t *testing.T) {
	address, log, _ := startServer(t, ServerOptions{MaxConnections: 1})
	first, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := first.Write(frame(adt("L1", "MRN1"))); err != nil {
		t.Fatal(err)
	}
	if _, err := NewReader(first, 0).ReadFrame(); err != nil {
		t.Fatal(err)
	}
	if _, err := rawExchange(t, address, frame(adt("L2", "MRN1"))); err == nil {
		t.Fatal("a second connection was served beyond the limit")
	}
	found := false
	for _, line := range log.all() {
		found = found || strings.Contains(line, "connection limit")
	}
	if !found {
		t.Fatalf("errors %q lack the connection limit", log.all())
	}
}
