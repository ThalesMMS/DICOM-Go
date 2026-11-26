package render

import (
	"context"
	"fmt"
	"math"
	"slices"
)

// Joint histogram of modality value and gradient magnitude (#1232), the
// backdrop of the 2D transfer function editor (after the 2D histograms of
// Kniss et al.'s multidimensional transfer functions and the author's Isis
// GradientHistogramCalculator; no code is copied). Each counted voxel adds
// one to the bin of its value and of its covariant central-difference
// gradient magnitude in modality units per mm, the magnitude DVR classifies
// with. Large volumes are counted on a regular voxel lattice of at most
// MaxVRJointHistogramSamples voxels.

// MaxVRJointHistogramSamples bounds the voxels a joint histogram counts.
const MaxVRJointHistogramSamples = 1 << 21

// vrJointHistogramGradientPercentile chooses the automatic gradient maximum:
// the magnitudes above it are the few strongest edges, clamped into the top
// row.
const vrJointHistogramGradientPercentile = 0.99

// VRJointHistogram counts voxels by modality value (columns over
// [IntensityMin, IntensityMax]) and gradient magnitude (rows over
// [0, GradientMax], the last row also holding larger magnitudes).
type VRJointHistogram struct {
	IntensityBins, GradientBins int
	IntensityMin, IntensityMax  float64
	GradientMax                 float64
	// Counts is row-major: gradient row r, value column c at
	// r·IntensityBins+c.
	Counts []uint64
	// Samples is the number of voxels counted, Stride the voxel spacing of
	// the counting lattice on every axis.
	Samples uint64
	Stride  int
}

// At returns the count of a value column and gradient row.
func (h VRJointHistogram) At(column, row int) uint64 {
	if column < 0 || row < 0 || column >= h.IntensityBins || row >= h.GradientBins || len(h.Counts) != h.IntensityBins*h.GradientBins {
		return 0
	}
	return h.Counts[row*h.IntensityBins+column]
}

// JointHistogramContext counts the prepared volume's voxels into
// intensityBins × gradientBins bins (each 2..4096). gradientMax <= 0 selects
// the 99th percentile of the counted magnitudes (at least one unit per mm).
// The value range is the volume's exact range.
func (p *VRPreparedVolume) JointHistogramContext(ctx context.Context, intensityBins, gradientBins int, gradientMax float64) (VRJointHistogram, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if intensityBins < 2 || gradientBins < 2 || intensityBins > 4096 || gradientBins > 4096 {
		return VRJointHistogram{}, fmt.Errorf("render: invalid VR joint histogram bins %dx%d", intensityBins, gradientBins)
	}
	if math.IsNaN(gradientMax) || math.IsInf(gradientMax, 0) {
		return VRJointHistogram{}, fmt.Errorf("render: invalid VR joint histogram gradient maximum %v", gradientMax)
	}
	if p == nil || p.Volume == nil {
		return VRJointHistogram{}, ErrInvalidVolumeSnapshot
	}
	minimum, maximum, err := p.intensityRangeContext(ctx)
	if err != nil {
		return VRJointHistogram{}, err
	}
	sampler, err := p.newSampler()
	if err != nil {
		return VRJointHistogram{}, err
	}
	defer sampler.Close()
	stride := 1
	for {
		count := uint64(ceilDiv(sampler.cols, stride)) * uint64(ceilDiv(sampler.rows, stride)) * uint64(ceilDiv(sampler.depth, stride))
		if count <= MaxVRJointHistogramSamples {
			break
		}
		stride++
	}
	nx, ny, nz := ceilDiv(sampler.cols, stride), ceilDiv(sampler.rows, stride), ceilDiv(sampler.depth, stride)
	values := make([]float32, nx*ny*nz)
	magnitudes := make([]float32, nx*ny*nz)
	vol := sampler.vol
	err = parallelRowsContext(ctx, nz, func(k int) {
		z := k * stride
		for j := 0; j < ny; j++ {
			y := j * stride
			for i := 0; i < nx; i++ {
				x := i * stride
				index := (k*ny+j)*nx + i
				values[index] = float32(sampler.valueClamped(x, y, z))
				gradient := Vec3{
					X: sampler.valueClamped(x+1, y, z) - sampler.valueClamped(x-1, y, z),
					Y: sampler.valueClamped(x, y+1, z) - sampler.valueClamped(x, y-1, z),
					Z: sampler.valueClamped(x, y, z+1) - sampler.valueClamped(x, y, z-1),
				}
				magnitudes[index] = float32(vol.physicalGradientMagnitude(gradient))
			}
		}
	})
	if err != nil {
		return VRJointHistogram{}, err
	}
	if !(gradientMax > 0) {
		sorted := slices.Clone(magnitudes)
		slices.Sort(sorted)
		gradientMax = 1
		if len(sorted) > 0 {
			gradientMax = math.Max(1, float64(sorted[int(vrJointHistogramGradientPercentile*float64(len(sorted)-1))]))
		}
	}
	histogram := VRJointHistogram{
		IntensityBins: intensityBins, GradientBins: gradientBins,
		IntensityMin: minimum, IntensityMax: maximum, GradientMax: gradientMax,
		Counts: make([]uint64, intensityBins*gradientBins), Stride: stride,
	}
	bin := func(u float64, n int) int {
		if !(u > 0) {
			return 0
		}
		return min(int(u*float64(n)), n-1)
	}
	for index, value := range values {
		column := 0
		if maximum > minimum {
			column = bin((float64(value)-minimum)/(maximum-minimum), intensityBins)
		}
		row := bin(float64(magnitudes[index])/gradientMax, gradientBins)
		histogram.Counts[row*intensityBins+column]++
	}
	histogram.Samples = uint64(len(values))
	return histogram, ctx.Err()
}

func ceilDiv(n, d int) int { return (n + d - 1) / d }
