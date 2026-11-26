package hl7cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/hl7"
	"github.com/ThalesMMS/dicom-go/net/mllp"

	"github.com/ThalesMMS/dicom-go/internal/privatefs"
)

func fixture(t *testing.T) *hl7.Message {
	t.Helper()
	m := hl7.NewMessage(hl7.MessageHeader{Type: hl7.MessageType{Code: "ADT", Trigger: "A08"}, ControlID: "synthetic-1", Version: "2.5.1"})
	m.Add("PID").SetValue(3, "synthetic-patient")
	return m
}
func fixtureText(t *testing.T) string {
	b, err := fixture(t).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func waitStopped(t *testing.T, ch <-chan int) int {
	t.Helper()
	select {
	case code := <-ch:
		return code
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop and join")
		return -1
	}
}

func startServer(t *testing.T, handler mllp.HandlerFunc) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- mllp.Serve(ctx, listener, mllp.ServerOptions{Handler: handler, MaxFrameBytes: defaultBytes, IdleTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			t.Error("server lifetime leak")
		}
	})
	return listener.Addr().String()
}

func TestSenderAcknowledgments(t *testing.T) {
	cases := []struct {
		name      string
		code      hl7.AckCode
		correlate bool
		want      int
	}{{"AA", hl7.AckAccept, true, 0}, {"AE", hl7.AckError, true, 1}, {"AR", hl7.AckReject, true, 1}, {"CA", "CA", true, 1}, {"mismatch", hl7.AckAccept, false, 1}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			address := startServer(t, func(_ context.Context, r *mllp.Request) *hl7.Message {
				m := hl7.NewACK(r.Message, tc.code, hl7.AckOptions{})
				if !tc.correlate {
					m.Segments[1].SetValue(2, "other")
				}
				return m
			})
			var out, diag bytes.Buffer
			code := Run(context.Background(), "hl7snd", []string{"-address", address, "-timeout", "1s", "-"}, strings.NewReader(fixtureText(t)), &out, &diag)
			if code != tc.want {
				t.Fatalf("exit %d, expected %d: %s", code, tc.want, &diag)
			}
			if strings.Contains(diag.String(), "synthetic-patient") || strings.Contains(diag.String(), "synthetic-1") {
				t.Fatal("content leaked")
			}
			if tc.want == 0 && out.String() != "AA\n" {
				t.Fatal("successful acknowledgment missing")
			}
		})
	}
}

func TestSenderRefusesAmbiguousAcknowledgments(t *testing.T) {
	address := startServer(t, func(_ context.Context, r *mllp.Request) *hl7.Message {
		message := hl7.NewACK(r.Message, hl7.AckAccept, hl7.AckOptions{})
		msa := message.Add("MSA")
		msa.SetValue(1, "AR")
		msa.SetValue(2, r.Message.Header().ControlID)
		return message
	})
	var out, diag bytes.Buffer
	if Run(context.Background(), "hl7snd", []string{"-address", address, "-"}, strings.NewReader(fixtureText(t)), &out, &diag) != 1 || out.Len() != 0 {
		t.Fatal("ambiguous AA/AR response accepted")
	}
}

func TestSenderMissingACKAndTimeout(t *testing.T) {
	t.Run("no-MSA", func(t *testing.T) {
		address := startServer(t, func(_ context.Context, r *mllp.Request) *hl7.Message { return r.Message })
		var out, diag bytes.Buffer
		if Run(context.Background(), "hl7snd", []string{"-address", address, "-"}, strings.NewReader(fixtureText(t)), &out, &diag) != 1 {
			t.Fatal("non-ack response accepted")
		}
	})
	t.Run("timeout", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = mllp.NewReader(conn, defaultBytes).ReadFrame()
			<-ctx.Done()
		}()
		var out, diag bytes.Buffer
		if Run(context.Background(), "hl7snd", []string{"-address", listener.Addr().String(), "-timeout", "30ms", "-"}, strings.NewReader(fixtureText(t)), &out, &diag) != 1 {
			t.Fatal("unanswered request accepted")
		}
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("timeout test peer leaked")
		}
	})
}

type receiverOutput struct {
	mu sync.Mutex
	bytes.Buffer
	ready chan string
}

