package render

import (
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/imagefilter"
)

// VolumeVesselScale contains the scale-normalized recursive Hessian kernels.
// Kernels[axis][order] is absent for a singleton axis: smoothing is identity
// and any derivative on that axis is zero. Evaluate axes z, y, then x, with
// float32 rounding after each pass, as in the library's vesselness contract.
type VolumeVesselScale struct {
	SigmaMM float64
	Kernels [3][3]imagefilter.RecursiveGaussianKernel
	// ZMargin bounds the replicated-endpoint line extension on either side of
	// a core slab. Clipping this margin at a volume face is equivalent to the
	// library's six-sigma slab extension.
	ZMargin uint32
	// ZSlabSlices fixes the mathematical core domains independently of device
	// texture partitions and storage-buffer limits.
	ZSlabSlices uint32
}

// VolumeVesselEnhancementPlan describes a display-only bright-tube Frangi
// enhancement. Keep the maximum response across scales, sorted eigenvalues by
// magnitude, without structureness or largest-eigenvalue scaling. Convert that
// response to weight w using the ramp, then output minimum+(source-minimum)*w.
// The device computes the source minimum; this planner never accesses voxels.
type VolumeVesselEnhancementPlan struct {
	Dimensions        [3]uint32
	ImageDimensions   uint32
	Alpha, Beta       float64
	RampLow, RampHigh float64
	Scales            []VolumeVesselScale
}

// PlanVolumeVesselEnhancement supplies geometry and coefficients without
// preparing a filtered CPU volume. It does not assert device availability.
func PlanVolumeVesselEnhancement(d VolumeDescriptor) (VolumeVesselEnhancementPlan, error) {
	if err := ValidateVolumeDescriptor(d); err != nil {
		return VolumeVesselEnhancementPlan{}, err
	}
	if d.Components != 1 {
		return VolumeVesselEnhancementPlan{}, fmt.Errorf("%w: vessel enhancement requires scalar samples", ErrInvalidVolumeSnapshot)
	}
	p := VolumeVesselEnhancementPlan{Dimensions: d.Dimensions, ImageDimensions: 3,
		Alpha: .5, Beta: .5, RampLow: VesselEnhanceRampLow, RampHigh: VesselEnhanceRampHigh}
	if d.Dimensions[2] == 1 {
		p.ImageDimensions = 2
	}
	slab, err := imagefilter.PlanVesselnessSlabSlices([3]int{int(d.Dimensions[0]), int(d.Dimensions[1]), int(d.Dimensions[2])}, VesselEnhanceWorkingBytes)
	if err != nil {
		return VolumeVesselEnhancementPlan{}, err
	}
	for _, sigma := range VesselEnhanceSigmasMM(d.SpacingMM) {
		s := VolumeVesselScale{SigmaMM: sigma, ZSlabSlices: uint32(slab)}
		for axis, n := range d.Dimensions {
			if n == 1 {
				continue
			}
			samples := sigma / d.SpacingMM[axis]
			for order := range s.Kernels[axis] {
				k, err := imagefilter.PlanRecursiveGaussian(samples, order, math.Pow(samples, float64(order)))
				if err != nil {
					return VolumeVesselEnhancementPlan{}, fmt.Errorf("%w: vessel scale %v axis %d: %v", ErrInvalidVolumeSnapshot, sigma, axis, err)
				}
				s.Kernels[axis][order] = k
			}
		}
		if d.Dimensions[2] > 1 {
			s.ZMargin = uint32(math.Min(float64(d.Dimensions[2]-1), math.Ceil(imagefilter.VesselnessSlabMarginSigmas*sigma/d.SpacingMM[2])))
		}
		p.Scales = append(p.Scales, s)
	}
	return p, nil
}
