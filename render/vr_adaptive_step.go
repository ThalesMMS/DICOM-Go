package render

import "math"

// Gradient-adaptive sampling step for the CPU DVR ray caster (#1231). A ray
// lengthens its step across homogeneous regions and shortens it at edges:
// after each composited sample the change of the classified sample (the
// transfer function's opacity and opacity-weighted colour) since the previous
// one, normalized to one base step, selects the next step between Max and
// Min times the base step. A long step that lands on an edge is rejected and
// the ray resumes from the previous sample at the shortest step, so edges are
// never composited with the opacity of a long homogeneous interval. Each
// composited sample stands for the interval it closes (from the previous
// sample), and its opacity is corrected for that length with CorrectVRAlpha
// and the transfer function's OpacityUnitDistanceMM, as fixed steps are.
// Written for this renderer after the adaptive step of the author's Isis DICOM
// Viewer (volume_compute.metal); no code is copied.

// VRAdaptiveStep configures the gradient-adaptive step of DVR rays. The zero
// value disables it: every ray marches the physical step of VRQuality.SpacingFraction.
type VRAdaptiveStep struct {
	// Max is the longest step, in base steps, taken across homogeneous
	// regions; above 1 enables adaptive stepping.
	Max float64
	// Min is the shortest step, in base steps, taken at edges, in (0,1];
	// zero means 1.
	Min float64
	// Threshold is the change of the classified sample per base step (in
	// opacity units, [0,1]) at which the step reaches Min; zero means
	// DefaultVRAdaptiveStepThreshold.
	Threshold float64
}

// DefaultVRAdaptiveStepThreshold is the change of the classified sample per
// base step that selects the shortest step.
const DefaultVRAdaptiveStepThreshold = 0.02

// DefaultVRAdaptiveStep lengthens the physical step up to four times across
// homogeneous regions and keeps the base step at detected edges.
func DefaultVRAdaptiveStep() VRAdaptiveStep {
	return VRAdaptiveStep{Max: 4, Min: 1, Threshold: DefaultVRAdaptiveStepThreshold}
}

// Enabled reports whether the settings lengthen any step.
func (a VRAdaptiveStep) Enabled() bool {
	return finite(a.Max) && a.Max > 1
}

// Normalized clamps the settings to their domains: Max in [1, 16], Min in
// (0, 1] and a positive Threshold.
func (a VRAdaptiveStep) Normalized() VRAdaptiveStep {
	if !finite(a.Max) || a.Max < 1 {
		a.Max = 1
	}
	a.Max = math.Min(a.Max, 16)
	if !finite(a.Min) || a.Min <= 0 || a.Min > 1 {
		a.Min = 1
	}
	if !finite(a.Threshold) || a.Threshold <= 0 {
		a.Threshold = DefaultVRAdaptiveStepThreshold
	}
	return a
}

// vrAdaptiveStepper is a render worker's adaptive step state, reset at the
// start of each ray. A disabled stepper marches the base step. samples counts
// the classified samples, for tests and benchmarks.
type vrAdaptiveStepper struct {
	enabled   bool
	samples   int
	base      float64 // mm
	minStep   float64 // mm
	maxStep   float64 // mm
	threshold float64
	// The previous composited sample: its parameter and classification.
	previous    RGBA
	tPrevious   float64
	hasPrevious bool
	// refineUntil is the parameter of the last rejected sample: the ray
	// keeps the shortest step until it passes it, so one edge costs one
	// rejection.
	refineUntil float64
}

func newVRAdaptiveStepper(settings VRAdaptiveStep, base float64) *vrAdaptiveStepper {
	if !settings.Enabled() || !(base > 0) {
		return &vrAdaptiveStepper{base: base}
	}
	settings = settings.Normalized()
	return &vrAdaptiveStepper{
		enabled: true, base: base,
		minStep: base * settings.Min, maxStep: base * settings.Max,
		threshold: settings.Threshold,
	}
}

// beginRay starts a ray. A nil stepper is the fixed step.
func (s *vrAdaptiveStepper) beginRay() {
	if s != nil {
		s.hasPrevious = false
		s.refineUntil = math.Inf(-1)
	}
}

// reset forgets the previous sample after one that was not composited
// (clipped, skipped or outside the volume): the next step is the base step.
func (s *vrAdaptiveStepper) reset() {
	if s != nil {
		s.hasPrevious = false
	}
}

// vrClassifiedChange is the change between two classified samples: the
// opacity change, or a colour change weighted by the larger opacity.
func vrClassifiedChange(a, b RGBA) float64 {
	weight := math.Max(a.A, b.A)
	change := math.Abs(a.A - b.A)
	change = math.Max(change, weight*math.Abs(a.R-b.R))
	change = math.Max(change, weight*math.Abs(a.G-b.G))
	return math.Max(change, weight*math.Abs(a.B-b.B))
}

// observe decides on the sample classified rgba at parameter t. accept false
// rejects it (a long step landed on an edge): the march resumes from resume
// with the shortest step. Otherwise length is the interval the sample stands
// for, the distance from the previous composited sample (the base step for
// the first one), and next the step to the following sample.
func (s *vrAdaptiveStepper) observe(t float64, rgba RGBA) (accept bool, length, next, resume float64) {
	s.samples++
	if !s.enabled {
		return true, s.base, s.base, t
	}
	if !s.hasPrevious {
		s.previous, s.tPrevious, s.hasPrevious = rgba, t, true
		return true, s.base, s.base, t
	}
	taken := t - s.tPrevious
	if !(taken > 0) {
		taken = s.base
	}
	rate := vrClassifiedChange(s.previous, rgba) * s.base / taken
	fraction := clampUnit(rate / s.threshold)
	next = s.maxStep + (s.minStep-s.maxStep)*fraction
	if taken > s.base*(1+1e-9) && next <= s.base {
		// The long step crossed an edge: resample it at the shortest step.
		s.refineUntil = t
		return false, 0, s.minStep, s.tPrevious
	}
	if t < s.refineUntil {
		next = s.minStep
	}
	s.previous, s.tPrevious = rgba, t
	return true, taken, next, t
}
