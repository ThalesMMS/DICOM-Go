package jpeg2000

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/codecfixture"
)

func init() {
	if os.Getenv("DICOM_GO_TEST_NATIVE_HELPER") == "1" {
		nativeFakeMain()
		os.Exit(0)
	}
}
func nativeFakeMain() {
	mode := os.Getenv("DICOM_GO_TEST_NATIVE_MODE")
	for {
		var h [44]byte
		if _, e := io.ReadFull(os.Stdin, h[:]); e != nil {
			return
		}
		n := binary.LittleEndian.Uint32(h[40:])
		if n > nativeMaximumBytes {
			return
		}
		hello := binary.LittleEndian.Uint32(h[8:]) == 1
		if !hello && mode == "blocked-write" {
			time.Sleep(time.Minute)
			return
		}
		payload := make([]byte, n)
		if _, e := io.ReadFull(os.Stdin, payload); e != nil {
			return
		}
		if (hello && mode == "hello-hang") || (!hello && mode == "hang") {
			time.Sleep(time.Minute)
			return
		}
		if !hello && mode == "crash" {
			os.Exit(17)
		}
		var out [36]byte
		copy(out[:], "J2KH")
		binary.LittleEndian.PutUint32(out[4:], 1)
		copy(out[8:16], h[12:20])
		copy(out[20:32], h[20:32])
		var pixels []byte
		if hello {
			pixels = []byte(QualifiedOpenJPEGVersion)
			binary.LittleEndian.PutUint32(out[28:], 1)
			if mode == "version" {
				pixels = []byte("9.99.99")
			}
		} else {
			size := uint64(binary.LittleEndian.Uint32(h[20:])) * uint64(binary.LittleEndian.Uint32(h[24:])) * uint64(binary.LittleEndian.Uint32(h[28:])) * uint64(binary.LittleEndian.Uint32(h[32:])/8)
			if size > nativeMaximumBytes {
				return
			}
			pixels = bytes.Repeat([]byte{42}, int(size))
			if mode == "wrong-id" {
				binary.LittleEndian.PutUint64(out[8:], 999)
			}
			if mode == "status" {
				binary.LittleEndian.PutUint32(out[16:], 2)
				pixels = nil
			}
			if mode == "oversized" {
				binary.LittleEndian.PutUint32(out[32:], uint32(size+1))
				_ = nativeWriteAll(os.Stdout, out[:])
				continue
			}
		}
		binary.LittleEndian.PutUint32(out[32:], uint32(len(pixels)))
		if nativeWriteAll(os.Stdout, out[:]) != nil || nativeWriteAll(os.Stdout, pixels) != nil {
			return
		}
	}
}
func nativeFakeConfig(t *testing.T, mode string) NativeSessionConfig {
	t.Helper()
	t.Setenv("DICOM_GO_TEST_NATIVE_HELPER", "1")
	t.Setenv("DICOM_GO_TEST_NATIVE_MODE", mode)
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	return NativeSessionConfig{Executable: exe, Backend: NativeOpenJPEG, Timeout: 2 * time.Second}
}
func nativeTestInput(t *testing.T) ([]byte, pixeldata.Metadata) {
	t.Helper()
	b, e := os.ReadFile("testdata/openjpeg/u8-gray-reversible.j2k")
	if e != nil {
		t.Fatal(e)
	}
	return b, codecfixture.JPEG2000LosslessEncoderCases()[0].Metadata
}
func TestNativeSessionConcurrentOwnershipAndClose(t *testing.T) {
	s, e := OpenNativeSession(context.Background(), nativeFakeConfig(t, "ok"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	b, m := nativeTestInput(t)
	original := bytes.Clone(b)
	var wg sync.WaitGroup
	errorsOut := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, e := s.DecodeFrame(b, m)
			if e == nil && (!bytes.Equal(p, bytes.Repeat([]byte{42}, len(p))) || len(p) != int(m.Rows)*int(m.Columns)) {
				e = errors.New("incorrect pixels")
			}
			if e != nil {
				errorsOut <- e
			}
			if len(p) > 0 {
				p[0] = 0
			}
		}()
	}
	wg.Wait()
	close(errorsOut)
	for e := range errorsOut {
		t.Error(e)
	}
	if !bytes.Equal(b, original) {
		t.Fatal("borrowed input modified")
	}
	if stats := s.Stats(); stats.Starts != 1 || stats.Frames != 20 || stats.PID <= 0 {
		t.Fatalf("unexpected lifecycle %+v", stats)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	if stats := s.Stats(); !stats.Closed || stats.PID != 0 {
		t.Fatalf("retained process %+v", stats)
	}
	if _, e = s.DecodeFrame(b, m); !errors.Is(e, ErrNativeHelperUnavailable) {
		t.Fatal(e)
	}
}
func TestNativeSessionCancelBlockedIOAndRestart(t *testing.T) {
	for _, mode := range []string{"hang", "blocked-write"} {
		t.Run(mode, func(t *testing.T) {
			s, e := OpenNativeSession(context.Background(), nativeFakeConfig(t, mode))
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			b, m := nativeTestInput(t)
			if mode == "blocked-write" {
				b = append(bytes.Clone(b), make([]byte, 2<<20)...)
			}
			ctx, c := context.WithTimeout(context.Background(), 40*time.Millisecond)
			defer c()
			if p, e := s.DecodeFrameContext(ctx, b, m); !errors.Is(e, context.DeadlineExceeded) || p != nil {
				t.Fatalf("pixels %d err %v", len(p), e)
			}
			if stats := s.Stats(); stats.PID != 0 || stats.Failures != 0 {
				t.Fatalf("canceled process retained or budget charged %+v", stats)
			}
			t.Setenv("DICOM_GO_TEST_NATIVE_MODE", "ok")
			if p, e := s.DecodeFrame(b, m); e != nil || len(p) == 0 {
				t.Fatalf("clean restart: %v", e)
			}
			if stats := s.Stats(); stats.Starts != 2 {
				t.Fatal(stats)
			}
		})
	}
}
func TestNativeSessionProtocolFailuresAndRestartBudget(t *testing.T) {
	for _, mode := range []string{"wrong-id", "oversized", "crash"} {
		t.Run(mode, func(t *testing.T) {
			cfg := nativeFakeConfig(t, mode)
			cfg.MaxRestarts = 1
			s, e := OpenNativeSession(context.Background(), cfg)
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			b, m := nativeTestInput(t)
			for i := 0; i < 2; i++ {
				if p, e := s.DecodeFrame(b, m); !errors.Is(e, ErrNativeHelperProtocol) || p != nil {
					t.Fatalf("pixels %d err %v", len(p), e)
				}
			}
			if p, e := s.DecodeFrame(b, m); !errors.Is(e, ErrNativeHelperUnavailable) || p != nil {
				t.Fatalf("restart limit: pixels %d err %v", len(p), e)
			}
			if stats := s.Stats(); stats.Starts != 2 || stats.PID != 0 {
				t.Fatal(stats)
			}
		})
	}
}
func TestNativeSessionHandshakeLimitsAndAdmission(t *testing.T) {
	if s, e := OpenNativeSession(context.Background(), nativeFakeConfig(t, "version")); !errors.Is(e, ErrNativeHelperProtocol) || s != nil {
		t.Fatalf("version s=%v e=%v", s, e)
	}
	cfg := nativeFakeConfig(t, "ok")
	cfg.MaxEncodedBytes = nativeMaximumBytes + 1
	if _, e := OpenNativeSession(context.Background(), cfg); !errors.Is(e, ErrNativeHelperLimit) {
		t.Fatal(e)
	}
	cfg = nativeFakeConfig(t, "ok")
	denied := errors.New("admission denied")
	cfg.Admit = func(context.Context, int64) (func(), error) { return nil, denied }
	s, e := OpenNativeSession(context.Background(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	b, m := nativeTestInput(t)
	if p, e := s.DecodeFrame(b, m); !errors.Is(e, denied) || p != nil {
		t.Fatalf("admission: %v", e)
	}
	m.PixelRepresentation = 1
	if p, e := s.DecodeFrame(b, m); !errors.Is(e, ErrUnsupportedMetadata) || p != nil {
		t.Fatalf("signed: %v", e)
	}
	ctx, c := context.WithCancel(context.Background())
	c()
	if p, e := s.DecodeFrameContext(ctx, b, m); !errors.Is(e, context.Canceled) || p != nil {
		t.Fatalf("pre-cancel: %v", e)
	}
}
func TestNativeSessionTimeoutAndConcurrentClose(t *testing.T) {
	cfg := nativeFakeConfig(t, "hang")
	s, e := OpenNativeSession(context.Background(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	// Use the short deadline for decoding rather than process startup.
	s.cfg.Timeout = 40 * time.Millisecond
	b, m := nativeTestInput(t)
	if p, e := s.DecodeFrame(b, m); !errors.Is(e, ErrDecoderTimeout) || p != nil {
		t.Fatalf("timeout: %v", e)
	}
	cfg = nativeFakeConfig(t, "hang")
	s, e = OpenNativeSession(context.Background(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	result := make(chan error, 1)
	go func() { _, e := s.DecodeFrame(b, m); result <- e }()
	// Close may race with either queued admission or in-flight I/O.
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	if e = <-result; !errors.Is(e, ErrNativeHelperUnavailable) {
		t.Fatal(e)
	}
}

func TestNativeSessionAdmissionTimeoutAndRelease(t *testing.T) {
	cfg := nativeFakeConfig(t, "ok")
	cfg.Admit = func(ctx context.Context, n int64) (func(), error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	s, e := OpenNativeSession(context.Background(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	// Use the short deadline for decoding rather than process startup.
	s.cfg.Timeout = 40 * time.Millisecond
	b, m := nativeTestInput(t)
	if p, e := s.DecodeFrame(b, m); !errors.Is(e, ErrDecoderTimeout) || p != nil {
		t.Fatalf("admission timeout: %v", e)
	}
	if stats := s.Stats(); stats.Starts != 1 || stats.Frames != 0 || stats.PID == 0 {
		t.Fatal(stats)
	}
	_ = s.Close()
	released := 0
	cfg = nativeFakeConfig(t, "status")
	cfg.Admit = func(ctx context.Context, n int64) (func(), error) {
		if n != int64(len(b))+int64(m.Rows)*int64(m.Columns) {
			t.Fatalf("charge %d", n)
		}
		return func() { released++ }, nil
	}
	s, e = OpenNativeSession(context.Background(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	for i := 0; i < 2; i++ {
		if p, e := s.DecodeFrame(b, m); !errors.Is(e, ErrUnsupportedMetadata) || p != nil {
			t.Fatalf("status: %v", e)
		}
	}
	if released != 2 || s.Stats().Starts != 1 || s.Stats().Failures != 0 {
		t.Fatalf("release/reuse: %d %+v", released, s.Stats())
	}
}

func TestNativeSessionQueuedDeadlineAndHandshakeTimeout(t *testing.T) {
	cfg := nativeFakeConfig(t, "hello-hang")
	cfg.Timeout = 40 * time.Millisecond
	if s, e := OpenNativeSession(context.Background(), cfg); s != nil || !errors.Is(e, ErrDecoderTimeout) {
		t.Fatalf("handshake timeout: %v", e)
	}
	cfg = nativeFakeConfig(t, "hang")
	s, e := OpenNativeSession(context.Background(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	b, m := nativeTestInput(t)
	// Hold admission before the pipe exchange so the second call is queued.
	entered := make(chan struct{})
	release := make(chan struct{})
	s.cfg.Admit = func(ctx context.Context, n int64) (func(), error) { close(entered); <-release; return nil, nil }
	first := make(chan error, 1)
	go func() { _, e := s.DecodeFrame(b, m); first <- e }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if p, e := s.DecodeFrameContext(ctx, b, m); p != nil || !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("queued: %v", e)
	}
	if s.Stats().PID == 0 {
		t.Fatal("queued cancellation killed active worker")
	}
	_ = s.Close()
	close(release)
	if e := <-first; !errors.Is(e, ErrNativeHelperUnavailable) {
		t.Fatal(e)
	}
}
