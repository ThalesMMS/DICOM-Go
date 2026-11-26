package render

// VRQuality controls output size, physical ray sampling and its safety budget.
type VRQuality struct {
	Width, Height int
	// MaxSteps caps primary ray-march iterations. Zero selects 4096. An
	// insufficient budget returns ErrVRSampleBudgetExceeded, never a coarse ray.
	MaxSteps int
	// SpacingFraction multiplies the smallest regular-grid spacing in mm.
	// Zero selects 0.5; it is independent of volume extent and MaxSteps.
	SpacingFraction float64
	// Jitter offsets each pixel's first sample by VRRayJitter of a step
	// (VTK's UseJittering), turning the "wood-grain" rings of a step coarser
	// than the voxels into fine, deterministic noise.
	Jitter bool
	// Scattering allows the preset's volumetric scattering (#1064), including
	// interaction when the caller retains settled quality.
	Scattering bool
	// AdaptiveStep lengthens DVR steps across homogeneous regions and
	// shortens them at edges (#1231); the zero value keeps the fixed step.
	// The CPU ray caster honours it for DVR rays without segment labels or
	// a fused volume; projections, surfaces and the WebGPU worker march
	// the fixed step.
	AdaptiveStep VRAdaptiveStep
}

// DefaultVRQuality samples at half the smallest physical voxel spacing.
// Increasing the iteration budget does not change this distance.
func DefaultVRQuality(w, h int) VRQuality {
	return VRQuality{Width: w, Height: h, MaxSteps: DefaultVRMaxSteps, SpacingFraction: DefaultVRSpacingFraction, Jitter: true, Scattering: true}
}

// PreviewVRQuality explicitly uses a two-voxel step without jitter or scattering.
// Production controllers may retain DefaultVRQuality during motion.
func PreviewVRQuality(w, h int) VRQuality {
	return VRQuality{Width: w, Height: h, MaxSteps: DefaultVRMaxSteps, SpacingFraction: 2}
}

// AdaptivePreviewVRQuality samples edges at one voxel spacing and permits
// longer steps in homogeneous regions. It is explicitly a draft profile.
func AdaptivePreviewVRQuality(w, h int) VRQuality {
	q := PreviewVRQuality(w, h)
	q.SpacingFraction /= 2
	q.AdaptiveStep = DefaultVRAdaptiveStep()
	return q
}
