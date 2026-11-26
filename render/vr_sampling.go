package render

import (
	"errors"
	"fmt"
	"math"
)

// DefaultVRMaxSteps bounds primary ray-march iterations, independently of the
// physical sample distance. Lighting and surface bisection have separate limits.
const DefaultVRMaxSteps = 4096

const DefaultVRSpacingFraction = 0.5

var (
	ErrInvalidVRSampling      = errors.New("render: invalid VR sampling")
	ErrVRSampleBudgetExceeded = errors.New("render: VR ray sample budget exceeded")
)

// VRSampleBudgetError describes a conservative iteration requirement. Increasing
// MaxSteps never changes sample distance; an insufficient budget rejects a frame.
type VRSampleBudgetError struct {
	RequiredSteps    int
	MaxSteps         int
	SampleDistanceMM float64
}

func (e *VRSampleBudgetError) Error() string {
	return fmt.Sprintf("%v: requires up to %d iterations at %.6g mm, limit %d", ErrVRSampleBudgetExceeded, e.RequiredSteps, e.SampleDistanceMM, e.MaxSteps)
}
func (e *VRSampleBudgetError) Unwrap() error { return ErrVRSampleBudgetExceeded }

// VRRaySamplingPlan is shared by CPU and GPU adapters. RequiredSteps includes
// the endpoint and, for adaptive sampling, rejected samples and refinement.
type VRRaySamplingPlan struct {
	SampleDistanceMM float64
	RequiredSteps    int
	MaxSteps         int
}

// ResolveVRRaySampling uses the regular prepared volume's physical spacing.
// Zero fraction/budget select 0.5/4096; negative or non-finite inputs fail.
// It never coarsens sampling to make a request fit its budget.
func ResolveVRRaySampling(spacingMM [3]float64, diagonalMM float64, quality VRQuality) (VRRaySamplingPlan, error) {
	fraction := quality.SpacingFraction
	if fraction == 0 {
		fraction = DefaultVRSpacingFraction
	}
	cap := quality.MaxSteps
	if cap == 0 {
		cap = DefaultVRMaxSteps
	}
	if !finite(fraction) || fraction <= 0 || cap < 1 || !finite(diagonalMM) || diagonalMM <= 0 {
		return VRRaySamplingPlan{}, ErrInvalidVRSampling
	}
	minimum := math.Inf(1)
	for _, spacing := range spacingMM {
		if !finite(spacing) || spacing <= 0 {
			return VRRaySamplingPlan{}, ErrInvalidVRSampling
		}
		minimum = math.Min(minimum, spacing)
	}
	step := minimum * fraction
	minimumStep, refinement := step, 1.0
	if quality.AdaptiveStep.Enabled() {
		minimumStep *= quality.AdaptiveStep.Normalized().Min
		refinement = 2
	}
	if !finite(step) || step <= 0 || !finite(minimumStep) || minimumStep <= 0 {
		return VRRaySamplingPlan{}, ErrInvalidVRSampling
	}
	required := math.Ceil(diagonalMM/minimumStep)*refinement + 1
	if !finite(required) || required >= float64(math.MaxInt) || required > float64(cap) {
		count := math.MaxInt
		if finite(required) && required < float64(math.MaxInt) {
			count = int(required)
		}
		return VRRaySamplingPlan{}, &VRSampleBudgetError{RequiredSteps: count, MaxSteps: cap, SampleDistanceMM: step}
	}
	return VRRaySamplingPlan{SampleDistanceMM: step, RequiredSteps: int(required), MaxSteps: cap}, nil
}

func volumeVRRaySampling(vol *Volume, quality VRQuality) (VRRaySamplingPlan, error) {
	return ResolveVRRaySampling([3]float64{vol.ColSpacing, vol.RowSpacing, vol.SliceSpacing}, vol.PhysicalSize().Length(), quality)
}

// A guard is owned by one ray. The preflight normally proves it sufficient;
// this runtime check also bounds non-advancing arithmetic and adaptive rollbacks.
type vrRayGuard struct {
	plan       VRRaySamplingPlan
	iterations int
	err        error
}

func (g *vrRayGuard) take() bool {
	if g == nil {
		return true
	}
	if g.iterations >= g.plan.MaxSteps {
		g.err = &VRSampleBudgetError{RequiredSteps: g.iterations + 1, MaxSteps: g.plan.MaxSteps, SampleDistanceMM: g.plan.SampleDistanceMM}
		return false
	}
	g.iterations++
	return true
}

func firstVRRayGuard(guards []*vrRayGuard) *vrRayGuard {
	if len(guards) == 0 {
		return nil
	}
	return guards[0]
}
