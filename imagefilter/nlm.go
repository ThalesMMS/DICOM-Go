package imagefilter

// Block-wise non-local means denoising (#1186).
//
// NonLocalMeans is the blockwise non-local means filter of P. Coupé, P.
// Yger, S. Prima, P. Hellier, C. Kervrann and C. Barillot ("An Optimized
// Blockwise Nonlocal Means Denoising Filter for 3-D Magnetic Resonance
// Images", IEEE TMI 27(4), 2008), after the pixelwise filter of A. Buades,
// B. Coll and J.-M. Morel (2005), written from the papers. Blocks Bᵢ of
// (2r+1) samples per axis are centred on a grid with a step of n samples.
// Each block is restored as a whole: Â(Bᵢ) = Σⱼ w(i, j)·u(Bⱼ) / Σⱼ w(i, j)
// over the blocks Bⱼ centred on every sample of the search window around
// Bᵢ (clipped to the volume), with
//
//	w(i, j) = exp(−‖u(Bᵢ) − u(Bⱼ)‖² / (|B|·h²)),   h² = 2·β·σ²,
//
// σ the noise standard deviation and |B| the samples of a block, so two
// noisy copies of the same patch, whose mean squared difference is 2σ²,
// weigh e^(−1/β). The block's own weight is the largest weight of the
// others (1 if none), so it does not dominate. Each sample is then the
// mean of the estimates of the blocks covering it; with n ≤ 2r+1 every
// sample is covered, the first and last samples of each axis being block
// centres. Patch samples beyond the volume replicate its edge. A candidate
// whose squared distance passes 18·|B|·h² (a weight below 1.6·10⁻⁸) is
// dropped as soon as its partial sum exceeds that, which saves most of the
// work across edges.
//
// ITK's PatchBasedDenoisingImageFilter (an entropy-minimizing method with
// stochastic patch sampling) was a behaviour reference for the patch radius
// and the noise model only; no ITK code is used. The cost per block is the
// search window times the block size, about (2s+1)^d·(2r+1)^d/n^d
// operations per sample for a search radius s, so a 512×512 slice with the
// defaults (r = 1, s = 5, n = 2) takes some 2·10⁸ operations, and 3-D
// volumes are best denoised with a small search radius along z, or slice by
// slice. Blocks are computed one plane of block centres at a time and the
// samples of a slice are averaged as soon as every block covering them is
// done, so the working memory is a few block planes whatever the depth, and
// the filter streams through FilterSlabs with a halo of 2·r_z + s_z slices.

import (
	"context"
	"fmt"
	"math"
)

// NonLocalMeansOptions configures NonLocalMeans.
type NonLocalMeansOptions struct {
	// NoiseSigma is the standard deviation of the noise in modality units
	// (HU for CT); it must be positive. A robust estimate is the median
	// absolute difference of horizontal neighbours divided by 0.6745·√2.
	NoiseSigma float64
	// Beta scales the smoothing, h² = 2·Beta·NoiseSigma²; 1 when zero.
	Beta float64
	// PatchRadiusMM is the block (patch) half-width in mm per axis,
	// rounded to whole samples like MedianOptions.RadiusMM. When all are
	// zero the radius is one sample along every axis of more than one
	// sample (3×3 blocks in a slice, 3×3×3 in a volume).
	PatchRadiusMM [3]float64
	// SearchRadiusMM is the search window half-width in mm per axis,
	// rounded to whole samples. When all are zero it is five samples along
	// every axis of more than one sample. A zero z radius with a zero z
	// patch radius denoises each slice of a volume on its own.
	SearchRadiusMM [3]float64
	// BlockStep is the distance between block centres in samples, along
	// every axis where the patch radius is positive (1 elsewhere); 2 when
	// zero, at most 2·radius+1 so every sample is covered.
	BlockStep int
	// MaxSearchSamples bounds the search window; 1<<16 when zero.
	MaxSearchSamples int
	// Workers bounds the goroutines (0 selects the shared concurrency budget). The result does
	// not depend on the worker count.
	Workers int
}

