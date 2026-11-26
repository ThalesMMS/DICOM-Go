package imagefilter

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Common baseline/candidate measurement: prescribed delay, standard context,
// whole public call, no phase hooks. A completed call is a FAIL, not a sample
// to silently discard. Host scheduling is included in the observation.
func TestN41505MeasuredCancel(t *testing.T) {
	in := NewVolume([3]int{128, 96, 32}, [3]float64{0.7, 1.8, 4.2})
	for i := range in.Data {
		in.Data[i] = float32(100 + i%17)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requested := make(chan time.Time, 1)
	timer := time.AfterFunc(5*time.Millisecond, func() { at := time.Now(); cancel(); requested <- at })
	defer timer.Stop()
	started := time.Now()
	out, err := N4BiasFieldCorrection(ctx, in, N4Options{ShrinkFactor: 1, Iterations: []int{50, 50}, Workers: 2})
	ended := time.Now()
	if out != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel did not stop public call: result %v error %v", out, err)
	}
	at := <-requested
	t.Logf("N41505_CANCEL public_total_ns=%d requested_after_ns=%d cancel_to_return_ns=%d", ended.Sub(started).Nanoseconds(), at.Sub(started).Nanoseconds(), ended.Sub(at).Nanoseconds())
}
