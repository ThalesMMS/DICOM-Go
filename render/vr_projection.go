package render

import (
	"errors"
	"math"
)

// Grayscale projections of the CPU ray caster: MIP, MinIP, Average and the
// ray-sum (#1181). A clean-room implementation in the spirit of VTK's
// ADDITIVE_BLEND and AverageIPScalarRange; no VTK code is copied.

// VRModeRaySum integrates the modality value along each ray: the sum of
// value × step length in modality units × mm (HU×mm for CT), a digitally
// reconstructed radiograph. The frame shows the integral divided by the
// volume's physical diagonal through the VR window, so the window keeps
// modality units and a uniform material brightens linearly with the length a
// ray traverses before the window clamps it. It follows VRModeIsoSurface (4)
// and is carried by the V21 GPU view-state contract.
const VRModeRaySum VRMode = 5

// ErrInvalidVRValueRange reports an enabled VRValueRange whose bounds are not
// finite or are reversed.
var ErrInvalidVRValueRange = errors.New("render: invalid VR projection value range")

// VRValueRange restricts Average and RaySum to the samples whose modality
// value lies in [Low, High], inclusive, so air and the table can be excluded
// from a mean or a radiograph. The zero value (Enabled false) keeps every
// sample. MIP, MinIP and DVR ignore it.
type VRValueRange struct {
	Low, High float64
	Enabled   bool
}

// Valid reports whether the range is disabled or has finite, ordered bounds.
func (r VRValueRange) Valid() bool {
	return !r.Enabled || (finite(r.Low) && finite(r.High) && r.Low <= r.High)
}

// contains reports whether value takes part in a ranged projection.
func (r VRValueRange) contains(value float64) bool {
	return !r.Enabled || (value >= r.Low && value <= r.High)
}

// UsesValueRange reports whether the preset's mode applies its enabled
// ProjectionRange.
func (preset VRPreset) UsesValueRange() bool {
	return preset.ProjectionRange.Enabled && (preset.Mode == VRModeAverage || preset.Mode == VRModeRaySum)
}

// CPUOnlyProjection reports the historical pre-V21 projection restriction.
// Deprecated: V21 carries Ray Sum and ranged Average. Consumers should validate
// their selected view-state contract and runtime capabilities instead.
func (preset VRPreset) CPUOnlyProjection() bool {
	return preset.Mode == VRModeRaySum || preset.UsesValueRange()
}

// vrProjection is the per-render state of a grayscale projection march.
type vrProjection struct {
	mode       VRMode
	window     WindowLevel
	valueRange VRValueRange
	coloredMIP bool
	// raySumReferenceMM divides the ray-sum before the window.
	raySumReferenceMM float64
}

func newVRProjection(preset VRPreset, window WindowLevel, diagonalMM float64) vrProjection {
	projection := vrProjection{mode: preset.Mode, window: window, raySumReferenceMM: diagonalMM, coloredMIP: preset.ColoredMIP && preset.Mode == VRModeMIP}
	if preset.UsesValueRange() {
		projection.valueRange = preset.ProjectionRange
	}
	return projection
}

// display maps the accumulated value of count samples to the gray in [0,1]
// before inversion.
func (p vrProjection) display(acc float64, count int) float64 {
	switch p.mode {
	case VRModeAverage:
		return acc / float64(count)
	case VRModeRaySum:
		if !(p.raySumReferenceMM > 0) {
			return 0
		}
		return windowedUnit(acc/p.raySumReferenceMM, p.window)
	}
	return acc
}

// forValueRange enables the skipping of Average and RaySum blocks whose
// every sample lies outside the range: they add nothing to the march.
func (l *vrSpaceLeap) forValueRange(valueRange VRValueRange) {
	l.valueRange = valueRange
}

// outsideValueRange reports whether no trilinear sample of block lies in the
// leap's value range.
func (l *vrSpaceLeap) outsideValueRange(block int) bool {
	if !l.valueRange.Enabled {
		return false
	}
	lo, hi := float64(l.grid.min[block]), float64(l.grid.max[block])
	if math.IsNaN(lo) {
		return false
	}
	lo, hi = widen(lo, hi)
	return hi < l.valueRange.Low || lo > l.valueRange.High
}

// skipsProjectionSample reports whether the sample in block cannot change
// the projection: a MIP/MinIP block that cannot beat the extreme acc of count
// samples, or an Average/RaySum block outside the value range.
func (l *vrSpaceLeap) skipsProjectionSample(block int, mode VRMode, acc float64, count int) bool {
	switch mode {
	case VRModeMIP, VRModeMinIP:
		return count > 0 && l.skipsProjection(block, mode, acc)
	case VRModeAverage, VRModeRaySum:
		return l.outsideValueRange(block)
	}
	return false
}
