package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func durableTestOptions(t *testing.T) DurableOptions {
	t.Helper()
	return DurableOptions{Directory: filepath.Join(t.TempDir(), "spool"), Destinations: []AuditDestination{{ID: "primary", Network: SyslogTCP, Address: "127.0.0.1:1"}}, MaxRecordBytes: 4096, MaxBytes: 32768, MaxMessages: 8, RetryInterval: 10 * time.Millisecond, SendTimeout: 100 * time.Millisecond, SourceIdentity: "synthetic-catalog"}
}
func openDurableTest(t *testing.T, o DurableOptions) *DurableLogger {
	t.Helper()
	l, err := OpenDurableLogger(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

// auditConditionTimeout only bounds a condition that will never complete:
// each awaited condition is the completion signal itself, so a generous bound
// lets a loaded machine finish without hiding a lost record or attempt.
const auditConditionTimeout = 30 * time.Second

// awaitAudit polls observe until it reports the condition complete. On
// timeout it names the condition and the last observed state.
func awaitAudit(t *testing.T, condition string, observe func() (state any, complete bool)) {
	t.Helper()
	start := time.Now()
	for {
		state, complete := observe()
		if complete {
			return
		}
		if time.Since(start) >= auditConditionTimeout {
			t.Fatalf("audit condition %q did not complete within %v; observed %+v", condition, auditConditionTimeout, state)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func startAuditReceiver(t *testing.T, listener net.Listener) (chan SyslogRecord, *Receiver) {
	t.Helper()
	records := make(chan SyslogRecord, 32)
	receiver, err := NewReceiver(ReceiverOptions{Handle: func(ctx context.Context, r SyslogRecord) error {
		select {
		case records <- r:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- receiver.Serve(ctx, listener) }()
	t.Cleanup(func() { cancel(); <-done })
	return records, receiver
}

func TestDurableTwoDestinationsCheckpointIndependentlyAcrossRestart(t *testing.T) {
	a, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	recordsA, receiverA := startAuditReceiver(t, a)
	b, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addressB := b.Addr().String()
	b.Close()
	o := durableTestOptions(t)
	o.Destinations = []AuditDestination{{ID: "a", Network: SyslogTCP, Address: a.Addr().String()}, {ID: "b", Network: SyslogTCP, Address: addressB}}
	l := openDurableTest(t, o)
	payload := []byte("<AuditMessage>controlled</AuditMessage>")
	if err = l.LogRawSource(context.Background(), 7, 5, time.Now(), payload); err != nil {
		t.Fatal(err)
	}
	payload[0] = 'X'
	if err = l.Flush(context.Background()); err == nil {
		t.Fatal("offline repository unexpectedly succeeded")
	}
	awaitAudit(t, "repository a received the record", func() (any, bool) {
		s := receiverA.Stats()
		return s, s.Received == 1
	})
	first := <-recordsA
	if !strings.HasPrefix(string(first.Message), "<AuditMessage>") {
		t.Fatal("input bytes aliased durable record")
	}
	s := l.Stats()
	if s.Destinations["a"] != 0 || s.Destinations["b"] != 1 || s.Pending != 1 || s.SourceCursor != 7 {
		t.Fatalf("checkpoint state: %+v", s)
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	b, err = net.Listen("tcp", addressB)
	if err != nil {
		t.Fatal(err)
	}
	recordsB, receiverB := startAuditReceiver(t, b)
	l = openDurableTest(t, o)
	if err = l.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	awaitAudit(t, "repository b received the backlog after restart", func() (any, bool) {
		s := receiverB.Stats()
		return s, s.Received == 1
	})
	second := <-recordsB
	if string(first.Raw) != string(second.Raw) {
		t.Fatal("repositories received different immutable bytes")
	}
	if receiverA.Stats().Received != 1 || l.Stats().Pending != 0 {
		t.Fatal("successful repository was resent or spool retained")
	}
	if err = l.LogRawSource(context.Background(), 7, 5, time.Now(), []byte("retired")); err != nil {
		t.Fatal(err)
	}
	if l.Stats().Pending != 0 {
		t.Fatal("source resurrected after all records GC")
	}
}

func TestDurableAdmissionExpiryPauseAndDestinationIdentity(t *testing.T) {
	o := durableTestOptions(t)
	o.MaxMessages = 1
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	o.Now = func() time.Time { return at }
	o.MaxAge = time.Hour
	l := openDurableTest(t, o)
	if err := l.LogRawSource(context.Background(), 1, 5, at, []byte("synthetic")); err != nil {
		t.Fatal(err)
	}
	if err := l.LogRaw(5, at, []byte("second")); !errors.Is(err, ErrSpoolFull) {
		t.Fatalf("full: %v", err)
	}
	if l.Stats().Pending != 1 {
		t.Fatal("full queue evicted an accepted record")
	}
	l.Close()
	changed := o
	changed.Destinations = append([]AuditDestination(nil), o.Destinations...)
	changed.Destinations[0].Address = "127.0.0.1:2"
	if _, err := OpenDurableLogger(changed); err == nil || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("redirect accepted or address leaked: %v", err)
	}
	paused := o
	paused.Destinations = nil
	l = openDurableTest(t, paused)
	if l.Stats().Destinations["primary"] != 1 {
		t.Fatal("removed destination lost backlog")
	}
	if err := l.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l.Stats().Pending != 1 {
		t.Fatal("paused backlog sent")
	}
	l.Close()
	at = at.Add(2 * time.Hour)
	l = openDurableTest(t, changed)
	if s := l.Stats(); s.Pending != 0 || s.Expired != 1 || s.SourceCursor != 1 {
		t.Fatalf("expiry cursor: %+v", s)
	}
	if err := l.LogRawSource(context.Background(), 1, 5, at, []byte("old")); err != nil {
		t.Fatal(err)
	}
	if l.Stats().Pending != 0 {
		t.Fatal("expired source resurrected")
	}
}

func TestDurableDiskFailureCorruptionAndLock(t *testing.T) {
	for _, stage := range []string{"record_sync", "state_sync", "state_rename", "dir_sync"} {
		t.Run(stage, func(t *testing.T) {
			o := durableTestOptions(t)
			l := openDurableTest(t, o)
			l.options.Fault = func(s string) error {
				if s == stage {
					return errors.New("SYNTHETIC-PATIENT destination.invalid:6514")
				}
				return nil
			}
			err := l.LogRaw(5, time.Now(), []byte("SYNTHETIC-PATIENT"))
			if err == nil || strings.Contains(err.Error(), "SYNTHETIC") || strings.Contains(err.Error(), "destination.invalid") {
				t.Fatalf("unsanitized failure: %v", err)
			}
			if l.Stats().PersistenceFailures == 0 {
				t.Fatal("failure not counted")
			}
			if err = l.LogRaw(5, time.Now(), []byte("again")); err == nil {
				t.Fatal("uncertain disk state continued")
			}
			l.Close()
			l = openDurableTest(t, o)
			if l.Stats().Pending > 1 {
				t.Fatal("recovery duplicated an uncertain record")
			}
		})
	}
	o := durableTestOptions(t)
	l := openDurableTest(t, o)
	if _, err := OpenDurableLogger(o); err == nil {
		t.Fatal("second writer acquired queue")
	}
	if err := l.LogRaw(5, time.Now(), []byte("controlled")); err != nil {
		t.Fatal(err)
	}
	path := l.recordPath(1)
	l.Close()
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteAt([]byte("X"), 0)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = OpenDurableLogger(o); !errors.Is(err, ErrSpoolCorrupt) {
		t.Fatalf("corrupt header accepted: %v", err)
	}
}

func TestDurableRejectsUnsafeFilesAndUnboundedTemporaryRecovery(t *testing.T) {
	for _, kind := range []string{"permissions", "symlink", "entry_limit", "state"} {
		t.Run(kind, func(t *testing.T) {
			o := durableTestOptions(t)
			l := openDurableTest(t, o)
			l.Close()
			switch kind {
			case "permissions":
				if err := makeSpoolDirectoryPublic(o.Directory); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				os.Symlink("state.json", filepath.Join(o.Directory, "00000000000000000001.msg"))
			case "entry_limit":
				for i := 0; i < 2*o.MaxMessages+32; i++ {
					f, err := os.CreateTemp(o.Directory, ".tmp-")
					if err != nil {
						t.Fatal(err)
					}
					f.Close()
				}
			case "state":
				os.WriteFile(filepath.Join(o.Directory, "state.json"), []byte("{}"), 0600)
			}
			if logger, err := OpenDurableLogger(o); err == nil {
				logger.Close()
				t.Fatal("unsafe recovery accepted")
			}
		})
	}
}

func TestDurableCancelInterruptsBlockedSocketAndBackoff(t *testing.T) {
	o := durableTestOptions(t)
	o.SendTimeout = time.Minute
	var attempts atomic.Int32
	var peers []net.Conn
	o.Destinations[0].Dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		attempts.Add(1)
		client, server := net.Pipe()
		peers = append(peers, server)
		return client, nil
	}
	l := openDurableTest(t, o)
	if err := l.LogRaw(5, time.Now(), []byte("controlled")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	awaitAudit(t, "Run dialed the blocking repository once", func() (any, bool) {
		n := attempts.Load()
		return fmt.Sprintf("attempts=%d", n), n == 1
	})
	start := time.Now()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancel waited for write timeout")
	}
	for _, p := range peers {
		p.Close()
	}
	if l.Stats().Pending != 1 {
		t.Fatal("cancel lost pending record")
	}
	// An enqueue wake must not bypass failed repository backoff. The backoff
	// outlasts the test, so a second attempt can only come from the wake:
	// a loaded machine that is slow to observe the first attempt or to
	// return from the sleep cannot see the legitimate retry instead.
	o = durableTestOptions(t)
	o.RetryInterval = time.Minute
	o.Destinations[0].Dial = func(context.Context, string, string) (net.Conn, error) {
		attempts.Add(1)
		return nil, errors.New("offline")
	}
	attempts.Store(0)
	l = openDurableTest(t, o)
	l.LogRaw(5, time.Now(), []byte("one"))
	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	awaitAudit(t, "Run made the first failed attempt", func() (any, bool) {
		n := attempts.Load()
		return fmt.Sprintf("attempts=%d", n), n >= 1
	})
	if attempts.Load() != 1 {
		t.Fatal("the first failed attempt was retried before its backoff")
	}
	l.LogRaw(5, time.Now(), []byte("two"))
	time.Sleep(150 * time.Millisecond)
	if attempts.Load() != 1 {
		t.Fatal("enqueue bypassed retry backoff")
	}
	cancel()
	<-done
}

func TestDurableInputLimitsAndContextBeforeAdmission(t *testing.T) {
	o := durableTestOptions(t)
	o.MaxRecordBytes = 256
	l := openDurableTest(t, o)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.LogRawSource(ctx, 1, 5, time.Now(), []byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := l.LogRaw(5, time.Now(), make([]byte, 257)); !errors.Is(err, ErrSpoolFull) {
		t.Fatal(err)
	}
	if err := l.Log(atnaMessages()["query"]); !errors.Is(err, ErrSpoolFull) {
		t.Fatalf("bounded XML: %v", err)
	}
	if l.Stats().Pending != 0 {
		t.Fatal("refused input allocated a spool record")
	}
}

func TestDurableCloseCancelsAndDrainsPublicFlushBlockedDial(t *testing.T) {
	o := durableTestOptions(t)
	entered, release := make(chan struct{}), make(chan struct{})
	client, server := net.Pipe()
	defer server.Close()
	var canceled <-chan struct{}
	o.Destinations[0].Dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		canceled = ctx.Done()
		close(entered)
		<-release
		return client, nil
	}
	l := openDurableTest(t, o)
	if err := l.LogRaw(5, time.Now(), []byte("controlled")); err != nil {
		t.Fatal(err)
	}
	flushed := make(chan error, 1)
	go func() { flushed <- l.Flush(context.Background()) }()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- l.Close() }()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("Close failed to cancel public Flush dial")
	}
	close(release)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close failed to drain canceled Flush")
	}
	if err := <-flushed; err == nil {
		t.Fatal("post-Close flush succeeded")
	}
	server.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	if n, err := server.Read(b[:]); n != 0 || err == nil {
		t.Fatal("socket leaked or sent after Close")
	}
	s := l.Stats()
	if s.Sent != 0 || s.Pending != 1 {
		t.Fatal("Close checkpointed or dropped pending record")
	}
}

func TestDurableTLSOwnsPoolsAndSlicesAndRequiresCustomPolicyDigest(t *testing.T) {
	certificate, pool := testCertificate(t)
	second, pool2 := testCertificate(t)
	if !bytes.Equal(pool.Subjects()[0], pool2.Subjects()[0]) {
		t.Fatal("fixture roots need same subject")
	}
	config := &tls.Config{RootCAs: pool, ClientCAs: pool, NextProtos: []string{"controlled"}, CipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256}, CurvePreferences: []tls.CurveID{tls.CurveP256}, Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	o := durableTestOptions(t)
	o.Destinations[0].Network = SyslogTLS
	o.Destinations[0].TLS = config
	if _, err := OpenDurableLogger(o); err == nil {
		t.Fatal("custom pool without policy digest accepted")
	}
	hash := sha256.Sum256(certificate.Certificate[0])
	o.Destinations[0].Fingerprint = hex.EncodeToString(hash[:])
	l := openDurableTest(t, o)
	owned := l.options.Destinations[0].TLS
	before := bytes.Clone(owned.Certificates[0].Certificate[0])
	parsed, err := x509.ParseCertificate(second.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	pool.AddCert(parsed)
	config.NextProtos[0] = "mutated"
	config.CipherSuites[0] = 0
	config.CurvePreferences[0] = 0
	config.Certificates[0].Certificate[0][0] ^= 1
	if len(owned.RootCAs.Subjects()) != 1 || len(owned.ClientCAs.Subjects()) != 1 || owned.NextProtos[0] != "controlled" || owned.CipherSuites[0] == 0 || owned.CurvePreferences[0] == 0 || !bytes.Equal(owned.Certificates[0].Certificate[0], before) {
		t.Fatal("TLS data aliased caller mutation")
	}
	if err = l.LogRaw(5, time.Now(), []byte("controlled")); err != nil {
		t.Fatal(err)
	}
	l.Close()
	changed := o
	changed.Destinations = append([]AuditDestination(nil), o.Destinations...)
	changed.Destinations[0].TLS = &tls.Config{RootCAs: pool2, MinVersion: tls.VersionTLS12}
	hash = sha256.Sum256(second.Certificate[0])
	changed.Destinations[0].Fingerprint = hex.EncodeToString(hash[:])
	if _, err = OpenDurableLogger(changed); err == nil {
		t.Fatal("same-subject changed policy redirected pending records")
	}
}

func TestDurableENOSPCAndTruncatedOrExtraPayloadRefused(t *testing.T) {
	o := durableTestOptions(t)
	l := openDurableTest(t, o)
	l.options.Fault = func(stage string) error {
		if stage == "record_sync" {
			return syscall.ENOSPC
		}
		return nil
	}
	if err := l.LogRaw(5, time.Now(), []byte("controlled")); err == nil || strings.Contains(err.Error(), "space") {
		t.Fatal("disk-full error escaped sanitized boundary")
	}
	l.Close()
	for _, extra := range []bool{false, true} {
		t.Run(stringInt(intBool(extra)), func(t *testing.T) {
			o := durableTestOptions(t)
			l := openDurableTest(t, o)
			if err := l.LogRaw(5, time.Now(), []byte("controlled")); err != nil {
				t.Fatal(err)
			}
			path := l.recordPath(1)
			l.Close()
			if extra {
				f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
				if err != nil {
					t.Fatal(err)
				}
				f.Write([]byte("extra"))
				f.Close()
			} else {
				stat, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.Truncate(path, stat.Size()-1); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := OpenDurableLogger(o); !errors.Is(err, ErrSpoolCorrupt) {
				t.Fatal("length corruption accepted")
			}
		})
	}
}
func intBool(v bool) int {
	if v {
		return 1
	}
	return 0
}

func TestDurableOpenContextCancellationDoesNotReportCorruption(t *testing.T) {
	o := durableTestOptions(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := OpenDurableLoggerContext(ctx, o); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled open materialized spool or misclassified error")
	}
	ctx, cancel = context.WithCancel(context.Background())
	o.Fault = func(stage string) error {
		if stage == "state_sync" {
			cancel()
		}
		return nil
	}
	if _, err := OpenDurableLoggerContext(ctx, o); !errors.Is(err, context.Canceled) {
		t.Fatal("open lost cancellation at commit boundary")
	}
	o.Fault = nil
	l := openDurableTest(t, o)
	if l.Stats().Pending != 0 {
		t.Fatal("canceled constructor created a payload")
	}
}
