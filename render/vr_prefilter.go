package render

import (
	"context"
	"fmt"
	"math"
	"slices"
)

// Render-only noise-reduction prefilters for volume rendering of low-dose CT
// (#1008), after VTK's vtkImageGaussianSmooth and vtkImageMedian3D: a
// separable Gaussian with sigma in millimetres (so anisotropic voxels are
// smoothed equally in patient space) and a 3×3×3 median. Both clip their
// kernel at the volume border: the Gaussian renormalizes over the voxels it
// covers, the median takes the voxels present (the mean of the two middle
// values for an even count). Work is split across slices or lines.

// VRGaussianPrefilterSigmaMM is the sigma of VRPrefilterGaussian1mm.
const VRGaussianPrefilterSigmaMM = 1.0

// vrGaussianRadiusFactor truncates the Gaussian kernel at 3 sigma.
const vrGaussianRadiusFactor = 3

// SmoothGaussian3D smooths an x-fastest volume of dims voxels with a
// separable Gaussian of sigmaMM millimetres, given the voxel spacing (x, y,
// z) in millimetres. An axis whose sigma is below a hundredth of a voxel, or
// of a single voxel, is left as is.
func SmoothGaussian3D(ctx context.Context, values []float32, dims [3]int, spacingMM [3]float64, sigmaMM float64) ([]float32, error) {
	if err := checkPrefilterInput(values, dims); err != nil {
		return nil, err
	}
	if !(sigmaMM > 0) || math.IsInf(sigmaMM, 0) {
		return nil, fmt.Errorf("render: invalid Gaussian sigma %v mm", sigmaMM)
	}
	out := slices.Clone(values)
	strides := [3]int{1, dims[0], dims[0] * dims[1]}
	for axis := 0; axis < 3; axis++ {
		if !(spacingMM[axis] > 0) {
			return nil, fmt.Errorf("render: invalid voxel spacing %v mm", spacingMM[axis])
		}
		sigma := sigmaMM / spacingMM[axis]
		radius := int(sigma * vrGaussianRadiusFactor)
		n := dims[axis]
		if sigma < 0.01 || radius < 1 || n < 2 {
			continue
		}
		radius = min(radius, n-1)
		weights := make([]float64, 2*radius+1)
		for k := range weights {
			x := float64(k - radius)
			weights[k] = math.Exp(-x * x / (2 * sigma * sigma))
		}
		// Every line along the axis starts at a voxel of the other two axes.
		a, b := (axis+1)%3, (axis+2)%3
		err := parallelRowsContext(ctx, dims[b], func(j int) {
			line := make([]float64, n)
			for i := 0; i < dims[a]; i++ {
				start := i*strides[a] + j*strides[b]
				for k := 0; k < n; k++ {
					line[k] = float64(out[start+k*strides[axis]])
				}
				for k := 0; k < n; k++ {
					lo, hi := max(k-radius, 0), min(k+radius, n-1)
					sum, weight := 0.0, 0.0
					for m := lo; m <= hi; m++ {
						w := weights[m-k+radius]
						sum += w * line[m]
						weight += w
					}
					out[start+k*strides[axis]] = float32(sum / weight)
				}
			}
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Median3x3x3 replaces each voxel of an x-fastest volume by the median of
// its 3×3×3 neighbourhood clipped to the volume.
func Median3x3x3(ctx context.Context, values []float32, dims [3]int) ([]float32, error) {
	if err := checkPrefilterInput(values, dims); err != nil {
		return nil, err
	}
	out := make([]float32, len(values))
	nx, ny, nz := dims[0], dims[1], dims[2]
	err := parallelRowsContext(ctx, nz, func(z int) {
		var hood [27]float32
		for y := 0; y < ny; y++ {
			for x := 0; x < nx; x++ {
				count := 0
				for zz := max(z-1, 0); zz <= min(z+1, nz-1); zz++ {
					for yy := max(y-1, 0); yy <= min(y+1, ny-1); yy++ {
						row := (zz*ny + yy) * nx
						for xx := max(x-1, 0); xx <= min(x+1, nx-1); xx++ {
							hood[count] = values[row+xx]
							count++
						}
					}
				}
				h := hood[:count]
				slices.Sort(h)
				median := h[count/2]
				if count%2 == 0 {
					low := h[count/2-1]
					median = low + (median-low)/2
				}
				out[(z*ny+y)*nx+x] = median
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func checkPrefilterInput(values []float32, dims [3]int) error {
	count, ok := checkedVoxelCount(dims[0], dims[1], dims[2])
	if !ok || dims[0] <= 0 || dims[1] <= 0 || dims[2] <= 0 || len(values) != count {
		return fmt.Errorf("%w: prefilter volume of %d values for dimensions %v", ErrInvalidVolumeSnapshot, len(values), dims)
	}
	for _, v := range values {
		if v != v || math.IsInf(float64(v), 0) {
			return fmt.Errorf("%w: invalid source voxel", ErrInvalidVolumeSnapshot)
		}
	}
	return nil
}

// samplerModalityValues copies the sampler's modality values, x-fastest.
func samplerModalityValues(ctx context.Context, sampler *volumeSampler) ([]float32, error) {
	count, ok := checkedVoxelCount(sampler.cols, sampler.rows, sampler.depth)
	if !ok {
		return nil, fmt.Errorf("%w: prepared volume is too large", ErrInvalidVolumeSnapshot)
	}
	if len(sampler.values) == count {
		return slices.Clone(sampler.values), nil
	}
	values := make([]float32, count)
	for z := 0; z < sampler.depth; z++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for y := 0; y < sampler.rows; y++ {
			for x := 0; x < sampler.cols; x++ {
				value, ok := sampler.valueAt(x, y, z)
				if !ok {
					return nil, fmt.Errorf("%w: invalid source voxel", ErrInvalidVolumeSnapshot)
				}
				values[(z*sampler.rows+y)*sampler.cols+x] = float32(value)
			}
		}
	}
	return values, nil
}