// NonLocalMeans denoises src by blockwise non-local means: each block is
// replaced by the average of the similar blocks in its search window,
// weighted by patch similarity, which removes Gaussian-like noise of
// low-dose CT and MR while keeping edges and repeated texture. The result
// is a display and analysis aid, not a measurement. ctx is checked between
// rows of blocks; on cancellation ctx.Err() is returned and no result.
func NonLocalMeans(ctx context.Context, src *Volume, opts NonLocalMeansOptions) (*Volume, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := src.Validate(); err != nil {
		return nil, err
	}
	k, err := newNLMKernel(src.Dims, src.SpacingMM, opts)
	if err != nil {
		return nil, err
	}
	out := &Volume{Dims: src.Dims, SpacingMM: src.SpacingMM, Data: make([]float32, len(src.Data))}
	if err := k.run(ctx, wholeView(src), 0, src.Dims[2], out.Data); err != nil {
		return nil, err
	}
	return out, nil
}

// nlmKernel is a validated NonLocalMeans request for one grid.
type nlmKernel struct {
	dims    [3]int
	patch   [3]int // block radius in samples
	search  [3]int // search radius in samples
	step    [3]int
	cutoff  float64 // squared distance per sample beyond which a block is dropped
	invH2   float64 // 1/h²
	workers int

	// Per axis: the block centres and, per coordinate, the first and last
	// block covering it.
	centres    [3][]int
	coverLo    [3][]int
	coverHi    [3][]int
	blockLen   int   // samples per block
	rowOffsets []int // per block row (dz, dy): its offset from the block's first row
	ring       int   // block planes held at once

	planes  [][]float32 // ring of block-plane estimates, [ky][kx][block sample]
	scratch scratchPool[nlmScratch]
}

// nlmScratch is one worker's block buffers.
type nlmScratch struct {
	patch, other []float32
	patch64, acc []float64
}

func (opts NonLocalMeansOptions) slabKernel(dims [3]int, spacingMM [3]float64) (slabKernel, error) {
	return newNLMKernel(dims, spacingMM, opts)
}

func newNLMKernel(dims [3]int, spacingMM [3]float64, opts NonLocalMeansOptions) (*nlmKernel, error) {
	if !(opts.NoiseSigma > 0) || math.IsInf(opts.NoiseSigma, 0) {
		return nil, fmt.Errorf("%w: noise sigma %v", ErrInvalidVolume, opts.NoiseSigma)
	}
	if opts.Beta == 0 {
		opts.Beta = 1
	}
	if !(opts.Beta > 0) || math.IsInf(opts.Beta, 0) {
		return nil, fmt.Errorf("%w: beta %v", ErrInvalidVolume, opts.Beta)
	}
	if opts.BlockStep == 0 {
		opts.BlockStep = 2
	}
	if opts.BlockStep < 0 {
		return nil, fmt.Errorf("%w: block step %d", ErrInvalidVolume, opts.BlockStep)
	}
	if opts.MaxSearchSamples == 0 {
		opts.MaxSearchSamples = 1 << 16
	}
	k := &nlmKernel{dims: dims, workers: opts.Workers}
	var err error
	if k.patch, err = radiusOrDefault("patch", opts.PatchRadiusMM, 1, dims, spacingMM); err != nil {
		return nil, err
	}
	if k.search, err = radiusOrDefault("search", opts.SearchRadiusMM, 5, dims, spacingMM); err != nil {
		return nil, err
	}
	searchSamples, blockLen := 1, 1
	for axis := 0; axis < 3; axis++ {
		searchSamples *= 2*k.search[axis] + 1
		blockLen *= 2*k.patch[axis] + 1
		if searchSamples > opts.MaxSearchSamples {
			return nil, fmt.Errorf("%w: the search window exceeds %d samples", ErrInvalidVolume, opts.MaxSearchSamples)
		}
		k.step[axis] = min(opts.BlockStep, 2*k.patch[axis]+1)
		k.centres[axis], k.coverLo[axis], k.coverHi[axis] = blockCover(dims[axis], k.patch[axis], k.step[axis])
	}
	k.blockLen = blockLen
	for dz := -k.patch[2]; dz <= k.patch[2]; dz++ {
		for dy := -k.patch[1]; dy <= k.patch[1]; dy++ {
			k.rowOffsets = append(k.rowOffsets, (dz*dims[1]+dy)*dims[0])
		}
	}
	h2 := 2 * opts.Beta * opts.NoiseSigma * opts.NoiseSigma
	if !(h2 > 0) || math.IsInf(h2, 0) {
		return nil, fmt.Errorf("%w: noise sigma %v and beta %v", ErrInvalidVolume, opts.NoiseSigma, opts.Beta)
	}
	k.invH2 = 1 / h2
	k.cutoff = nlmCutoff * h2
	for z := range k.coverLo[2] {
		k.ring = max(k.ring, k.coverHi[2][z]-k.coverLo[2][z]+1)
	}
	return k, nil
}

