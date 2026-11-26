package pixeldata_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

func TestMemoryAdmissionGrantsInOrderWithinCapacity(t *testing.T) {
	admission, err := pixeldata.NewMemoryAdmission(100)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first, err := admission.Acquire(ctx, 60)
	if err != nil {
		t.Fatal(err)
	}
	// 50 does not fit beside 60 and waits; 10 would fit but queues behind
	// it, so the large request is not starved.
	granted := make(chan func(), 2)
	for i, n := range []int64{50, 10} {
		go func(n int64) {
			release, err := admission.Acquire(ctx, n)
			if err != nil {
				t.Error(err)
				return
			}
			granted <- release
		}(n)
		waitFor(t, func() bool { return admission.Stats().Queued == i+1 })
	}
	if stats := admission.Stats(); stats.Held != 60 || stats.Queued != 2 {
		t.Fatalf("before the release %+v", stats)
	}
	first()
	first() // releasing twice is harmless
	(<-granted)()
	(<-granted)()
	stats := admission.Stats()
	if stats.Held != 0 || stats.Granted != 3 || stats.Waited != 2 || stats.Peak != 60 || stats.Oversized != 0 {
		t.Fatalf("stats %+v", stats)
	}
}

func TestMemoryAdmissionGrantsOversizedRequestsAlone(t *testing.T) {
	admission, err := pixeldata.NewMemoryAdmission(100)
	if err != nil {
		t.Fatal(err)
	}
	small, err := admission.Acquire(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	granted := make(chan func())
	go func() {
		release, err := admission.Acquire(context.Background(), 250)
		if err != nil {
			t.Error(err)
		}
		granted <- release
	}()
	waitFor(t, func() bool { return admission.Stats().Queued == 1 })
	small()
	release := <-granted
	if stats := admission.Stats(); stats.Held != 250 || stats.Oversized != 1 || stats.Peak != 250 {
		t.Fatalf("oversized %+v", stats)
	}
	release()
}

func TestMemoryAdmissionCancellationLeavesTheQueue(t *testing.T) {
	admission, err := pixeldata.NewMemoryAdmission(10)
	if err != nil {
		t.Fatal(err)
	}
	held, err := admission.Acquire(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() {
		_, err := admission.Acquire(ctx, 5)
		done <- err
	}()
	waitFor(t, func() bool { return admission.Stats().Queued == 1 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait: %v", err)
	}
	held()
	if stats := admission.Stats(); stats.Held != 0 || stats.Queued != 0 || stats.Canceled != 1 {
		t.Fatalf("stats %+v", stats)
	}
	if _, err := admission.Acquire(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context: %v", err)
	}
	if _, err := admission.Acquire(context.Background(), 0); !errors.Is(err, pixeldata.ErrInvalidAdmission) {
		t.Fatalf("zero request: %v", err)
	}
	if _, err := pixeldata.NewMemoryAdmission(0); !errors.Is(err, pixeldata.ErrInvalidAdmission) {
		t.Fatalf("zero capacity: %v", err)
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}
