package render

import (
	"context"
	"math"
	"slices"
	"testing"
	"time"
)

// TestVRInteractiveLODConvergesToTarget drives the controller with a cost
// model proportional to pixels × steps (a third on the interaction level) and
// checks it settles within the target, uses the interaction level only when
// the full volume cannot meet it, and returns to full preview quality once
// frames are cheap again.
func TestVRInteractiveLODConvergesToTarget(t *testing.T) {
	base := PreviewVRQuality(300, 300)
	for _, tc := range []struct {
		name         string
		fullCost     time.Duration // full-preview frame time on the full volume
		wantLowRes   bool
		wantFullSize bool
	}{
		{name: "fast", fullCost: 20 * time.Millisecond, wantFullSize: true},
		{name: "2x slow", fullCost: 100 * time.Millisecond},
		{name: "10x slow", fullCost: 500 * time.Millisecond, wantLowRes: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lod := NewVRInteractiveLOD(50 * time.Millisecond)
			fullCost := tc.fullCost
			var elapsed time.Duration
			frame := func() {
				q := lod.Quality(base)
				cost := float64(fullCost) * float64(q.Width*q.Height) / float64(base.Width*base.Height) * base.SpacingFraction / q.SpacingFraction
				if lod.LowResolution() {
					cost /= 3
				}
				elapsed = time.Duration(cost)
				lod.Observe(elapsed)
			}
			for i := 0; i < 20; i++ {
				frame()
			}
			if elapsed > lod.Target()*6/5 {
				t.Fatalf("settled frame %v exceeds target %v (scale %v)", elapsed, lod.Target(), lod.Scale())
			}
			if tc.wantFullSize && lod.Quality(base) != base {
				t.Fatalf("fast volume degraded to %+v", lod.Quality(base))
			}
			if !tc.wantFullSize && elapsed < lod.Target()/2 {
				t.Fatalf("settled frame %v wastes the target %v", elapsed, lod.Target())
			}
			if lod.LowResolution() != tc.wantLowRes {
				t.Fatalf("low resolution = %v, want %v (scale %v)", lod.LowResolution(), tc.wantLowRes, lod.Scale())
			}
			// Once frames are cheap again (a smaller view), quality returns.
			fullCost = 10 * time.Millisecond
			for i := 0; i < 20; i++ {
				frame()
			}
			if lod.LowResolution() || lod.Quality(base) != base {
				t.Fatalf("cheap frames kept LOD: low=%v scale=%v", lod.LowResolution(), lod.Scale())
			}
		})
	}
}

func TestVRInteractiveLODQualityBounds(t *testing.T) {
	lod := NewVRInteractiveLOD(time.Millisecond)
	for i := 0; i < 50; i++ {
		lod.Observe(time.Second)
	}
	if got := lod.Scale(); got != MinVRInteractiveScale {
		t.Fatalf("scale floor = %v", got)
	}
	q := lod.Quality(PreviewVRQuality(64, 20))
	factor := math.Cbrt(MinVRInteractiveScale)
	if q.Width != max(minVRInteractiveSide, int(math.Round(64*factor))) || q.Height != 20 ||
		q.MaxSteps != DefaultVRMaxSteps || math.Abs(q.SpacingFraction-2/factor) > 1e-12 {
		t.Fatalf("floor quality %+v", q)
	}
	lod.Reset()
	if lod.Scale() != 1 || lod.LowResolution() {
		t.Fatal("reset kept the LOD state")
	}
	var nilLOD *VRInteractiveLOD
	nilLOD.Observe(time.Second)
	if nilLOD.Quality(PreviewVRQuality(10, 10)) != PreviewVRQuality(10, 10) || nilLOD.LowResolution() {
		t.Fatal("nil controller changed quality")
	}
	if NewVRInteractiveLOD(0).Target() != DefaultVRInteractiveTarget {
		t.Fatal("default target")
	}
}

