package render

import (
	"fmt"
	"math"
)

type VolumeFilterMode uint32

const (
	VolumeFilterNone VolumeFilterMode = iota
	VolumeFilterGaussian
	VolumeFilterBasicSmooth5x5
	// Median3x3x3 clips its neighbourhood at volume borders and averages
	// the two central modality values when the surviving count is even.
	VolumeFilterMedian3x3x3
	// VesselEnhance selects the physical multiscale bright-tube display ramp.
	VolumeFilterVesselEnhance
)

// VolumeFilterState selects display-only volumetric filtering. The original
// source generation and geometry remain unchanged. This contract never samples
// or filters a volume on the CPU; backends explicitly implement/refuse it.
type VolumeFilterState struct {
	Mode    VolumeFilterMode
	SigmaMM float64
}

const MaxVolumeGaussianRadius = 64

func (s VolumeFilterState) Validate() error {
	if s.Mode > VolumeFilterVesselEnhance || !finiteScalar(s.SigmaMM) || ((s.Mode != VolumeFilterGaussian) && s.SigmaMM != 0) || (s.Mode == VolumeFilterGaussian && (s.SigmaMM <= 0 || s.SigmaMM > 100)) {
		return fmt.Errorf("%w: invalid volume filter", ErrInvalidViewState)
	}
	return nil
}

type VolumeGaussianPlan struct {
	Radius  [3]uint32
	Weights [3][2*MaxVolumeGaussianRadius + 1]float32
}

// PlanVolumeGaussian supplies bounded geometry/kernel metadata only. Kernels
// truncate at floor(3*sigma/spacing), clipped to dimension-1. At borders the
// backend omits out-of-volume taps and renormalizes their surviving weights.
func PlanVolumeGaussian(d VolumeDescriptor, s VolumeFilterState) (VolumeGaussianPlan, error) {
	var plan VolumeGaussianPlan
	if err := s.Validate(); err != nil {
		return plan, err
	}
	if s.Mode != VolumeFilterGaussian {
		return plan, fmt.Errorf("%w: Gaussian plan requires Gaussian mode", ErrInvalidViewState)
	}
	if err := ValidateVolumeDescriptor(d); err != nil {
		return plan, err
	}
	for axis, n := range d.Dimensions {
		a := d.IndexToPatientLPS
		spacing := math.Hypot(math.Hypot(a[axis], a[axis+4]), a[axis+8])
		if spacing <= 0 || !finiteScalar(spacing) {
			return plan, fmt.Errorf("%w: Gaussian voxel spacing", ErrInvalidViewState)
		}
		sigma := s.SigmaMM / spacing
		radius := math.Floor(3 * sigma)
		if n < 2 || sigma < 0.01 || radius < 1 {
			plan.Weights[axis][0] = 1
			continue
		}
		radius = math.Min(radius, float64(n-1))
		if radius > MaxVolumeGaussianRadius {
			return plan, fmt.Errorf("%w: Gaussian radius exceeds %d", ErrInvalidViewState, MaxVolumeGaussianRadius)
		}
		r := int(radius)
		plan.Radius[axis] = uint32(r)
		total := 0.0
		for k := -r; k <= r; k++ {
			total += math.Exp(-float64(k*k) / (2 * sigma * sigma))
		}
		for k := -r; k <= r; k++ {
			plan.Weights[axis][k+r] = float32(math.Exp(-float64(k*k)/(2*sigma*sigma)) / total)
		}
	}
	return plan, nil
}

// VolumeBasicSmoothKernel is the existing in-plane BasicSmooth5x5 contract,
// expressed as detached metadata. The backend clamps tap coordinates at edges.
func VolumeBasicSmoothKernel() [5][5]float32 {
	var kernel [5][5]float32
	for y, row := range basicSmooth5x5Kernel {
		for x, weight := range row {
			kernel[y][x] = float32(weight)
		}
	}
	return kernel
}