func (w *receiverOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if strings.HasPrefix(string(p), "listening ") {
		select {
		case w.ready <- strings.TrimSpace(strings.TrimPrefix(string(p), "listening ")):
		default:
		}
	}
	return w.Buffer.Write(p)
}
func (w *receiverOutput) text() string { w.mu.Lock(); defer w.mu.Unlock(); return w.Buffer.String() }
func startReceiver(t *testing.T, extra ...string) (context.CancelFunc, string, <-chan int, *receiverOutput) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	out := &receiverOutput{ready: make(chan string, 1)}
	done := make(chan int, 1)
	args := append([]string{"-listen", "127.0.0.1:0", "-duration", "2s", "-timeout", "1s"}, extra...)
	go func() { done <- Run(ctx, "hl7rcv", args, strings.NewReader(""), out, io.Discard) }()
	select {
	case address := <-out.ready:
		return cancel, address, done, out
	case code := <-done:
		cancel()
		t.Fatalf("receiver exited %d before listen", code)
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("receiver did not listen")
	}
	return nil, "", nil, nil
}
func sendOne(t *testing.T, address string) *hl7.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := mllp.Dial(ctx, address, mllp.ClientOptions{ResponseTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	response, err := c.Send(ctx, fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestReceiverPersistenceAndCancellation(t *testing.T) {
	dir := t.TempDir()
	cancel, address, done, out := startReceiver(t, "-output-dir", dir)
	defer cancel()
	response := sendOne(t, address)
	ack, err := hl7.ParseAcknowledgment(response)
	if err != nil || ack.Code != hl7.AckAccept {
		t.Fatalf("persisted message not accepted: %v", err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatal("one atomic file expected")
	}
	data, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, []byte(fixtureText(t))) {
		t.Fatal("stored message changed")
	}
	if privatefs.Check(filepath.Join(dir, files[0].Name())) != nil {
		t.Fatal("message file permits group/world access")
	}
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte{mllp.StartBlock, 'M', 'S'})
	cancel()
	if code := waitStopped(t, done); code != 0 {
		t.Fatalf("cancellation exit %d", code)
	}
	if !strings.Contains(out.text(), "persisted 1") {
		t.Fatal("summary missing")
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	_, err = conn.Read(b[:])
	if err == nil {
		t.Fatal("partial-frame connection remained open")
	}
}

func TestReceiverQuotas(t *testing.T) {
	t.Run("disk", func(t *testing.T) {
		dir := t.TempDir()
		cancel, address, done, _ := startReceiver(t, "-output-dir", dir, "-max-output-bytes", "1")
		defer cancel()
		ack, err := hl7.ParseAcknowledgment(sendOne(t, address))
		if err != nil || ack.Code != hl7.AckError {
			t.Fatal("disk quota must produce AE")
		}
		cancel()
		if waitStopped(t, done) != 1 {
			t.Fatal("refusal exit status missing")
		}
		files, _ := os.ReadDir(dir)
		if len(files) != 0 {
			t.Fatal("quota created files")
		}
	})
	t.Run("messages", func(t *testing.T) {
		cancel, address, done, _ := startReceiver(t, "-max-messages", "1")
		defer cancel()
		ack, err := hl7.ParseAcknowledgment(sendOne(t, address))
		if err != nil || ack.Code != hl7.AckAccept {
			t.Fatal("first admission refused")
		}
		ctx, c := context.WithTimeout(context.Background(), time.Second)
		defer c()
		client, err := mllp.Dial(ctx, address, mllp.ClientOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		response, err := client.Send(ctx, fixture(t))
		if err == nil {
			ack, e := hl7.ParseAcknowledgment(response)
			if e != nil || ack.Code != hl7.AckReject {
				t.Fatal("excess admission accepted")
			}
		}
		if waitStopped(t, done) != 1 {
			t.Fatal("exhausted receiver did not report refusal")
		}
	})
}

func TestConvertersAtomicRoundTripAndLimits(t *testing.T) {
	var xmlOut, diag bytes.Buffer
	if code := Run(context.Background(), "hl72xml", []string{"-output", "-", "-"}, strings.NewReader(fixtureText(t)), &xmlOut, &diag); code != 0 {
		t.Fatalf("encode %d: %s", code, &diag)
	}
	dir := t.TempDir()
	dest := filepath.Join(dir, "message.hl7")
	if code := Run(context.Background(), "xml2hl7", []string{"-output", dest, "-"}, bytes.NewReader(xmlOut.Bytes()), io.Discard, &diag); code != 0 {
		t.Fatalf("decode %d: %s", code, &diag)
	}
	data, _ := os.ReadFile(dest)
	if string(data) != fixtureText(t) {
		t.Fatal("conversion changed canonical message")
	}
	if code := Run(context.Background(), "xml2hl7", []string{"-output", dest, "-"}, bytes.NewReader(xmlOut.Bytes()), io.Discard, &diag); code != 1 {
		t.Fatal("existing destination overwritten")
	}
	data, _ = os.ReadFile(dest)
	if string(data) != fixtureText(t) {
		t.Fatal("refusal changed destination")
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 1 {
		t.Fatal("staging file left behind")
	}
	var out bytes.Buffer
	if Run(context.Background(), "hl72xml", []string{"-max-bytes", "1", "-output", "-", "-"}, strings.NewReader(fixtureText(t)), &out, &diag) != 1 || out.Len() != 0 {
		t.Fatal("oversized input produced output")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if Run(ctx, "xml2hl7", []string{"-output", filepath.Join(dir, "canceled"), "-"}, bytes.NewReader(xmlOut.Bytes()), io.Discard, &diag) != 1 {
		t.Fatal("canceled conversion succeeded")
	}
}

func TestFlagsAndMalformedDiagnosticsDoNotQuoteValues(t *testing.T) {
	for _, cmd := range []string{"hl7snd", "hl7rcv", "hl72xml", "xml2hl7"} {
		var out, diag bytes.Buffer
		if Run(context.Background(), cmd, []string{"-max-bytes", "sensitive-value"}, strings.NewReader(""), &out, &diag) != 2 {
			t.Fatal("invalid flags accepted")
		}
		if strings.Contains(diag.String(), "sensitive-value") {
			t.Fatal("flag error exposed argument")
		}
	}
	var out, diag bytes.Buffer
	if Run(context.Background(), "xml2hl7", []string{"-output", "-", "-"}, strings.NewReader(`<sensitive-value>`), &out, &diag) != 1 {
		t.Fatal("malformed XML accepted")
	}
	if strings.Contains(diag.String(), "sensitive-value") {
		t.Fatal("XML diagnostic leaked content")
	}
}

func TestAtomicCreateDoesNotReplaceSymlink(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "original")
	if err := os.WriteFile(original, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(original, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	if err := createOutput(link, []byte("new")); err == nil {
		t.Fatal("symlink destination replaced")
	}
	data, _ := os.ReadFile(original)
	if string(data) != "original" {
		t.Fatal("symlink target changed")
	}
}

func TestShortWriter(t *testing.T) {
	if !errors.Is(writeOutput("-", []byte("data"), shortWriter{}), io.ErrShortWrite) {
		t.Fatal("short stdout write accepted")
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func ExampleRun() {
	var output bytes.Buffer
	code := Run(context.Background(), "hl72xml", []string{"-output", "-", "-"}, strings.NewReader("MSH|^~\\&|S|F|R|F||ADT^A08|id|P|2.5.1\r"), &output, io.Discard)
	fmt.Println(code, strings.Contains(output.String(), "hl7-message"))
	// Output: 0 true
}

type errorWriter struct{ attempt string }

func (w *errorWriter) Write(p []byte) (int, error) {
	w.attempt = string(p)
	return 0, errors.New("synthetic output failure")
}

type finalErrorWriter struct{ initial bool }

func (w *finalErrorWriter) Write(p []byte) (int, error) {
	if !w.initial {
		w.initial = true
		return len(p), nil
	}
	return 0, errors.New("synthetic summary failure")
}

func TestSummaryOutputFailures(t *testing.T) {
	t.Run("sender", func(t *testing.T) {
		address := startServer(t, func(_ context.Context, r *mllp.Request) *hl7.Message {
			return hl7.NewACK(r.Message, hl7.AckAccept, hl7.AckOptions{})
		})
		var diag bytes.Buffer
		if Run(context.Background(), "hl7snd", []string{"-address", address, "-"}, strings.NewReader(fixtureText(t)), &errorWriter{}, &diag) != 1 {
			t.Fatal("failed AA summary returned success")
		}
		if Run(context.Background(), "hl7snd", []string{"-address", address, "-"}, strings.NewReader(fixtureText(t)), shortWriter{}, &diag) != 1 {
			t.Fatal("short AA summary returned success")
		}
	})
	t.Run("receiver-startup", func(t *testing.T) {
		writer := &errorWriter{}
		var diag bytes.Buffer
		if Run(context.Background(), "hl7rcv", []string{"-listen", "127.0.0.1:0", "-duration", "1s"}, strings.NewReader(""), writer, &diag) != 1 {
			t.Fatal("failed listener summary returned success")
		}
		address := strings.TrimSpace(strings.TrimPrefix(writer.attempt, "listening "))
		conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			t.Fatal("listener remained open after startup output failure")
		}
	})
	t.Run("receiver-final", func(t *testing.T) {
		var diag bytes.Buffer
		if Run(context.Background(), "hl7rcv", []string{"-listen", "127.0.0.1:0", "-duration", "20ms"}, strings.NewReader(""), &finalErrorWriter{}, &diag) != 1 {
			t.Fatal("failed final summary returned success")
		}
	})
}

func TestMLLP2CommandsSeparateTransportAndApplicationResults(t *testing.T) {
	for _, persisted := range []bool{true, false} {
		t.Run(fmt.Sprint(persisted), func(t *testing.T) {
			dir := t.TempDir()
			args := []string{"-mllp-release", "2", "-transport-ack-timeout", "1s", "-output-dir", dir}
			if !persisted {
				args = append(args, "-max-output-bytes", "1")
			}
			cancel, address, done, summary := startReceiver(t, args...)
			defer cancel()
			var out, diag bytes.Buffer
			code := Run(context.Background(), "hl7snd", []string{"-address", address, "-mllp-release", "2", "-transport-ack-timeout", "1s", "-timeout", "1s", "-"}, strings.NewReader(fixtureText(t)), &out, &diag)
			files, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if persisted {
				if code != 0 || out.String() != "AA\n" || len(files) != 1 {
					t.Fatalf("MLLP2 persistence/application result: %d %s %s", code, &out, &diag)
				}
			} else if code != 1 || out.Len() != 0 || len(files) != 0 || !strings.Contains(diag.String(), "AE acknowledgment") {
				t.Fatalf("transport ACK masked failed application publication: %d %s", code, &diag)
			}
			cancel()
			want := 0
			if !persisted {
				want = 1
			}
			if result := waitStopped(t, done); result != want || !strings.Contains(summary.text(), "admitted 1") {
				t.Fatalf("receiver result: %d %s", result, summary.text())
			}
		})
	}
}

func TestMLLP2SenderTransportNAKDiagnostic(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		c, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(time.Second))
		_, err = mllp.NewReader(c, defaultBytes).ReadFrame()
		if err == nil {
			_, err = c.Write([]byte{0x0b, 0x15, 0x1c, 0x0d})
		}
		done <- err
	}()
	var out, diag bytes.Buffer
	if code := Run(context.Background(), "hl7snd", []string{"-address", listener.Addr().String(), "-mllp-release", "2", "-timeout", "1s", "-"}, strings.NewReader(fixtureText(t)), &out, &diag); code != 1 || out.Len() != 0 || !strings.Contains(diag.String(), "transport NAK") || strings.Contains(diag.String(), "synthetic") {
		t.Fatalf("NAK confused with application rejection: %d %s", code, &diag)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestMLLPCommandReleaseFlagsRefuseBeforeNetwork(t *testing.T) {
	for _, command := range []string{"hl7snd", "hl7rcv"} {
		for _, flags := range [][]string{{"-mllp-release", "0"}, {"-mllp-release", "3"}, {"-mllp-release", "invalid"}, {"-transport-ack-timeout", "0s"}, {"-transport-ack-timeout", "6m"}} {
			args := append([]string(nil), flags...)
			if command == "hl7snd" {
				args = append(args, "-")
			}
			var out, diag bytes.Buffer
			if code := Run(context.Background(), command, args, strings.NewReader(fixtureText(t)), &out, &diag); code != 2 || out.Len() != 0 {
				t.Fatalf("%s flags accepted: %v %d %s", command, flags, code, &diag)
			}
		}
	}
}
