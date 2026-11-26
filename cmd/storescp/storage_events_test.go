package main

import (
	"context"
	"errors"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func testStorageRuntime(t *testing.T, args []string, deps storageRuntimeDependencies) *storescpStorageRuntime {
	t.Helper()
	p := layoutPolicy(t, args...)
	r, err := newStorescpStorageRuntimeWith(p, t.TempDir(), deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		if err := r.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return r
}
func prepareStorageTest(t *testing.T, r *storescpStorageRuntime, ae string) *storagePrepared {
	t.Helper()
	src := layoutData()
	p, err := r.prepare(context.Background(), src, &ul.Association{CalledAETitle: ae, CallingAETitle: "SENDER"}, dimse.CStoreRequest{AffectedSOPClassUID: "1.2.3", AffectedSOPInstanceUID: "1.2.3.4"})
	if err != nil {
		t.Fatal(err)
	}
	if err = p.format(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	return p
}
func runtimeHookArgs(t *testing.T) []string {
	t.Helper()
	path, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	return []string{"-exec-on-reception", storageHookJSON([]string{path, "${filename}"}), "-exec-on-eostudy", storageHookJSON([]string{path, "${generation}", "${count}", "${last_path}"}), "-eostudy-timeout", "1h"}
}
func TestStorageRuntimeFIFOStudyEndAndLateReceipt(t *testing.T) {
	start := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	var observed [][]string
	first := true
	now := time.Now()
	r := testStorageRuntime(t, runtimeHookArgs(t), storageRuntimeDependencies{manualPoll: true, now: func() time.Time { return now }, execute: func(ctx context.Context, argv []string, _ string) error {
		mu.Lock()
		isFirst := first
		first = false
		observed = append(observed, append([]string(nil), argv[1:]...))
		mu.Unlock()
		if isFirst {
			close(start)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			return errStorageHook
		}
		return nil
	}})
	p1 := prepareStorageTest(t, r, "MixedAE")
	p1.stored(filepath.Join(r.root, "one.dcm"))
	<-start
	p2 := prepareStorageTest(t, r, "MixedAE")
	p2.stored(filepath.Join(r.root, "two.dcm"))
	r.poll(now.Add(time.Hour))
	p3 := prepareStorageTest(t, r, "MixedAE")
	p3.stored(filepath.Join(r.root, "late.dcm"))
	p3Generation := p3.study.Generation()
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(observed) != 4 || observed[0][0] != "one.dcm" || observed[1][0] != "two.dcm" || observed[2][1] != "2" || observed[2][2] != filepath.Join(r.root, "two.dcm") || observed[3][0] != "late.dcm" {
		t.Fatalf("ordering %v", observed)
	}
	if p3Generation <= p1.study.Generation() {
		t.Fatal("late generation reused")
	}
	m := r.Metrics()
	if m.Completed != 3 || m.Failed != 1 || m.Cancelled != 1 || m.Slots != 0 || m.Bytes != 0 {
		t.Fatalf("accounting %+v", m)
	}
}
func TestStorageRuntimeReservationRollbackAndQueueCancellation(t *testing.T) {
	path, _ := filepath.Abs(os.Args[0])
	args := []string{"-exec-on-reception", storageHookJSON([]string{path, "${path}"}), "-hook-queue-depth", "1"}
	started := make(chan struct{})
	calls := 0
	r := testStorageRuntime(t, args, storageRuntimeDependencies{manualPoll: true, execute: func(ctx context.Context, _ []string, _ string) error {
		calls++
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}})
	p := prepareStorageTest(t, r, "AE")
	if _, err := r.prepare(context.Background(), layoutData(), &ul.Association{CalledAETitle: "AE"}, dimse.CStoreRequest{}); !errors.Is(err, errStorageEventLimit) {
		t.Fatalf("capacity accepted %v", err)
	}
	p.abort()
	p.abort()
	if m := r.Metrics(); m.Slots != 0 || m.Bytes != 0 {
		t.Fatalf("rollback %+v", m)
	}
	p = prepareStorageTest(t, r, "AE")
	p.stored(filepath.Join(r.root, "one.dcm"))
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls %d", calls)
	}
	if m := r.Metrics(); m.Cancelled != 1 || m.Slots != 0 || m.Bytes != 0 {
		t.Fatalf("cancel accounting %+v", m)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestStorageRuntimePrecommitByteAndStudyFailure(t *testing.T) {
	path, _ := filepath.Abs(os.Args[0])
	r := testStorageRuntime(t, []string{"-exec-on-reception", storageHookJSON([]string{path}), "-hook-max-bytes", "1"}, storageRuntimeDependencies{manualPoll: true})
	if p, err := r.prepare(context.Background(), layoutData(), &ul.Association{CalledAETitle: "AE"}, dimse.CStoreRequest{}); p != nil || !errors.Is(err, errStorageEventLimit) {
		t.Fatalf("byte admission %v/%v", p, err)
	}
	if m := r.Metrics(); m.Bytes != 0 || m.Slots != 0 {
		t.Fatal(m)
	}
	r = testStorageRuntime(t, runtimeHookArgs(t), storageRuntimeDependencies{manualPoll: true, execute: func(context.Context, []string, string) error { return nil }})
	p1 := prepareStorageTest(t, r, "AE")
	p2 := prepareStorageTest(t, r, "AE")
	p1.abort()
	if m := r.Metrics(); m.Slots != 2 {
		t.Fatalf("first abort lost future %+v", m)
	}
	p2.abort()
	if m := r.Metrics(); m.Slots != 0 || m.Bytes != 0 {
		t.Fatalf("last abort leak %+v", m)
	}
	// Cancelled/invalid metadata never triggers the runner or changes reservations.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.prepare(ctx, layoutData(), &ul.Association{}, dimse.CStoreRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.Metrics(), storageHookMetrics{}) {
		t.Fatal(r.Metrics())
	}
}

// The injected tick, acknowledged only after Poll, makes expiry independent of
// wall-clock sleeps. The timeout below is solely a deadlock/failure guard.
func TestStorageRuntimePollingIndependentOfBlockedFIFO(t *testing.T) {
	ticks := make(chan time.Time, 1)
	polled := make(chan struct{}, 1)
	blocked := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	var clockMu sync.Mutex
	clock := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock }
	var observedMu sync.Mutex
	var observed [][]string
	r := testStorageRuntime(t, runtimeHookArgs(t), storageRuntimeDependencies{
		now: now, pollTicks: ticks, pollObserved: func() { polled <- struct{}{} },
		execute: func(ctx context.Context, argv []string, _ string) error {
			observedMu.Lock()
			observed = append(observed, append([]string(nil), argv[1:]...))
			observedMu.Unlock()
			if argv[1] == "b1.dcm" {
				close(blocked)
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		},
	})
	prepare := func(study string) *storagePrepared {
		src := layoutData()
		src.Put(core.NewRawElement(core.NewTag(0x0020, 0x000D), core.VRUI, []byte(study)))
		p, err := r.prepare(context.Background(), src, &ul.Association{CalledAETitle: "MixedAE", CallingAETitle: "SENDER"}, dimse.CStoreRequest{AffectedSOPClassUID: "1.2.3", AffectedSOPInstanceUID: "1.2.3.4"})
		if err != nil {
			t.Fatal(err)
		}
		if err = p.format(context.Background(), src); err != nil {
			t.Fatal(err)
		}
		return p
	}
	a := prepare("1.2.3.51")
	a.stored(filepath.Join(r.root, "a1.dcm"))
	b := prepare("1.2.3.52")
	b.stored(filepath.Join(r.root, "b1.dcm"))
	select {
	case <-blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("worker never blocked")
	}
	b2 := prepare("1.2.3.52")
	b2.stored(filepath.Join(r.root, "b2.dcm"))
	pendingB := prepare("1.2.3.52")
	defer pendingB.abort()
	clockMu.Lock()
	clock = clock.Add(time.Hour)
	tick := clock
	clockMu.Unlock()
	ticks <- tick
	select {
	case <-polled:
	case <-time.After(time.Second):
		t.Fatal("inactivity polling waited for the blocked FIFO executor")
	}
	r.mu.Lock()
	retainedA := r.future[a.study.Generation()]
	queued := append([]storageHookEvent(nil), r.queue...)
	r.mu.Unlock()
	if retainedA || len(queued) != 2 || queued[0].argv[1] != "b2.dcm" || !queued[1].study || queued[1].argv[2] != "1" || queued[1].argv[3] != filepath.Join(r.root, "a1.dcm") {
		t.Fatalf("expiry/queued order retainedA=%v queue=%+v", retainedA, queued)
	}
	lateA := prepare("1.2.3.51")
	if !lateA.study.NewGeneration() || lateA.study.Generation() <= a.study.Generation() {
		t.Fatal("late receipt reused expired generation")
	}
	lateA.stored(filepath.Join(r.root, "a2.dcm"))
	pendingB.abort()
	// Unblock before normal shutdown; no hook is invoked concurrently by polling.
	release <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	observedMu.Lock()
	defer observedMu.Unlock()
	if len(observed) != 5 || observed[0][0] != "a1.dcm" || observed[1][0] != "b1.dcm" || observed[2][0] != "b2.dcm" || observed[3][1] != "1" || observed[3][2] != filepath.Join(r.root, "a1.dcm") || observed[4][0] != "a2.dcm" {
		t.Fatalf("FIFO attempts %v", observed)
	}
	if m := r.Metrics(); m.Completed != 5 || m.Failed != 0 || m.Cancelled != 2 || m.Slots != 0 || m.Bytes != 0 {
		t.Fatalf("joined accounting %+v", m)
	}
}

func TestStorageRuntimeCloseJoinsPollingCancellation(t *testing.T) {
	path, _ := filepath.Abs(os.Args[0])
	ticks := make(chan time.Time, 1)
	entered := make(chan struct{})
	var r *storescpStorageRuntime
	r = testStorageRuntime(t, []string{"-exec-on-eostudy", storageHookJSON([]string{path}), "-eostudy-timeout", "1h"}, storageRuntimeDependencies{
		pollTicks: ticks,
		pollObserved: func() {
			close(entered)
			<-r.ctx.Done()
		},
	})
	p := prepareStorageTest(t, r, "MixedAE")
	p.stored(filepath.Join(r.root, "one.dcm"))
	ticks <- time.Now()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("poller did not observe injected tick")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Close(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case <-r.pollDone:
	default:
		t.Fatal("Close returned before polling stopped")
	}
	if _, err := r.prepare(context.Background(), layoutData(), &ul.Association{CalledAETitle: "MixedAE"}, dimse.CStoreRequest{}); !errors.Is(err, errStorageEventLimit) {
		t.Fatalf("admission after Close: %v", err)
	}
	if m := r.Metrics(); m.Completed != 0 || m.Failed != 0 || m.Cancelled != 1 || m.Slots != 0 || m.Bytes != 0 {
		t.Fatalf("shutdown accounting %+v", m)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
