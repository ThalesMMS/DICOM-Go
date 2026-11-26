package render

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
)

// Cubic B-spline interpolation for non-interactive exports (#1190): the
// regularized NIfTI/NRRD/MetaImage volume and the aligned series of
// registration.Resample. The interactive MPR, CPR and VR samplers keep
// nearest, trilinear and Catmull-Rom (volume_interpolation.go).
//
// A cubic B-spline does not pass through the samples, so the samples of the
// exported region are first turned into coefficients by the recursive
// prefilter of Unser, Aldroubi and Eden ("B-spline signal processing",
// IEEE Trans. Signal Process. 41(2), 1993), separably along x, y and z with
// mirror boundaries, the same decomposition as ITK's
// BSplineDecompositionImageFilter. The interpolant then reproduces every
// sample exactly and is C² between them, smoother than trilinear and
// without Catmull-Rom's sharper overshoot. Reimplemented from the
// published equations; no ITK code is copied.

// ErrBSplineSamples reports a prefilter region that is empty, too large or
// holds a non-finite sample: the infinite-impulse prefilter would spread
// NaN or Inf over every coefficient of the lines through it.
var ErrBSplineSamples = errors.New("render: invalid cubic B-spline samples")

// bsplinePole is the pole of the cubic B-spline prefilter, √3 − 2.
var bsplinePole = math.Sqrt(3) - 2

// bsplineHorizon is the number of samples after which the pole's powers
// fall below 1e-12: the causal initialization sums no further.
var bsplineHorizon = int(math.Ceil(math.Log(1e-12) / math.Log(math.Abs(bsplinePole))))

// CubicBSpline is the cubic B-spline interpolant of a box of a 3D sample
// grid. Its coefficients are float32, as are the volumes it exports.
type CubicBSpline struct {
	min, size    [3]int
	coefficients []float32
}