// nlmCutoff is the normalized squared distance, in h², beyond which a
// candidate block is dropped: exp(−18) ≈ 1.5·10⁻⁸.
const nlmCutoff = 18

// radiusOrDefault converts a radius in mm, or when all axes are zero gives
// def samples along every axis of more than one sample.
func radiusOrDefault(name string, mm [3]float64, def int, dims [3]int, spacingMM [3]float64) ([3]int, error) {
	if mm == ([3]float64{}) {
		var r [3]int
		for axis := range r {
			if dims[axis] > 1 {
				r[axis] = min(def, dims[axis])
			}
		}
		return r, nil
	}
	return radiusSamples(name, mm, dims, spacingMM)
}

// blockCover returns the block centres along an axis of n samples (0, step,
// 2·step, … and n−1) and, per coordinate, the first and last centre whose
// block of the given radius covers it.
func blockCover(n, radius, step int) (centres, lo, hi []int) {
	for c := 0; ; c += step {
		if c >= n-1 {
			centres = append(centres, n-1)
			break
		}
		centres = append(centres, c)
	}
	lo, hi = make([]int, n), make([]int, n)
	first := 0
	for p := 0; p < n; p++ {
		for centres[first] < p-radius {
			first++
		}
		last := first
		for last+1 < len(centres) && centres[last+1] <= p+radius {
			last++
		}
		lo[p], hi[p] = first, last
	}
	return centres, lo, hi
}

// halo is the z reach of a block plane's reads: its patch radius around
// the candidates of the search window, for the planes covering a slice.
func (k *nlmKernel) halo() int { return 2*k.patch[2] + k.search[2] }

// scratchBytes is the ring of block planes and the per-worker block
// buffers.
func (k *nlmKernel) scratchBytes(int) int64 {
	plane := int64(len(k.centres[0])) * int64(len(k.centres[1])) * int64(k.blockLen) * 4
	perWorker := int64(k.blockLen) * (4 + 4 + 8)
	return int64(k.ring)*plane + int64(min(workerBound(k.workers), len(k.centres[1])))*perWorker
}

