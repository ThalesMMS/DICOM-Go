package render

import (
	"context"
	"math"
	"sync"
)

// Empty-space skipping for the CPU ray caster (after VTK's
// vtkVolumeRayCastSpaceLeapingImageFilter). A min/max grid of 8³ voxel blocks
// bounds the trilinear samples in each block; a DVR sample whose block's value
// range maps to zero opacity, or a MIP/MinIP sample whose block cannot beat the
// current extreme, contributes nothing and is skipped without interpolation.
// The march still advances by repeated additions of the step, so the samples
// it does take sit where the full march puts them and images are identical.

const (
	vrBlockShift = 3 // 8³ voxel blocks
	vrBlockSize  = 1 << vrBlockShift
	// vrLeapMargin (voxels) keeps a leap inside the block despite the rounding
	// between the analytic ray and each sample's own voxel position.
	vrLeapMargin = 1e-6
)

// vrBlockGrid bounds, for each block, the trilinear samples whose base voxel
// floor(p) lies in the block: voxels 8b..8b+8 on each axis, clamped to the
// volume like the sampler's neighbours (#1005). A block holding a non-finite
// value has min NaN and is never skipped.
type vrBlockGrid struct {
	nx, ny, nz       int
	min, max         []float32
	gradientMu       sync.Mutex
	gradientMax      []float64
	gradientBuilding chan struct{}
}

// blockGrid returns the prepared volume's block grid, building it on first use
// from sampler (a sampler of this prepared generation). A cancelled build is
// not kept.
func (p *VRPreparedVolume) blockGrid(ctx context.Context, sampler *volumeSampler) (*vrBlockGrid, error) {
	p.blocksMu.Lock()
	defer p.blocksMu.Unlock()
	if p.blocks != nil {
		return p.blocks, nil
	}
	grid, err := buildVRBlockGrid(ctx, sampler)
	if err != nil {
		return nil, err
	}
	p.blocks = grid
	return grid, nil
}