// TestVRInteractiveLODMeetsTargetOnLargeVolume renders a large volume's
// interaction frames on the CPU through the controller: the settled frames
// must be several times cheaper than the uncontrolled preview, and the
// settled full-quality frame is untouched by it. Frames are timed by the
// process CPU time they use, the work the controller trades away, so a
// loaded machine (whose wall time is mostly waiting for a core, worst for
// the short frames) does not change the comparison.
func TestVRInteractiveLODMeetsTargetOnLargeVolume(t *testing.T) {
	if testing.Short() {
		t.Skip("renders a 160³ volume")
	}
	const n = 160
	vol, err := BuildVolume(stack16(n, true, func(int) Rescale { return Rescale{Slope: 1, Intercept: -1024} }, func(x, y, z int) int {
		dx, dy, dz := x-n/2, y-n/2, z-n/2
		if dx*dx+dy*dy+dz*dz < (n/3)*(n/3) {
			return 1400 + (x+y+z)%50
		}
		return 900
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer vol.Close()
	cam := NewVRCamera(vol.BoundingRadiusMM())
	preset := DefaultVRPreset()
	window := WindowLevel{Center: 400, Width: 1000}
	ctx := context.Background()
	base := PreviewVRQuality(384, 384)
	render := func(q VRQuality, lowRes bool) time.Duration {
		target := vol
		if lowRes {
			target, err = vol.InteractiveVolumeContext(ctx)
			if err != nil {
				t.Fatal(err)
			}
		}
		started := time.Now()
		if _, err := RenderVRPreviewContext(ctx, target, cam, preset, window, nil, q); err != nil {
			t.Fatal(err)
		}
		return time.Since(started)
	}
	render(base, false) // warm the snapshot and the space-leap grid
	uncontrolled := min(render(base, false), render(base, false))
	lod := NewVRInteractiveLOD(uncontrolled / 8)
	// The interaction level is built outside the timed frames.
	if _, err := vol.InteractiveVolumeContext(ctx); err != nil {
		t.Fatal(err)
	}
	render(base, true)
	var settled []time.Duration
	for i := 0; i < 16; i++ {
		elapsed := render(lod.Quality(base), lod.LowResolution())
		lod.Observe(elapsed)
		if i >= 10 {
			settled = append(settled, elapsed)
		}
	}
	slices.Sort(settled)
	median := settled[len(settled)/2]
	t.Logf("uncontrolled %v, target %v, settled median %v, scale %.3f, low-res %v",
		uncontrolled, lod.Target(), median, lod.Scale(), lod.LowResolution())
	if median > uncontrolled/3 {
		t.Fatalf("settled interaction frames %v not reduced from %v toward %v", median, uncontrolled, lod.Target())
	}
	// Final frames do not consult the controller.
	full := DefaultVRQuality(64, 64)
	a := RenderVR(vol, cam, preset, window, true, nil, full)
	lod.Observe(time.Hour)
	b := RenderVR(vol, cam, preset, window, true, nil, full)
	if !sameImage(a, b) {
		t.Fatal("settled frame changed with the interaction controller")
	}
}

// TestVRInteractiveLODCountsUnmeasurableFramesAsFast: a frame the clock
// measures as zero (faster than its resolution, common on Windows) raises
// the scale like any fast frame; a negative time is ignored.
func TestVRInteractiveLODCountsUnmeasurableFramesAsFast(t *testing.T) {
	lod := NewVRInteractiveLOD(time.Second)
	for i := 0; i < 10; i++ {
		lod.Observe(time.Minute)
	}
	floor := lod.Scale()
	lod.Observe(-time.Millisecond)
	if lod.Scale() != floor {
		t.Fatalf("negative observation changed the scale to %v", lod.Scale())
	}
	lod.Observe(0)
	if lod.Scale() <= floor {
		t.Fatalf("zero-time frame left the scale at %v", lod.Scale())
	}
}
