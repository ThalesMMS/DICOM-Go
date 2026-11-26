package imagefilter

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

// ErrCannyResourceLimit reports a request outside Canny's finite logical budget.
var ErrCannyResourceLimit = errors.New("imagefilter: Canny resource limit exceeded")

// CannyParameters selects physical Gaussian scale and gradient thresholds.
// Thresholds are in input intensity units/mm, without intensity normalization.
// Zero SigmaMM uses 1 mm on active axes; 0/0 thresholds link all positive peaks.
type CannyParameters struct {
	SigmaMM                        [3]float64
	LowerThreshold, UpperThreshold float64
}

// CannyLimits bounds logical retained capacities, excluding caller input and Go
// runtime overhead. Zero selects 8 Mi voxels/512 MiB; overrides may only lower
// those ceilings. These reservations are not measurements of resident memory.
type CannyLimits struct {
	MaxVoxels int
	MaxBytes  int64
}

const cannyMaxVoxels = 8 << 20
const cannyMaxBytes int64 = 512 << 20

// CannyOptions selects Canny parameters and request-local resource limits.
// Workers is bounded by the shared helper budget (zero selects that budget).
type CannyOptions struct {
	CannyParameters
	Limits  CannyLimits
	Workers int
}

// CannyResult owns two independent dense arrays: Edges is exactly 0/1 and
// Strength is the retained gradient magnitude, zero outside linked edges.
type CannyResult struct {
	Edges, Strength *Volume
	EdgeCount       int
}

// DefaultCannyParameters returns sigma 1 mm and zero thresholds.
func DefaultCannyParameters() CannyParameters { return CannyParameters{SigmaMM: IsotropicSigma(1)} }

// Normalize validates parameters for a 2D or 3D image and resolves defaults.
func (p CannyParameters) Normalize(dimensions int) (CannyParameters, error) {
	if dimensions != 2 && dimensions != 3 {
		return p, fmt.Errorf("%w: Canny dimensions", ErrInvalidVolume)
	}
	if p.SigmaMM == [3]float64{} {
		p.SigmaMM = IsotropicSigma(1)
	}
	for a := 0; a < dimensions; a++ {
		if !(p.SigmaMM[a] > 0 && p.SigmaMM[a] <= math.MaxFloat64) {
			return p, fmt.Errorf("%w: Canny sigma on axis %d", ErrInvalidVolume, a)
		}
	}
	if dimensions == 2 {
		p.SigmaMM[2] = 0
	}
	if !(p.LowerThreshold >= 0 && p.UpperThreshold >= p.LowerThreshold && p.UpperThreshold <= math.MaxFloat64) {
		return p, fmt.Errorf("%w: Canny thresholds", ErrInvalidVolume)
	}
	return p, nil
}

// Normalize validates limits and resolves finite defaults.
func (l CannyLimits) Normalize() (CannyLimits, error) {
	if l.MaxVoxels < 0 || l.MaxVoxels > cannyMaxVoxels || l.MaxBytes < 0 || l.MaxBytes > cannyMaxBytes {
		return l, fmt.Errorf("%w: Canny limits", ErrInvalidVolume)
	}
	if l.MaxVoxels == 0 {
		l.MaxVoxels = cannyMaxVoxels
	}
	if l.MaxBytes == 0 {
		l.MaxBytes = cannyMaxBytes
	}
	return l, nil
}

// CannyWorkingBytes reserves all mutable Canny/gradient buffers and retained
// Gaussian scratch for dims. It lets callback adapters admit work before
// allocating their dense input. workers must be a fixed positive ceiling;
// callers using zero Workers resolve it once before both admission and Canny.
func CannyWorkingBytes(dims [3]int, workers int) (int64, error) {
	n, ok := voxelCount(dims)
	if !ok || dims[0] < 2 || dims[1] < 2 || n > cannyMaxVoxels || workers < 1 {
		return 0, fmt.Errorf("%w: Canny dimensions/workers", ErrCannyResourceLimit)
	}
	// At most five Gaussian arrays (20N), gradient magnitude (8N), peaks and
	// two outputs (12N), linked queue (4N); 64N leaves room for bookkeeping.
	total := int64(n)*64 + 4096
	scratch, err := CannyGaussianScratchBytes(dims, workers)
	if err != nil {
		return 0, err
	}
	return total + scratch, nil
}