func buildVRBlockGrid(ctx context.Context, s *volumeSampler) (*vrBlockGrid, error) {
	blocks := func(n int) int { return (n + vrBlockSize - 1) >> vrBlockShift }
	grid := &vrBlockGrid{nx: blocks(s.cols), ny: blocks(s.rows), nz: blocks(s.depth)}
	count := grid.nx * grid.ny * grid.nz
	grid.min, grid.max = make([]float32, count), make([]float32, count)
	// span is the voxel range of block b along an axis of n voxels.
	span := func(b, n int) (first, last int) {
		return b << vrBlockShift, min((b<<vrBlockShift)+vrBlockSize, n-1)
	}
	err := parallelRowsContext(ctx, grid.nz, func(bz int) {
		z0, z1 := span(bz, s.depth)
		for by := 0; by < grid.ny; by++ {
			y0, y1 := span(by, s.rows)
			for bx := 0; bx < grid.nx; bx++ {
				x0, x1 := span(bx, s.cols)
				minimum, maximum := math.Inf(1), math.Inf(-1)
				finiteBlock := true
				for z := z0; z <= z1 && finiteBlock; z++ {
					for y := y0; y <= y1 && finiteBlock; y++ {
						if len(s.values) > 0 {
							// Packed float32 rows: no per-voxel bounds or payload decoding.
							base := (z*s.rows + y) * s.cols
							for _, v32 := range s.values[base+x0 : base+x1+1] {
								v := float64(v32)
								if v != v || math.IsInf(v, 0) {
									finiteBlock = false
									break
								}
								if v < minimum {
									minimum = v
								}
								if v > maximum {
									maximum = v
								}
							}
							continue
						}
						for x := x0; x <= x1; x++ {
							v := s.valueOrZero(x, y, z)
							if !finite(v) {
								finiteBlock = false
								break
							}
							minimum = math.Min(minimum, v)
							maximum = math.Max(maximum, v)
						}
					}
				}
				i := (bz*grid.ny+by)*grid.nx + bx
				if !finiteBlock {
					grid.min[i], grid.max[i] = float32(math.NaN()), float32(math.NaN())
					continue
				}
				// float32 rounding outward keeps the bounds conservative.
				grid.min[i] = float32(minimum)
				if float64(grid.min[i]) > minimum {
					grid.min[i] = math.Nextafter32(grid.min[i], float32(math.Inf(-1)))
				}
				grid.max[i] = float32(maximum)
				if float64(grid.max[i]) < maximum {
					grid.max[i] = math.Nextafter32(grid.max[i], float32(math.Inf(1)))
				}
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return grid, nil
}

// widen covers the rounding of trilinear interpolation, whose result may
// leave its corners' range by a few ulps.
func widen(minimum, maximum float64) (float64, float64) {
	return minimum - 1e-9*(1+math.Abs(minimum)), maximum + 1e-9*(1+math.Abs(maximum))
}

// Equal bounds identify an exactly constant interpolation stencil. Lerp of
// equal finite endpoints returns that endpoint, so widening would invent
// opacity immediately above a zero LUT endpoint (notably PET background).
func interpolationBounds(lo, hi float64) (float64, float64) {
	if lo == hi && finite(lo) {
		return lo, hi
	}
	return widen(lo, hi)
}

// vrSpaceLeap is one render's skipping state: the grid and, for DVR, the
// blocks whose samples are all transparent under the transfer function.
type vrSpaceLeap struct {
	grid  *vrBlockGrid
	scale Vec3 // texture to voxel coordinates: dims-1
	// empty marks DVR blocks of zero opacity.
	empty []bool
	// window maps values for MIP/MinIP; monotone is false when it is not
	// known to be non-decreasing, which disables projection skipping.
	window   WindowLevel
	monotone bool
	// valueRange, when enabled, skips Average/RaySum blocks outside it.
	valueRange VRValueRange
}

func newVRSpaceLeap(grid *vrBlockGrid, s *volumeSampler) *vrSpaceLeap {
	return &vrSpaceLeap{grid: grid, scale: Vec3{X: float64(s.cols - 1), Y: float64(s.rows - 1), Z: float64(s.depth - 1)}}
}

// forDVR marks the blocks whose every sample has zero opacity: the LUT
// entries a block's (monotone) coordinate range can interpolate all have
// alpha 0, and Lookup interpolating two zeros returns exactly 0.
func (l *vrSpaceLeap) forDVR(tf VRTransferFunction, lut VRLUT, window WindowLevel, dataMin, dataMax float64) {
	n := len(lut.entries)
	if n == 0 {
		return
	}
	opaque := make([]int, n+1) // opaque[i]: entries before i with alpha > 0
	for i, e := range lut.entries {
		opaque[i+1] = opaque[i]
		if !(e.A == 0) {
			opaque[i+1]++
		}
	}
	coordinate := func(v float64) float64 {
		if tf.Domain() == VRTransferDomainNormalized {
			return tf.windowCoordinate(v, window)
		}
		return normalizedVRDataCoordinate(v, dataMin, dataMax)
	}
	index := func(c float64) int {
		switch {
		case c <= 0:
			return 0
		case c >= 1:
			return n - 1
		}
		return int(math.Floor(c * float64(n-1)))
	}
	l.empty = make([]bool, len(l.grid.min))
	for i := range l.empty {
		lo, hi := float64(l.grid.min[i]), float64(l.grid.max[i])
		if !finite(lo) || !finite(hi) {
			continue
		}
		lo, hi = interpolationBounds(lo, hi)
		c0, c1 := coordinate(lo), coordinate(hi)
		if !finite(c0) || !finite(c1) || c0 > c1 {
			continue
		}
		first, last := index(c0), index(c1)
		if c1 > 0 && c1 < 1 {
			last = int(math.Ceil(c1 * float64(n-1)))
		}
		if last > n-1 {
			last = n - 1
		}
		l.empty[i] = opaque[last+1]-opaque[first] == 0
	}
}

// forDVR2D marks the blocks whose every sample is transparent under a 2D
// transfer function (#1232). It first bounds every gradient; optional physical
// gradient maxima restrict the table rectangle without changing sample positions.
func (l *vrSpaceLeap) forDVR2D(transfer *VRTransfer2D, gradients ...[]float64) {
	n := transfer.width
	opaque := make([]int, n+1) // opaque[i]: columns before i with alpha > 0
	for i, alpha := range transfer.columnMaxAlpha {
		opaque[i+1] = opaque[i]
		if !(alpha == 0) {
			opaque[i+1]++
		}
	}
	l.empty = make([]bool, len(l.grid.min))
	for i := range l.empty {
		lo, hi := float64(l.grid.min[i]), float64(l.grid.max[i])
		if !finite(lo) || !finite(hi) {
			continue
		}
		lo, hi = interpolationBounds(lo, hi)
		first, last := int(transfer.columnPosition(lo)), int(math.Ceil(transfer.columnPosition(hi)))
		if last > n-1 {
			last = n - 1
		}
		l.empty[i] = opaque[last+1]-opaque[first] == 0
	}
	if len(gradients) != 0 {
		l.limitTransfer2DGradients(transfer, gradients[0])
	}
}

// forProjection enables MIP/MinIP skipping for a linear or sigmoid window of
// positive width (non-decreasing in the value).
func (l *vrSpaceLeap) forProjection(window WindowLevel) {
	l.window = window
	l.monotone = window.Width > 0 && finite(window.Center) && finite(window.Width)
}

// block returns the index of the block holding the sample at tex, computing
// the voxel position exactly as textureAt does, or -1 outside the grid.
func (l *vrSpaceLeap) block(tex Vec3) int {
	x := int(math.Floor(tex.X * l.scale.X))
	y := int(math.Floor(tex.Y * l.scale.Y))
	z := int(math.Floor(tex.Z * l.scale.Z))
	if x < 0 || y < 0 || z < 0 {
		return -1
	}
	x, y, z = x>>vrBlockShift, y>>vrBlockShift, z>>vrBlockShift
	if x >= l.grid.nx || y >= l.grid.ny || z >= l.grid.nz {
		return -1
	}
	return (z*l.grid.ny+y)*l.grid.nx + x
}

// skipsProjection reports whether no sample of block can change the MIP
// (MinIP) extreme acc.
func (l *vrSpaceLeap) skipsProjection(block int, mode VRMode, acc float64) bool {
	if !l.monotone {
		return false
	}
	lo, hi := float64(l.grid.min[block]), float64(l.grid.max[block])
	if math.IsNaN(lo) {
		return false
	}
	lo, hi = widen(lo, hi)
	switch mode {
	case VRModeMIP:
		return windowedUnit(hi, l.window) <= acc
	case VRModeMinIP:
		return windowedUnit(lo, l.window) >= acc
	}
	return false
}

// leap advances t over the following samples that stay in block (with
// vrLeapMargin on every face), by the same additions as the march, and
// returns the last skipped parameter; the march's own increment then takes
// the first sample past them. a and b give the voxel position a+t·b.
func (l *vrSpaceLeap) leap(block int, a, b Vec3, t, t1, step float64) float64 {
	bx := block % l.grid.nx
	by := (block / l.grid.nx) % l.grid.ny
	bz := block / (l.grid.nx * l.grid.ny)
	tIn, tOut := math.Inf(-1), math.Inf(1)
	for _, axis := range [3][3]float64{{a.X, b.X, float64(bx)}, {a.Y, b.Y, float64(by)}, {a.Z, b.Z, float64(bz)}} {
		lower := axis[2]*vrBlockSize + vrLeapMargin
		upper := (axis[2]+1)*vrBlockSize - vrLeapMargin
		if axis[1] == 0 {
			if axis[0] < lower || axis[0] > upper {
				return t
			}
			continue
		}
		enter, exit := (lower-axis[0])/axis[1], (upper-axis[0])/axis[1]
		if enter > exit {
			enter, exit = exit, enter
		}
		tIn, tOut = math.Max(tIn, enter), math.Min(tOut, exit)
	}
	if t < tIn {
		return t
	}
	for {
		next := t + step
		if next >= tOut || next > t1 {
			return t
		}
		t = next
	}
}

// rayVoxel returns a and b with the voxel position of o+t·d equal to a+t·b.
func (l *vrSpaceLeap) rayVoxel(o, d, boxMin, size Vec3) (a, b Vec3) {
	a = Vec3{X: (o.X - boxMin.X) / size.X * l.scale.X, Y: (o.Y - boxMin.Y) / size.Y * l.scale.Y, Z: (o.Z - boxMin.Z) / size.Z * l.scale.Z}
	b = Vec3{X: d.X / size.X * l.scale.X, Y: d.Y / size.Y * l.scale.Y, Z: d.Z / size.Z * l.scale.Z}
	return a, b
}
