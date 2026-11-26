package storetranscode

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestPreparedSessionAggregateQuotaAcrossSources(t *testing.T) {
	session, source, binding, opts := preparedTestSource(t, testFile(), transfer.RLELossless, false)
	session.options.MaxRetainedBytes = opts.MaxSpoolBytes
	if _, err := source.Inspect(context.Background()); err != nil {
		t.Fatal(err)
	}
	retained := session.Stats().RetainedBytes
	if retained <= 0 {
		t.Fatal("missing aggregatecharge")
	}
	// A second source's preparation sees only remaining aggregate capacity.
	second, err := session.NewSource(dimse.NewFileStoreSource(testFile(), 0), opts, binding)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = second.Inspect(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats := session.Stats()
	if stats.RetainedBytes <= retained || stats.RetainedBytes+stats.ReservedBytes > session.options.MaxRetainedBytes || stats.PeakBytes > session.options.MaxRetainedBytes {
		t.Fatal("aggregate quota exceeded", stats)
	}
	session.mu.Lock()
	session.options.MaxRetainedBytes = stats.RetainedBytes
	session.mu.Unlock()
	third, err := session.NewSource(dimse.NewFileStoreSource(testFile(), 0), opts, binding)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = third.Inspect(context.Background()); !errors.Is(err, ErrResourceLimit) {
		t.Fatal("full retainedquota bypass", err)
	}
	if err := source.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if session.Stats().RetainedBytes != stats.RetainedBytes-retained {
		t.Fatal("shared readers or close misaccounted")
	}
}

type preparedBlockingWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *preparedBlockingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}
func TestPreparedSessionCloseRacesWriterAndLease(t *testing.T) {
	session, source, _, opts := preparedTestSource(t, testFile(), transfer.RLELossless, false)
	opened, err := source.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writer := &preparedBlockingWriter{entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- opened.WriteDataSet(context.Background(), writer, opts.Target) }()
	select {
	case <-writer.entered:
	case <-time.After(time.Second):
		t.Fatal("writer never entered")
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := session.Close(closeCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("close falsely succeeded through blockedrealI/O", err)
	}
	if session.Stats().RetainedBytes == 0 {
		t.Fatal("released spool before actualwriter exit")
	}
	if _, err := source.Open(context.Background()); !errors.Is(err, ErrPreparedClosed) {
		t.Fatal("open afterclosing", err)
	}
	close(writer.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("writer not drained")
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stats := session.Stats(); stats.RetainedBytes != 0 || stats.ReservedBytes != 0 || stats.Entries != 0 {
		t.Fatal("cleanup quota not zero", stats)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	if err := opened.WriteDataSet(context.Background(), io.Discard, opts.Target); !errors.Is(err, ErrPreparedClosed) {
		t.Fatal("revoked handle writes", err)
	}
}
func TestPreparedSessionCancellationAndIndependentHandleClose(t *testing.T) {
	session, source, binding, opts := preparedTestSource(t, testFile(), transfer.RLELossless, false)
	a, err := source.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := source.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	calls := binding.EncoderCalls()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	_ = encodeSource(t, b, opts.Target)
	if session.Stats().RetainedBytes == 0 {
		t.Fatal("firstlease close removedother'sspool")
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if binding.EncoderCalls() != calls {
		t.Fatal("lease close reencoded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := source.Open(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("canceledopen succeeded", err)
	}
}

type preparedBlockingSource struct {
	base             dimse.StoreSource
	entered, release chan struct{}
	once             sync.Once
}

func (s *preparedBlockingSource) Inspect(ctx context.Context) (dimse.StoreDescriptor, error) {
	return s.base.Inspect(ctx)
}
func (s *preparedBlockingSource) Open(ctx context.Context) (dimse.OpenedStoreSource, error) {
	opened, err := s.base.Open(ctx)
	if err != nil {
		return opened, err
	}
	write := opened.WriteDataSet
	opened.WriteDataSet = func(ctx context.Context, w io.Writer, syntax transfer.Syntax) error {
		s.once.Do(func() { close(s.entered) })
		<-s.release
		return write(ctx, w, syntax)
	}
	return opened, nil
}
func TestPreparedSessionCloseRacesPreparation(t *testing.T) {
	binding, err := NewBuiltinPreparedBinding(BuiltinPreparedBindingOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer binding.Close()
	session, err := NewPreparedSession(context.Background(), PreparedSessionOptions{MaxRetainedBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	base := &preparedBlockingSource{base: dimse.NewFileStoreSource(testFile(), 0), entered: make(chan struct{}), release: make(chan struct{})}
	opts := Options{Target: transfer.RLELossless, SpoolDirectory: t.TempDir(), MaxSpoolBytes: 1 << 20}
	source, err := session.NewSource(base, opts, binding)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := source.Inspect(context.Background()); done <- err }()
	select {
	case <-base.entered:
	case <-time.After(time.Second):
		t.Fatal("prepare notentered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	if err := session.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("close falsely joined blockedpreparer", err)
	}
	cancel()
	if session.Stats().ReservedBytes == 0 {
		t.Fatal("reservation releasedbefore actualprepareexit")
	}
	close(base.release)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceledprepare published")
		}
	case <-time.After(time.Second):
		t.Fatal("prepare notdrained")
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if session.Stats().ReservedBytes != 0 {
		t.Fatal("prepare reservation leaked")
	}
	assertEmpty(t, opts.SpoolDirectory)
}

func TestPreparedSessionFailureDoesNotPublishOrRetainQuota(t *testing.T) {
	session, source, _, opts := preparedTestSource(t, testFile(), transfer.RLELossless, false)
	source.source.opts.MaxSpoolBytes = 1
	policy, err := preparedPolicyDigest(source.source.opts)
	if err != nil {
		t.Fatal(err)
	}
	source.policy = policy
	if _, err := source.Inspect(context.Background()); !errors.Is(err, ErrResourceLimit) {
		t.Fatal("bounded preparation failure not reported", err)
	}
	if stats := session.Stats(); stats.ReservedBytes != 0 || stats.RetainedBytes != 0 {
		t.Fatal("failedpreparation retainedquota", stats)
	}
	assertEmpty(t, opts.SpoolDirectory)
}

func TestPreparedSessionConcurrentPreparationUsesAggregateQuota(t *testing.T) {
	binding, err := NewBuiltinPreparedBinding(BuiltinPreparedBindingOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer binding.Close()
	session, err := NewPreparedSession(context.Background(), PreparedSessionOptions{MaxRetainedBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var once sync.Once
	defer func() {
		once.Do(func() { close(release) })
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := session.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	base := &preparedBlockingSource{base: dimse.NewFileStoreSource(testFile(), 0), entered: make(chan struct{}), release: release}
	opts := Options{Target: transfer.RLELossless, SpoolDirectory: t.TempDir(), MaxSpoolBytes: 1 << 20}
	first, err := session.NewSource(base, opts, binding)
	if err != nil {
		t.Fatal(err)
	}
	second, err := session.NewSource(dimse.NewFileStoreSource(testFile(), 0), opts, binding)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := first.Inspect(ctx); done <- err }()
	select {
	case <-base.entered:
	case <-ctx.Done():
		t.Fatal("preparer not entered")
	}
	if stats := session.Stats(); stats.ReservedBytes != 1<<20 || stats.RetainedBytes != 0 {
		t.Fatal("live reservation missing", stats)
	}
	if _, err := second.Inspect(ctx); !errors.Is(err, ErrResourceLimit) {
		t.Fatal("concurrent reservation bypassed aggregatequota", err)
	}
	if stats := session.Stats(); stats.PeakBytes > 1<<20 {
		t.Fatal("aggregatepeak exceeded", stats)
	}
	once.Do(func() { close(release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("preparer not drained")
	}
	if _, err := second.Inspect(ctx); err != nil {
		t.Fatal("remaining retainedcapacity not reusable", err)
	}
	if err := session.Close(ctx); err != nil {
		t.Fatal(err)
	}
	assertEmpty(t, opts.SpoolDirectory)
}

// The barrier delays publication of a real filesystem error, rather than
// inventing a removal failure. The second Close must share this pending result.
func TestPreparedCleanupRetryAfterRealRemovalFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory write permissions required for causal removal failure")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the POSIX removal failure prerequisite")
	}
	session, source, _, _ := preparedTestSource(t, testFile(), transfer.RLELossless, false)
	if _, err := source.Inspect(context.Background()); err != nil {
		t.Fatal(err)
	}
	dir := source.cache.dir
	retained := session.Stats().RetainedBytes
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer func() { _ = os.Chmod(dir, 0700); once.Do(func() { close(release) }) }()
	var calls int
	var callsMu sync.Mutex
	source.removePrepared = func(spool *prepared) error {
		callsMu.Lock()
		calls++
		n := calls
		callsMu.Unlock()
		err := spool.close()
		if n == 1 {
			if err == nil {
				return errors.New("real filesystem failure prerequisite absent")
			}
			close(entered)
			<-release
		}
		return err
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- source.Close(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("real failedremove not reached")
	}
	source.mu.Lock()
	firstAttempt := source.cleanupAttempt
	source.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	if err := source.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("concurrent Close bypassed in-flight removal", err)
	}
	cancel()
	source.mu.Lock()
	sameAttempt := source.cleanupAttempt == firstAttempt
	source.mu.Unlock()
	if !sameAttempt {
		t.Fatal("concurrent Close replaced in-flight attempt")
	}
	if stats := session.Stats(); stats.RetainedBytes != retained || stats.Entries != 1 {
		t.Fatal("failedrealremoval prematurely uncharged", stats)
	}
	once.Do(func() { close(release) })
	select {
	case err := <-firstDone:
		if err == nil {
			t.Fatal("failedremove returned success")
		}
	case <-time.After(time.Second):
		t.Fatal("failedattempt not drained")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// A subsequent session Close retries the failed source result. Concurrent
	// callers must either share that attempt or observe its completed success.
	done := make(chan error, 2)
	for range 2 {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			done <- session.Close(ctx)
		}()
	}
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("retry not drained")
		}
	}
	if stats := session.Stats(); stats.RetainedBytes != 0 || stats.ReservedBytes != 0 || stats.Entries != 0 || !stats.Closed {
		t.Fatal("recovery not fully drained", stats)
	}
	callsMu.Lock()
	n := calls
	callsMu.Unlock()
	if n != 2 {
		t.Fatal("parallel/duplicate removal attempts", n)
	}
	// The earlier result remains a failure after successful retry.
	if err := awaitPreparedCleanup(context.Background(), firstAttempt); err == nil {
		t.Fatal("retry rewrote prior result")
	}
	if err := source.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("retained directory survived recovery", err)
	}
}
func TestPreparedCleanupPartialSuccessNotReleasedTwice(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory write permissions required for causal removal failure")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the POSIX removal failure prerequisite")
	}
	session, source, _, opts := preparedTestSource(t, testFile(), transfer.RLELossless, false)
	if _, err := source.Inspect(context.Background()); err != nil {
		t.Fatal(err)
	}
	cacheDir := source.cache.dir
	failedDir, err := os.MkdirTemp(opts.SpoolDirectory, "failed-real-spool-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(failedDir, "pending"), []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(failedDir, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(failedDir, 0700)
	reserved, err := session.reserve(1024)
	if err != nil {
		t.Fatal(err)
	}
	err = source.failPreparation(&prepared{dir: failedDir}, reserved, ErrPreparation)
	if err == nil || session.Stats().ReservedBytes != reserved {
		t.Fatal("failedremove did not retain preparation charge", err, session.Stats())
	}
	if err := source.Close(context.Background()); err == nil {
		t.Fatal("partially failed cleanup succeeded")
	}
	if _, err := os.Stat(cacheDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("successful cache removal missing", err)
	}
	if stats := session.Stats(); stats.RetainedBytes != 0 || stats.ReservedBytes != reserved || stats.Entries != 1 {
		t.Fatal("partialcleanup charged incorrectly", stats)
	}
	if err := os.Chmod(failedDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stats := session.Stats(); stats.RetainedBytes != 0 || stats.ReservedBytes != 0 || stats.Entries != 0 {
		t.Fatal("retry double-released or leaked", stats)
	}
	assertEmpty(t, opts.SpoolDirectory)
}