// CannyGaussianScratchBytes bounds retained line scratch for Canny/gradient-magnitude axis passes,
// including causal arrays and 4*32 double states per worker. It does not
// allocate a max-axis array for every helper: worker j retains the largest
// active-axis array it could have claimed, preserving lazy scratch admission.
func CannyGaussianScratchBytes(dims [3]int, workers int) (int64, error) {
	n, ok := voxelCount(dims)
	if !ok || workers < 1 || n > 16<<20 {
		return 0, fmt.Errorf("%w: Gaussian scratch dimensions", ErrCannyResourceLimit)
	}
	var samples [3]int64
	var active [3]int
	maxWorkers := 0
	for a, d := range dims {
		if d == 1 {
			continue
		}
		lanes, groups := n/d, 1
		if a == 1 {
			lanes, groups = dims[0], dims[2]
		}
		chunks := (lanes + laneWidth - 1) / laneWidth
		active[a] = min(workers, groups*chunks)
		maxWorkers = max(maxWorkers, active[a])
		samples[a] = int64(d) * int64(min(laneWidth, lanes))
	}
	// No storage allocation proportional to an untrusted worker count.
	total := int64(maxWorkers) * (4*laneWidth*8 + 128)
	for w := 0; w < maxWorkers; w++ {
		var s int64
		for a := 0; a < 3; a++ {
			if w < active[a] {
				s = max(s, samples[a])
			}
		}
		total += s * 8
	}
	return total, nil
}