// run denoises the output slices [z0, z1): it computes the block planes
// covering them in increasing z and averages each slice as soon as the
// last plane covering it is done.
func (k *nlmKernel) run(ctx context.Context, in slabView, z0, z1 int, dst []float32) error {
	nx, ny := k.dims[0], k.dims[1]
	nbx, nby := len(k.centres[0]), len(k.centres[1])
	planeLen := nbx * nby * k.blockLen
	if len(k.planes) != k.ring {
		k.planes = make([][]float32, k.ring)
		for i := range k.planes {
			k.planes[i] = make([]float32, planeLen)
		}
	}
	next := z0 // the first slice not yet averaged
	for kz := k.coverLo[2][z0]; kz <= k.coverHi[2][z1-1]; kz++ {
		plane := k.planes[kz%k.ring]
		cz := k.centres[2][kz]
		if err := runWithScratch(ctx, nby, workerCount(k.workers, nby), &k.scratch, func(s *nlmScratch, ky int) {
			k.blockRow(in, s, plane[ky*nbx*k.blockLen:(ky+1)*nbx*k.blockLen], k.centres[1][ky], cz)
		}); err != nil {
			return err
		}
		for ; next < z1 && k.coverHi[2][next] == kz; next++ {
			z := next
			out := dst[(z-z0)*nx*ny : (z-z0+1)*nx*ny]
			if err := parallelLines(ctx, ny, workerCount(k.workers, ny), func(y int) {
				k.average(out[y*nx:(y+1)*nx], y, z)
			}); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

// average writes the mean of the block estimates covering each sample of
// row y of slice z.
func (k *nlmKernel) average(out []float32, y, z int) {
	nbx := len(k.centres[0])
	bx, by := 2*k.patch[0]+1, 2*k.patch[1]+1
	zLo, zHi := k.coverLo[2][z], k.coverHi[2][z]
	yLo, yHi := k.coverLo[1][y], k.coverHi[1][y]
	for x := range out {
		xLo, xHi := k.coverLo[0][x], k.coverHi[0][x]
		sum := 0.0
		for kz := zLo; kz <= zHi; kz++ {
			plane := k.planes[kz%k.ring]
			oz := z - k.centres[2][kz] + k.patch[2]
			for ky := yLo; ky <= yHi; ky++ {
				oy := y - k.centres[1][ky] + k.patch[1]
				row := ky * nbx
				for kx := xLo; kx <= xHi; kx++ {
					ox := x - k.centres[0][kx] + k.patch[0]
					sum += float64(plane[(row+kx)*k.blockLen+(oz*by+oy)*bx+ox])
				}
			}
		}
		count := (zHi - zLo + 1) * (yHi - yLo + 1) * (xHi - xLo + 1)
		out[x] = float32(sum / float64(count))
	}
}

// blockRow restores the blocks of one row of centres (y = cy, z = cz) into
// dst, block after block.
func (k *nlmKernel) blockRow(in slabView, s *nlmScratch, dst []float32, cy, cz int) {
	nx, ny, nz := k.dims[0], k.dims[1], k.dims[2]
	if len(s.patch) != k.blockLen {
		s.patch = make([]float32, k.blockLen)
		s.patch64 = make([]float64, k.blockLen)
		s.other = make([]float32, k.blockLen)
		s.acc = make([]float64, k.blockLen)
	}
	rx, ry, rz := k.patch[0], k.patch[1], k.patch[2]
	sx, sy, sz := k.search[0], k.search[1], k.search[2]
	width := 2*rx + 1
	limit := k.cutoff * float64(k.blockLen)
	for kx, cx := range k.centres[0] {
		k.gather(in, s.patch, cx, cy, cz)
		for i, v := range s.patch {
			s.patch64[i] = float64(v)
			s.acc[i] = 0
		}
		weights, best := 0.0, 0.0
		for oz := max(-sz, -cz); oz <= min(sz, nz-1-cz); oz++ {
			qz := cz + oz
			for oy := max(-sy, -cy); oy <= min(sy, ny-1-cy); oy++ {
				qy := cy + oy
				for ox := max(-sx, -cx); ox <= min(sx, nx-1-cx); ox++ {
					if ox == 0 && oy == 0 && oz == 0 {
						continue
					}
					qx := cx + ox
					// An interior block is read in place, row by row; one
					// that crosses the border is gathered with replication.
					// The squared distance is summed in the same order
					// either way, and a block is dropped once a partial sum
					// passes the cutoff (partial sums only grow).
					interior := qx >= rx && qx+rx < nx && qy >= ry && qy+ry < ny && qz >= rz && qz+rz < nz
					base := 0
					if interior {
						base = in.row(qy, qz) + qx - rx
					} else {
						k.gather(in, s.other, qx, qy, qz)
					}
					d := 0.0
					for r, off := range k.rowOffsets {
						var seg []float32
						if interior {
							seg = in.data[base+off : base+off+width]
						} else {
							seg = s.other[r*width : (r+1)*width]
						}
						p := s.patch64[r*width : (r+1)*width]
						for j, v := range seg {
							diff := p[j] - float64(v)
							d += diff * diff
						}
						if d > limit {
							break
						}
					}
					if d > limit {
						continue
					}
					w := math.Exp(-d / float64(k.blockLen) * k.invH2)
					best = max(best, w)
					weights += w
					for r, off := range k.rowOffsets {
						var seg []float32
						if interior {
							seg = in.data[base+off : base+off+width]
						} else {
							seg = s.other[r*width : (r+1)*width]
						}
						acc := s.acc[r*width : (r+1)*width]
						for j, v := range seg {
							acc[j] += w * float64(v)
						}
					}
				}
			}
		}
		if best == 0 {
			best = 1
		}
		weights += best
		block := dst[kx*k.blockLen : (kx+1)*k.blockLen]
		for i, v := range s.patch64 {
			block[i] = float32((s.acc[i] + best*v) / weights)
		}
	}
}

// gather copies the block centred at (cx, cy, cz) into dst, replicating the
// volume's edge samples.
func (k *nlmKernel) gather(in slabView, dst []float32, cx, cy, cz int) {
	nx, ny, nz := k.dims[0], k.dims[1], k.dims[2]
	rx, ry, rz := k.patch[0], k.patch[1], k.patch[2]
	i := 0
	for dz := -rz; dz <= rz; dz++ {
		z := clampAxis(cz+dz, nz)
		for dy := -ry; dy <= ry; dy++ {
			row := in.row(clampAxis(cy+dy, ny), z)
			for dx := -rx; dx <= rx; dx++ {
				dst[i] = in.data[row+clampAxis(cx+dx, nx)]
				i++
			}
		}
	}
}