// NewCubicBSplineContext prefilters the samples of the box of size samples
// whose first grid index is min, read through sample(x, y, z) in grid
// indices (called concurrently). The prefilter mirrors at the box's faces,
// so the result
// reproduces every sample of the box; choose a box that covers the points to
// evaluate with a few samples to spare when it is a part of a larger grid.
// ctx cancels it between lines.
func NewCubicBSplineContext(ctx context.Context, min, size [3]int, sample func(x, y, z int) float64) (*CubicBSpline, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if sample == nil {
		return nil, fmt.Errorf("%w: nil sample reader", ErrBSplineSamples)
	}
	count, ok := checkedVoxelCount(size[0], size[1], size[2])
	if !ok {
		return nil, fmt.Errorf("%w: box %v", ErrBSplineSamples, size)
	}
	spline := &CubicBSpline{min: min, size: size, coefficients: make([]float32, count)}
	nx, ny, nz := size[0], size[1], size[2]
	plane := nx * ny
	var nonFinite atomic.Bool
	err := parallelRowsContext(ctx, nz, func(z int) {
		line := make([]float64, nx)
		for y := 0; y < ny; y++ {
			for x := range line {
				line[x] = sample(min[0]+x, min[1]+y, min[2]+z)
			}
			for _, v := range line {
				if !finite(v) {
					nonFinite.Store(true)
					return
				}
			}
			bsplinePrefilterLine(line)
			row := spline.coefficients[z*plane+y*nx:]
			for x, v := range line {
				row[x] = float32(v)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	if nonFinite.Load() {
		return nil, fmt.Errorf("%w: non-finite sample", ErrBSplineSamples)
	}
	// y lines, one z plane per task.
	if err := parallelRowsContext(ctx, nz, func(z int) {
		line := make([]float64, ny)
		for x := 0; x < nx; x++ {
			for y := range line {
				line[y] = float64(spline.coefficients[z*plane+y*nx+x])
			}
			bsplinePrefilterLine(line)
			for y, v := range line {
				spline.coefficients[z*plane+y*nx+x] = float32(v)
			}
		}
	}); err != nil {
		return nil, err
	}
	// z lines, one y row per task.
	if err := parallelRowsContext(ctx, ny, func(y int) {
		line := make([]float64, nz)
		for x := 0; x < nx; x++ {
			for z := range line {
				line[z] = float64(spline.coefficients[z*plane+y*nx+x])
			}
			bsplinePrefilterLine(line)
			for z, v := range line {
				spline.coefficients[z*plane+y*nx+x] = float32(v)
			}
		}
	}); err != nil {
		return nil, err
	}
	return spline, nil
}

// bsplinePrefilterLine replaces the samples of one line by their cubic
// B-spline coefficients (mirror boundaries): the gain (1−z)(1−1/z) = 6,
// then one causal and one anti-causal first-order recursion.
func bsplinePrefilterLine(c []float64) {
	n := len(c)
	if n < 2 {
		return
	}
	z := bsplinePole
	gain := (1 - z) * (1 - 1/z)
	for i := range c {
		c[i] *= gain
	}
	c[0] = bsplineInitialCausal(c, z)
	for i := 1; i < n; i++ {
		c[i] += z * c[i-1]
	}
	c[n-1] = (z / (z*z - 1)) * (z*c[n-2] + c[n-1])
	for i := n - 2; i >= 0; i-- {
		c[i] = z * (c[i+1] - c[i])
	}
}

// bsplineInitialCausal is the first causal coefficient: the mirror-extended
// sum Σ z^k c[k], truncated past bsplineHorizon or exact for shorter lines.
func bsplineInitialCausal(c []float64, z float64) float64 {
	n := len(c)
	if bsplineHorizon < n {
		sum, zk := c[0], z
		for k := 1; k < bsplineHorizon; k++ {
			sum += zk * c[k]
			zk *= z
		}
		return sum
	}
	// Exact: (Σ_{k=0}^{n-1} z^k c[k] + Σ_{k=1}^{n-2} z^{2n-2-k} c[k] +
	// z^{n-1} c[n-1]) / (1 − z^{2n-2}), the mirrored sequence's geometric
	// series.
	zk := z
	z2n := math.Pow(z, float64(n-1))
	sum := c[0] + z2n*c[n-1]
	z2n *= z2n / z
	for k := 1; k < n-1; k++ {
		sum += (zk + z2n) * c[k]
		zk *= z
		z2n /= z
	}
	return sum / (1 - zk*zk)
}

// bsplineWeights returns the four cubic B-spline weights of the taps at
// floor(v)−1 … floor(v)+2 and floor(v).
func bsplineWeights(v float64) (base int, weight [4]float64) {
	base = int(math.Floor(v))
	t := v - float64(base)
	s := 1 - t
	t2, t3 := t*t, t*t*t
	weight = [4]float64{
		s * s * s / 6,
		(4 - 6*t2 + 3*t3) / 6,
		(1 + 3*t + 3*t2 - 3*t3) / 6,
		t3 / 6,
	}
	return base, weight
}

// mirrorIndex folds a box-relative index into [0, n) by mirroring at the
// first and last samples, as the prefilter's boundary condition.
func mirrorIndex(i, n int) int {
	if n == 1 {
		return 0
	}
	period := 2 * (n - 1)
	i %= period
	if i < 0 {
		i += period
	}
	if i >= n {
		i = period - i
	}
	return i
}

// At evaluates the interpolant at the continuous grid index p (the grid of
// NewCubicBSplineContext's sample reader). Points outside the box read its
// mirror image.
func (b *CubicBSpline) At(p Vec3) float64 {
	if b == nil || len(b.coefficients) == 0 {
		return 0
	}
	nx, ny, nz := b.size[0], b.size[1], b.size[2]
	bx, wx := bsplineWeights(p.X - float64(b.min[0]))
	by, wy := bsplineWeights(p.Y - float64(b.min[1]))
	bz, wz := bsplineWeights(p.Z - float64(b.min[2]))
	var xi [4]int
	for i := range xi {
		xi[i] = mirrorIndex(bx-1+i, nx)
	}
	sum := 0.0
	for k := 0; k < 4; k++ {
		if wz[k] == 0 {
			continue
		}
		planeOffset := mirrorIndex(bz-1+k, nz) * nx * ny
		plane := 0.0
		for j := 0; j < 4; j++ {
			if wy[j] == 0 {
				continue
			}
			row := b.coefficients[planeOffset+mirrorIndex(by-1+j, ny)*nx:]
			plane += wy[j] * (wx[0]*float64(row[xi[0]]) + wx[1]*float64(row[xi[1]]) +
				wx[2]*float64(row[xi[2]]) + wx[3]*float64(row[xi[3]]))
		}
		sum += wz[k] * plane
	}
	return sum
}