// Canny detects edges on a finite dense regular 2D/3D grid. It uses the
// existing constant-end recursive Gaussian derivatives, interpolated physical
// nonmaximum suppression and 8/26-neighbour hysteresis. It is not bitwise ITK's
// discrete Gaussian/zero-crossing Canny. No input/provider is retained.
func Canny(ctx context.Context, src *Volume, options CannyOptions) (*CannyResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if src == nil {
		return nil, fmt.Errorf("%w: nil Canny volume", ErrInvalidVolume)
	}
	n, ok := voxelCount(src.Dims)
	if !ok || len(src.Data) != n || src.Dims[0] < 2 || src.Dims[1] < 2 {
		return nil, fmt.Errorf("%w: Canny dimensions/data", ErrInvalidVolume)
	}
	dimensions := 3
	if src.Dims[2] == 1 {
		dimensions = 2
	}
	p, err := options.CannyParameters.Normalize(dimensions)
	if err != nil {
		return nil, err
	}
	limits, err := options.Limits.Normalize()
	if err != nil {
		return nil, err
	}
	if options.Workers < 0 {
		return nil, fmt.Errorf("%w: negative Canny workers", ErrInvalidVolume)
	}
	workers := min(parallel.Workers(options.Workers), parallel.DefaultWorkers())
	bytes, err := CannyWorkingBytes(src.Dims, workers)
	if err != nil {
		return nil, err
	}
	if n > limits.MaxVoxels || bytes > limits.MaxBytes {
		return nil, fmt.Errorf("%w: %d voxels, %d reserved bytes", ErrCannyResourceLimit, n, bytes)
	}
	for a := 0; a < dimensions; a++ {
		h := src.SpacingMM[a]
		extent := float64(src.Dims[a]) * h
		if !(h > 0 && h <= math.MaxFloat64) || !(extent <= math.Sqrt(math.MaxFloat64)/4) {
			return nil, fmt.Errorf("%w: Canny spacing/extent axis %d", ErrInvalidVolume, a)
		}
	}
	if err = validateGaussianSource(ctx, src); err != nil {
		return nil, err
	}
	flat := true
	for i, v := range src.Data {
		if i%4096 == 0 {
			if err = ctx.Err(); err != nil {
				return nil, err
			}
		}
		if v != src.Data[0] {
			flat = false
			break
		}
	}
	if flat {
		r := &CannyResult{Edges: &Volume{Dims: src.Dims, SpacingMM: src.SpacingMM, Data: make([]float32, n)}, Strength: &Volume{Dims: src.Dims, SpacingMM: src.SpacingMM, Data: make([]float32, n)}}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		return r, nil
	}
	gradients, err := GradientRecursiveGaussian(ctx, src, RecursiveGaussianOptions{SigmaMM: p.SigmaMM, Workers: workers})
	if err != nil {
		return nil, err
	}
	magnitude := make([]float64, n)
	for i := range magnitude {
		if i%4096 == 0 {
			if err = ctx.Err(); err != nil {
				return nil, err
			}
		}
		x, y, z := float64(gradients[0].Data[i]), float64(gradients[1].Data[i]), float64(gradients[2].Data[i])
		m := math.Hypot(math.Hypot(x, y), z)
		if !(m <= math.MaxFloat32) {
			return nil, fmt.Errorf("%w: non-finite Canny gradient", ErrInvalidVolume)
		}
		magnitude[i] = m
	}
	peaks, err := cannySuppress(ctx, src.Dims, src.SpacingMM, gradients, magnitude)
	if err != nil {
		return nil, err
	}
	edges, count, err := cannyLink(ctx, src.Dims, peaks, p.LowerThreshold, p.UpperThreshold)
	if err != nil {
		return nil, err
	}
	for i := range peaks {
		if i%4096 == 0 {
			if err = ctx.Err(); err != nil {
				return nil, err
			}
		}
		if edges[i] == 0 {
			peaks[i] = 0
		}
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return &CannyResult{Edges: &Volume{Dims: src.Dims, SpacingMM: src.SpacingMM, Data: edges}, Strength: &Volume{Dims: src.Dims, SpacingMM: src.SpacingMM, Data: peaks}, EdgeCount: count}, nil
}

func cannySuppress(ctx context.Context, dims [3]int, h [3]float64, g [3]*Volume, m []float64) ([]float32, error) {
	peaks := make([]float32, len(m))
	nx, ny := dims[0], dims[1]
	for i, v := range m {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if v <= 0 {
			continue
		}
		var dir [3]float64
		largest := 0.
		for a := 0; a < 3; a++ {
			if dims[a] > 1 {
				dir[a] = float64(g[a].Data[i]) / h[a]
				largest = math.Max(largest, math.Abs(dir[a]))
			}
		}
		if !(largest > 0 && largest <= math.MaxFloat64) {
			return nil, fmt.Errorf("%w: Canny direction", ErrInvalidVolume)
		}
		// The largest index displacement fixes direction; an exact tie
		// chooses the earlier axis. Tiny transverse roundoff cannot flip it.
		dominant := 0
		for a := 1; a < 3; a++ {
			if math.Abs(dir[a]) > math.Abs(dir[dominant]) {
				dominant = a
			}
		}
		sign := 1.
		if dir[dominant] < 0 {
			sign = -1
		}
		for a := range dir {
			dir[a] *= sign / largest
		}
		pos := [3]float64{float64(i % nx), float64(i / nx % ny), float64(i / (nx * ny))}
		plus, minus := pos, pos
		for a := range pos {
			plus[a] += dir[a]
			minus[a] -= dir[a]
		}
		// A replicated probe at the current voxel cannot supply a distinct
		// neighbour for the strict test; use the inward probe instead.
		negative, positive := cannyInterpolate(m, dims, minus), cannyInterpolate(m, dims, plus)
		plusSelf := true
		for a := range pos {
			clamped := math.Max(0, math.Min(float64(dims[a]-1), plus[a]))
			if clamped != pos[a] {
				plusSelf = false
			}
		}
		keep := v >= negative && v > positive
		if plusSelf {
			keep = v > negative
		}
		if keep {
			peaks[i] = float32(v)
		}
	}
	return peaks, nil
}

func cannyInterpolate(data []float64, dims [3]int, p [3]float64) float64 {
	var lo, hi [3]int
	var w [3]float64
	for a := range p {
		x := math.Max(0, math.Min(float64(dims[a]-1), p[a]))
		lo[a] = int(x)
		hi[a] = min(lo[a]+1, dims[a]-1)
		w[a] = x - float64(lo[a])
	}
	sum := 0.
	for z := 0; z < 2; z++ {
		for y := 0; y < 2; y++ {
			for x := 0; x < 2; x++ {
				pos := [3]int{lo[0], lo[1], lo[2]}
				weight := 1.
				for a, k := range [3]int{x, y, z} {
					if k == 1 {
						pos[a] = hi[a]
						weight *= w[a]
					} else {
						weight *= 1 - w[a]
					}
				}
				sum += weight * data[(pos[2]*dims[1]+pos[1])*dims[0]+pos[0]]
			}
		}
	}
	return sum
}

func cannyLink(ctx context.Context, dims [3]int, peaks []float32, lower, upper float64) ([]float32, int, error) {
	edges := make([]float32, len(peaks))
	queue := make([]int32, len(peaks))
	head, tail := 0, 0
	for i, v := range peaks {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, 0, err
			}
		}
		if v > 0 && float64(v) >= upper {
			edges[i] = 1
			queue[tail] = int32(i)
			tail++
		}
	}
	nx, ny, nz := dims[0], dims[1], dims[2]
	zlo, zhi := -1, 1
	if nz == 1 {
		zlo, zhi = 0, 0
	}
	for head < tail {
		if head%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, 0, err
			}
		}
		i := int(queue[head])
		head++
		x, y, z := i%nx, i/nx%ny, i/(nx*ny)
		for dz := zlo; dz <= zhi; dz++ {
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					xx, yy, zz := x+dx, y+dy, z+dz
					if xx < 0 || yy < 0 || zz < 0 || xx >= nx || yy >= ny || zz >= nz {
						continue
					}
					j := (zz*ny+yy)*nx + xx
					if edges[j] == 0 && peaks[j] > 0 && float64(peaks[j]) >= lower {
						edges[j] = 1
						queue[tail] = int32(j)
						tail++
					}
				}
			}
		}
	}
	return edges, tail, ctx.Err()
}
