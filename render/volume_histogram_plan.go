package render

import (
	"math"
)

// VolumeHistogramPlan contains only metadata. One-dimensional histograms count
// the full scalar volume; joint histograms use the existing bounded isotropic
// lattice and covariant central differences in modality units per millimetre.
type VolumeHistogramPlan struct {
	Dimensions, Lattice    [3]uint32
	Stride                 uint32
	Voxels, Samples, Cells uint64
	GradientCovector       [9]float64
}

func PlanVolumeHistogram(d VolumeDescriptor, intensityBins, gradientBins int) (VolumeHistogramPlan, error) {
	if err := ValidateVolumeDescriptor(d); err != nil {
		return VolumeHistogramPlan{}, err
	}
	if d.Components != 1 || intensityBins < 1 || intensityBins > maximumVRHistogramBins || gradientBins < 0 || gradientBins > 4096 || gradientBins == 1 || gradientBins > 0 && (intensityBins < 2 || intensityBins > 4096) {
		return VolumeHistogramPlan{}, ErrInvalidVolumeSnapshot
	}
	p := VolumeHistogramPlan{Dimensions: d.Dimensions, Lattice: d.Dimensions, Stride: 1, Cells: uint64(intensityBins) * uint64(max(1, gradientBins))}
	p.Voxels = uint64(d.Dimensions[0]) * uint64(d.Dimensions[1])
	if p.Voxels > math.MaxUint64/uint64(d.Dimensions[2]) {
		return VolumeHistogramPlan{}, ErrInvalidVolumeSnapshot
	}
	p.Voxels *= uint64(d.Dimensions[2])
	p.Samples = p.Voxels
	if gradientBins > 0 {
		p.Stride = uint32(max(1, math.Floor(math.Cbrt(float64(p.Voxels)/MaxVRJointHistogramSamples))))
		for {
			p.Samples = 1
			for i, n := range d.Dimensions {
				p.Lattice[i] = uint32((uint64(n) + uint64(p.Stride) - 1) / uint64(p.Stride))
				p.Samples *= uint64(p.Lattice[i])
			}
			if p.Samples <= MaxVRJointHistogramSamples {
				break
			}
			p.Stride++
		}
	}
	for row := 0; row < 3; row++ {
		for column := 0; column < 3; column++ {
			p.GradientCovector[row*3+column] = .5 * d.PatientLPSToIndex[column*4+row]
		}
	}
	return p, nil
}
