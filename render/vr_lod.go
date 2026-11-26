package render

import (
	"math"
	"sync"
	"time"
)

// VRInteractiveLOD adapts interaction frames to a frame-time target (#1066),
// after vtkSmartVolumeMapper's InteractiveUpdateRate,
// InteractiveAdjustSampleDistances and LowResModeResample (written from their
// documented behaviour, no VTK code is copied). A cost scale in
// [MinVRInteractiveScale, 1] shrinks the preview's output side and march steps
// by its cube root each, so the ray-cast cost (pixels × steps) follows the
// scale; after every frame the scale moves by target/elapsed. When the steps
// fall well below one per voxel the interaction level of the volume (the 2×
// BinShrink of Volume.InteractiveVolumeContext) loses little more and is
// cheaper to sample, so LowResolution reports it until that level renders at
// full preview quality in half the target. Settled
// frames never consult the controller: final quality is unchanged. Safe for
// concurrent use.
type VRInteractiveLOD struct {
	mu     sync.Mutex
	target time.Duration
	scale  float64
	lowRes bool
}

const (
	// MinVRInteractiveScale bounds the cost scale: a sixteenth of the preview
	// cost, 40% of its side and steps.
	MinVRInteractiveScale = 1.0 / 16
	// vrLowResEnter is the scale below which the interaction level is used;
	// the full volume returns once the interaction level renders at full
	// preview quality in half the target, resuming at vrLowResResume.
	vrLowResEnter  = 0.25
	vrLowResResume = 0.5
	// vrLODMaxStep bounds one adjustment to a factor of two either way, so
	// a single slow frame (a cache miss, a GC pause) cannot collapse quality.
	vrLODMaxStep = 2
	// Minimum preview output side.
	minVRInteractiveSide = 32
)

// DefaultVRInteractiveTarget is the default interaction frame time, 20 frames
// per second.
const DefaultVRInteractiveTarget = 50 * time.Millisecond

// NewVRInteractiveLOD returns a controller for the target frame time
// (DefaultVRInteractiveTarget when target <= 0) starting at full preview
// quality.
func NewVRInteractiveLOD(target time.Duration) *VRInteractiveLOD {
	if target <= 0 {
		target = DefaultVRInteractiveTarget
	}
	return &VRInteractiveLOD{target: target, scale: 1}
}

// Target returns the frame-time target.
func (c *VRInteractiveLOD) Target() time.Duration {
	if c == nil {
		return 0
	}
	return c.target
}

// Scale returns the current cost scale.
func (c *VRInteractiveLOD) Scale() float64 {
	if c == nil {
		return 1
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.scale
}

// LowResolution reports whether the interaction level of the volume should
// be rendered.
func (c *VRInteractiveLOD) LowResolution() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lowRes
}

// Quality scales a preview quality by the current cost scale.
func (c *VRInteractiveLOD) Quality(base VRQuality) VRQuality {
	factor := math.Cbrt(c.Scale())
	scale := func(n, floor int) int {
		if n <= floor {
			return n
		}
		return max(floor, int(math.Round(float64(n)*factor)))
	}
	base.Width = scale(base.Width, minVRInteractiveSide)
	base.Height = scale(base.Height, minVRInteractiveSide)
	if base.SpacingFraction == 0 {
		base.SpacingFraction = DefaultVRSpacingFraction
	}
	base.SpacingFraction /= factor
	return base
}

// Observe records the render time of an interaction frame rendered with
// Quality and LowResolution, and adjusts the scale for the next one. A zero
// elapsed time is a frame faster than the clock resolves (Windows' monotonic
// clock advances in steps of up to a millisecond or more) and counts as the
// fastest possible frame, not as no observation.
func (c *VRInteractiveLOD) Observe(elapsed time.Duration) {
	if c == nil || elapsed < 0 {
		return
	}
	elapsed = max(elapsed, time.Nanosecond)
	c.mu.Lock()
	defer c.mu.Unlock()
	ratio := float64(c.target) / float64(elapsed)
	ratio = math.Max(1.0/vrLODMaxStep, math.Min(vrLODMaxStep, ratio))
	if c.lowRes && c.scale == 1 && ratio >= vrLODMaxStep {
		// The full volume costs more per sample: resume it below full
		// preview quality.
		c.lowRes = false
		c.scale = vrLowResResume
		return
	}
	c.scale = math.Max(MinVRInteractiveScale, math.Min(1, c.scale*ratio))
	if !c.lowRes && c.scale < vrLowResEnter {
		c.lowRes = true
	}
}

// Reset returns to full preview quality, for a new volume or view size.
func (c *VRInteractiveLOD) Reset() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.scale, c.lowRes = 1, false
}
