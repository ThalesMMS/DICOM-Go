package imagefilter

// Slab-wise execution of neighbourhood filters (#1189).
//
// A neighbourhood filter's output at a voxel depends only on the input
// within a bounded distance; along z that distance is the filter's halo, in
// slices. FilterSlabs runs such a filter on a volume it never holds whole:
// the output is produced in slabs of whole slices, each computed from the
// input slab extended by the halo on both sides (clipped at the volume's
// first and last slice), read from a SliceReader and written to a
// SliceWriter. Every kernel addresses samples in the coordinates of the
// whole volume, clamping or clipping at the volume's borders and never at
// the slab's, and the halo covers every slice a kernel reads, so each
// output sample is computed from the same values in the same order as on
// the whole volume: the result is bit-identical to the whole-volume filter
// for any slab thickness and worker count. The slab thickness is the
// largest that keeps the input window, the output slab and the kernel's
// scratch within SlabOptions.MaxWorkingBytes, down to one slice; successive
// input windows overlap by twice the halo, and the overlap is moved within
// the buffer rather than read again.
//
// The behaviour follows ITK's streaming of requested regions padded by the
// filter radius (ImageRegion::PadByRadius, used by BoxImageFilter and
// SliceBySliceImageFilter); the code is this package's own.
//
// Median, Bilateral and NonLocalMeans run through the same kernels on a
// whole Volume. UnsharpMask streams only in plane (zero z σ, or a 2-D
// image): its recursive Gaussian has unbounded support along z. Anisotropic
// diffusion rescales its conductance from the gradient of the whole volume
// at every iteration, so it cannot stream; it bounds its own scratch by
// AnisotropicDiffusionOptions.MaxWorkingBytes instead.

import (
	"context"
	"fmt"
	"math"
	"sync/atomic"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

// DefaultSlabWorkingBytes bounds the slab buffers when
// SlabOptions.MaxWorkingBytes or AnisotropicDiffusionOptions.MaxWorkingBytes
// is zero.
const DefaultSlabWorkingBytes = 256 << 20

// SliceReader supplies whole slices of a volume, for example from a decoded
// series or a memory-mapped file. *Volume implements it.
type SliceReader interface {
	// ReadSlices fills dst with slices [z0, z1), x-fastest; len(dst) is
	// (z1−z0)·columns·rows.
	ReadSlices(ctx context.Context, z0, z1 int, dst []float32) error
}

// SliceWriter receives whole output slices. *Volume implements it.
type SliceWriter interface {
	// WriteSlices stores the slices in src from z0 on. src is reused once
	// WriteSlices returns.
	WriteSlices(ctx context.Context, z0 int, src []float32) error
}

// ReadSlices copies slices [z0, z1) of v into dst.
func (v *Volume) ReadSlices(ctx context.Context, z0, z1 int, dst []float32) error {
	nxy := v.Dims[0] * v.Dims[1]
	if z0 < 0 || z1 > v.Dims[2] || z0 > z1 || len(dst) != (z1-z0)*nxy {
		return fmt.Errorf("%w: slices [%d, %d) into %d samples of a %v volume", ErrInvalidVolume, z0, z1, len(dst), v.Dims)
	}
	copy(dst, v.Data[z0*nxy:z1*nxy])
	return nil
}

// WriteSlices copies whole slices from src into v from slice z0 on.
func (v *Volume) WriteSlices(ctx context.Context, z0 int, src []float32) error {
	nxy := v.Dims[0] * v.Dims[1]
	if nxy == 0 || len(src)%nxy != 0 || z0 < 0 || z0+len(src)/nxy > v.Dims[2] {
		return fmt.Errorf("%w: %d samples at slice %d of a %v volume", ErrInvalidVolume, len(src), z0, v.Dims)
	}
	copy(v.Data[z0*nxy:], src)
	return nil
}

// SlabFilter is a neighbourhood filter FilterSlabs can run slab by slab:
// MedianOptions, BilateralOptions, NonLocalMeansOptions and (in plane
// only) UnsharpMaskOptions.
type SlabFilter interface {
	slabKernel(dims [3]int, spacingMM [3]float64) (slabKernel, error)
}

// slabKernel is a validated filter for one grid.
type slabKernel interface {
	// halo is the number of slices read beyond each side of the output.
	halo() int
	// scratchBytes bounds the memory run allocates for an output slab of
	// the given number of slices.
	scratchBytes(slices int) int64
	// run writes the output slices [z0, z1) into dst; in holds every slice
	// of the volume within the halo of them.
	run(ctx context.Context, in slabView, z0, z1 int, dst []float32) error
}

// SlabOptions configures FilterSlabs.
type SlabOptions struct {
	// MaxWorkingBytes bounds the buffers FilterSlabs allocates: the input
	// window (the slab and its halo), the output slab and the filter's
	// scratch. 0 selects DefaultSlabWorkingBytes. Slabs are thinned to fit,
	// down to one slice, which is the minimum whatever the bound.
	MaxWorkingBytes int64
}

// SlabResult describes a FilterSlabs run.
type SlabResult struct {
	// SlabSlices is the number of output slices per slab and HaloSlices
	// the slices read beyond each side of it.
	SlabSlices, HaloSlices int
	// Slabs is the number of slabs filtered.
	Slabs int
	// WorkingBytes bounds the memory allocated: the input window, the
	// output slab and the filter's scratch.
	WorkingBytes int64
}

// FilterSlabs filters a volume of dims samples with spacingMM, read slab by
// slab from src, into dst, without holding the volume: the result is
// bit-identical to filtering the whole volume (Median, Bilateral,
// NonLocalMeans or UnsharpMask with the same options). Slabs are written
// in increasing z. The samples must be finite; unlike the Volume filters,
// FilterSlabs does not scan them first. ctx is checked between groups of
// lines; on cancellation ctx.Err() is returned and the slices already
// written stay written.
func FilterSlabs(ctx context.Context, f SlabFilter, src SliceReader, dims [3]int, spacingMM [3]float64, dst SliceWriter, opts SlabOptions) (SlabResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if f == nil || src == nil || dst == nil {
		return SlabResult{}, fmt.Errorf("%w: nil filter, source or destination", ErrInvalidVolume)
	}
	grid := Volume{Dims: dims, SpacingMM: spacingMM}
	if _, ok := voxelCount(dims); !ok {
		return SlabResult{}, fmt.Errorf("%w: dimensions %v", ErrInvalidVolume, dims)
	}
	for axis, s := range spacingMM {
		if dims[axis] > 1 && !(s > 0 && s <= math.MaxFloat64) {
			return SlabResult{}, fmt.Errorf("%w: spacing %v mm along axis %d", ErrInvalidVolume, s, axis)
		}
	}
	k, err := f.slabKernel(grid.Dims, grid.SpacingMM)
	if err != nil {
		return SlabResult{}, err
	}
	budget := opts.MaxWorkingBytes
	if budget < 0 {
		return SlabResult{}, fmt.Errorf("%w: working bound %d bytes", ErrInvalidVolume, budget)
	}
	if budget == 0 {
		budget = DefaultSlabWorkingBytes
	}
	nz, nxy := dims[2], dims[0]*dims[1]
	halo := min(k.halo(), nz)
	sliceBytes := int64(nxy) * 4
	cost := func(slices int) int64 {
		window := min(nz, slices+2*halo)
		return int64(window+slices)*sliceBytes + k.scratchBytes(slices)
	}
	slab := 1
	for lo, hi := 1, nz; lo <= hi; {
		mid := lo + (hi-lo)/2
		if cost(mid) <= budget {
			slab, lo = mid, mid+1
		} else {
			hi = mid - 1
		}
	}
	res := SlabResult{SlabSlices: slab, HaloSlices: halo, WorkingBytes: cost(slab)}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	window := make([]float32, min(nz, slab+2*halo)*nxy)
	out := make([]float32, slab*nxy)
	held0, held1 := 0, 0 // the input slices in window
	for z0 := 0; z0 < nz; z0 += slab {
		z1 := min(nz, z0+slab)
		lo, hi := max(0, z0-halo), min(nz, z1+halo)
		if lo >= held0 && lo < held1 {
			// Keep the overlap with the previous window.
			copy(window, window[(lo-held0)*nxy:(held1-held0)*nxy])
		} else {
			held1 = lo
		}
		if hi > held1 {
			if err := src.ReadSlices(ctx, held1, hi, window[(held1-lo)*nxy:(hi-lo)*nxy]); err != nil {
				return res, err
			}
		}
		held0, held1 = lo, hi
		view := slabView{data: window[:(hi-lo)*nxy], dims: dims, lo: lo, hi: hi}
		part := out[:(z1-z0)*nxy]
		if err := k.run(ctx, view, z0, z1, part); err != nil {
			return res, err
		}
		if err := dst.WriteSlices(ctx, z0, part); err != nil {
			return res, err
		}
		res.Slabs++
	}
	return res, ctx.Err()
}

// workerBound is the number of workers a kernel may run.
func workerBound(requested int) int {
	if requested <= 0 {
		return parallel.DefaultWorkers()
	}
	return requested
}

// scratchPool keeps one scratch per worker of a kernel across the slabs of
// one request, so a slab allocates nothing once the first has run. A pool
// serves one run at a time.
type scratchPool[T any] struct {
	items []*T
	next  atomic.Int32
}

// runWithScratch runs fn for every item in [0, count) on up to workers
// goroutines, each with its own scratch from pool, checking ctx between
// items.
func runWithScratch[T any](ctx context.Context, count, workers int, pool *scratchPool[T], fn func(s *T, item int)) error {
	workers = max(1, min(workers, count))
	for len(pool.items) < workers {
		pool.items = append(pool.items, new(T))
	}
	pool.next.Store(0)
	return runParallel(ctx, count, workers, func() func(int) {
		s := pool.items[pool.next.Add(1)-1]
		return func(item int) { fn(s, item) }
	})
}

// slabView is a run of whole slices [lo, hi) of a volume of dims, the input
// of a neighbourhood kernel. Coordinates are those of the whole volume, so a
// kernel clamps or clips at the volume's borders, never at the slab's: a
// kernel reading at most halo slices beyond the slices it writes gives the
// same result on a slab holding them as on the whole volume.
type slabView struct {
	data                     []float32 // slices [lo, hi), x-fastest
	dims                     [3]int    // the whole volume
	lo, hi                   int
	regionOrigin, regionSize [3]int // nonzero size: compact regional storage, global coordinates
}

// wholeView is the view of a whole volume.
func wholeView(v *Volume) slabView {
	return slabView{data: v.Data, dims: v.Dims, lo: 0, hi: v.Dims[2]}
}

// row returns the index in data of sample (0, y, z); z must lie in [lo, hi).
func (s slabView) row(y, z int) int {
	if s.regionSize[0] != 0 {
		return ((z-s.regionOrigin[2])*s.regionSize[1]+y-s.regionOrigin[1])*s.regionSize[0] - s.regionOrigin[0]
	}
	return ((z-s.lo)*s.dims[1] + y) * s.dims[0]
}

// clampAxis returns c clamped to [0, n).
func clampAxis(c, n int) int {
	if c < 0 {
		return 0
	}
	if c >= n {
		return n - 1
	}
	return c
}

// radiusSamples converts a radius in mm along each axis into whole samples,
// rounding to the nearest (halves up); axes of a single sample get 0.
func radiusSamples(name string, radiusMM [3]float64, dims [3]int, spacingMM [3]float64) ([3]int, error) {
	var r [3]int
	for axis, mm := range radiusMM {
		if !(mm >= 0) || math.IsInf(mm, 0) {
			return r, fmt.Errorf("%w: %s radius %v mm along axis %d", ErrInvalidVolume, name, mm, axis)
		}
		if dims[axis] == 1 || mm == 0 {
			continue
		}
		samples := math.Floor(mm/spacingMM[axis] + 0.5)
		if samples > float64(dims[axis]) {
			// A box wider than the axis sees every sample of it, clamped.
			samples = float64(dims[axis])
		}
		r[axis] = int(samples)
	}
	return r, nil
}
