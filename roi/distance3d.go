package roi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"
)

// Exact 3D Euclidean distance transform in millimetres and the morphology
// built on it (#1017): expand and shrink by a margin in mm, opening,
// closing and hole filling. The transform is Felzenszwalb and Huttenlocher's
// separable lower envelope of parabolas, run along x, y and z with each
// axis's voxel coordinates in mm, so anisotropic spacing and irregular slice
// positions give exact distances.

// ErrInvalidVolumeMask reports a mask whose size, dimensions or coordinates
// are inconsistent.
var ErrInvalidVolumeMask = errors.New("roi: invalid volume mask")

// VolumeMask is a dense binary volume, x-fastest (index (z*Rows+y)*Columns
// + x), with the voxel-centre coordinates in mm of each axis.
type VolumeMask struct {
	Columns, Rows, Slices int
	// X, Y and Z hold Columns, Rows and Slices ascending coordinates (mm).
	X, Y, Z []float64
	Voxels  []bool
}

// NewVolumeMask returns an empty mask with uniform spacing (mm).
func NewVolumeMask(columns, rows, slices int, spacing [3]float64) *VolumeMask {
	axis := func(n int, step float64) []float64 {
		out := make([]float64, n)
		for i := range out {
			out[i] = float64(i) * step
		}
		return out
	}
	return &VolumeMask{
		Columns: columns, Rows: rows, Slices: slices,
		X: axis(columns, spacing[0]), Y: axis(rows, spacing[1]), Z: axis(slices, spacing[2]),
		Voxels: make([]bool, columns*rows*slices),
	}
}

func (m *VolumeMask) validate() error { return m.validateContext(context.Background()) }

func (m *VolumeMask) validateContext(ctx context.Context) error {
	if m == nil || m.Columns <= 0 || m.Rows <= 0 || m.Slices <= 0 ||
		len(m.X) != m.Columns || len(m.Y) != m.Rows || len(m.Z) != m.Slices ||
		len(m.Voxels) != m.Columns*m.Rows*m.Slices {
		return ErrInvalidVolumeMask
	}
	for _, axis := range [][]float64{m.X, m.Y, m.Z} {
		for i, v := range axis {
			if i%4096 == 0 && ctx.Err() != nil {
				return ctx.Err()
			}
			if math.IsNaN(v) || math.IsInf(v, 0) || i > 0 && v <= axis[i-1] {
				return fmt.Errorf("%w: coordinates must be finite and ascending", ErrInvalidVolumeMask)
			}
		}
	}
	return nil
}

func (m *VolumeMask) index(x, y, z int) int { return (z*m.Rows+y)*m.Columns + x }

// Count returns the number of voxels set.
func (m *VolumeMask) Count() int {
	n := 0
	for _, v := range m.Voxels {
		if v {
			n++
		}
	}
	return n
}

// Clone returns a deep copy.
func (m *VolumeMask) Clone() *VolumeMask {
	c := *m
	c.X, c.Y, c.Z = append([]float64(nil), m.X...), append([]float64(nil), m.Y...), append([]float64(nil), m.Z...)
	c.Voxels = append([]bool(nil), m.Voxels...)
	return &c
}

// DistanceTransform returns, for every voxel, the Euclidean distance in mm
// to the nearest voxel whose Voxels value equals target (+Inf when there is
// none).
func (m *VolumeMask) DistanceTransform(ctx context.Context, target bool) ([]float64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := m.validateContext(ctx); err != nil {
		return nil, err
	}
	d := make([]float64, len(m.Voxels))
	for i, v := range m.Voxels {
		if i%4096 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if v == target {
			d[i] = 0
		} else {
			d[i] = math.Inf(1)
		}
	}
	nx, ny, nz := m.Columns, m.Rows, m.Slices
	// Squared distances along x, then y, then z.
	passes := []struct {
		n, lines int
		coords   []float64
		at       func(line, k int) int
	}{
		{nx, ny * nz, m.X, func(line, k int) int { return line*nx + k }},
		{ny, nx * nz, m.Y, func(line, k int) int { return (line/nx*ny+k)*nx + line%nx }},
		{nz, nx * ny, m.Z, func(line, k int) int { return k*nx*ny + line }},
	}
	for _, pass := range passes {
		if err := parallelLines(ctx, pass.lines, func() func(int) {
			f := make([]float64, pass.n)
			out := make([]float64, pass.n)
			hull := make([]int, pass.n)
			bounds := make([]float64, pass.n+1)
			return func(line int) {
				for k := 0; k < pass.n; k++ {
					if k%4096 == 0 && ctx.Err() != nil {
						return
					}
					f[k] = d[pass.at(line, k)]
				}
				if squaredDistance1DContext(ctx, out, f, pass.coords, hull, bounds) != nil {
					return
				}
				for k := 0; k < pass.n; k++ {
					if k%4096 == 0 && ctx.Err() != nil {
						return
					}
					d[pass.at(line, k)] = out[k]
				}
			}
		}); err != nil {
			return nil, err
		}
	}
	for i, v := range d {
		if i%4096 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		d[i] = math.Sqrt(v)
	}
	return d, nil
}

// squaredDistance1D is the 1D squared-distance transform over sorted
// coordinates c: out[q] = min_p f[p] + (c[q]-c[p])², by the lower envelope of
// the parabolas rooted at the finite f[p].
func squaredDistance1D(out, f, c []float64, hull []int, bounds []float64) {
	_ = squaredDistance1DContext(context.Background(), out, f, c, hull, bounds)
}

func squaredDistance1DContext(ctx context.Context, out, f, c []float64, hull []int, bounds []float64) error {
	n := len(f)
	k := -1
	innerSteps := 0
	for p := 0; p < n; p++ {
		if p%4096 == 0 && ctx.Err() != nil {
			return ctx.Err()
		}
		if math.IsInf(f[p], 1) {
			continue
		}
		for k >= 0 {
			innerSteps++
			if innerSteps%4096 == 0 && ctx.Err() != nil {
				return ctx.Err()
			}
			q := hull[k]
			s := ((f[p] + c[p]*c[p]) - (f[q] + c[q]*c[q])) / (2 * (c[p] - c[q]))
			if s <= bounds[k] {
				k--
				continue
			}
			k++
			hull[k], bounds[k] = p, s
			break
		}
		if k < 0 {
			k = 0
			hull[0], bounds[0] = p, math.Inf(-1)
		}
		bounds[k+1] = math.Inf(1)
	}
	if k < 0 {
		for q := range out {
			if q%4096 == 0 && ctx.Err() != nil {
				return ctx.Err()
			}
			out[q] = math.Inf(1)
		}
		return ctx.Err()
	}
	j := 0
	for q := 0; q < n; q++ {
		if q%4096 == 0 && ctx.Err() != nil {
			return ctx.Err()
		}
		for bounds[j+1] < c[q] {
			innerSteps++
			if innerSteps%4096 == 0 && ctx.Err() != nil {
				return ctx.Err()
			}
			j++
		}
		p := hull[j]
		dx := c[q] - c[p]
		out[q] = dx*dx + f[p]
	}
	return ctx.Err()
}

// parallelLines runs work over lines with one worker state per goroutine.
func parallelLines(ctx context.Context, lines int, worker func() func(int)) error {
	workers := min(max(1, lines/16), 16)
	var next atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			do := worker()
			for {
				line := int(next.Add(1) - 1)
				if line >= lines || ctx.Err() != nil {
					return
				}
				do(line)
			}
		}()
	}
	wg.Wait()
	return ctx.Err()
}

// Expand returns the voxels within marginMM (Euclidean, mm) of the mask:
// dilation by a ball, as radiotherapy margins.
func (m *VolumeMask) Expand(ctx context.Context, marginMM float64) (*VolumeMask, error) {
	if !(marginMM >= 0) || math.IsInf(marginMM, 0) {
		return nil, fmt.Errorf("%w: margin %v mm", ErrInvalidVolumeMask, marginMM)
	}
	d, err := m.DistanceTransform(ctx, true)
	if err != nil {
		return nil, err
	}
	out := m.Clone()
	for i, v := range d {
		out.Voxels[i] = v <= marginMM
	}
	return out, nil
}

// Shrink returns the mask voxels farther than marginMM from every voxel
// outside it: erosion by a ball. The volume border counts as inside, so a
// mask touching it is not eroded from there.
func (m *VolumeMask) Shrink(ctx context.Context, marginMM float64) (*VolumeMask, error) {
	if !(marginMM >= 0) || math.IsInf(marginMM, 0) {
		return nil, fmt.Errorf("%w: margin %v mm", ErrInvalidVolumeMask, marginMM)
	}
	d, err := m.DistanceTransform(ctx, false)
	if err != nil {
		return nil, err
	}
	out := m.Clone()
	for i, v := range d {
		out.Voxels[i] = m.Voxels[i] && v > marginMM
	}
	return out, nil
}

// Open shrinks then expands by radiusMM, removing structures thinner than
// the ball.
func (m *VolumeMask) Open(ctx context.Context, radiusMM float64) (*VolumeMask, error) {
	shrunk, err := m.Shrink(ctx, radiusMM)
	if err != nil {
		return nil, err
	}
	return shrunk.Expand(ctx, radiusMM)
}

// Close expands then shrinks by radiusMM, bridging gaps narrower than the
// ball.
func (m *VolumeMask) Close(ctx context.Context, radiusMM float64) (*VolumeMask, error) {
	expanded, err := m.Expand(ctx, radiusMM)
	if err != nil {
		return nil, err
	}
	return expanded.Shrink(ctx, radiusMM)
}

// FillHoles sets the background voxels not 6-connected to the volume border
// (enclosed cavities). Cancellation returns a nil mask and the context error.
func (m *VolumeMask) FillHoles(ctx context.Context) (*VolumeMask, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := m.validateContext(ctx); err != nil {
		return nil, err
	}
	nx, ny, nz := m.Columns, m.Rows, m.Slices
	outside := make([]bool, len(m.Voxels))
	stack := make([]int, 0, 1024)
	push := func(x, y, z int) {
		i := m.index(x, y, z)
		if !m.Voxels[i] && !outside[i] {
			outside[i] = true
			stack = append(stack, i)
		}
	}
	for z := 0; z < nz; z++ {
		for y := 0; y < ny; y++ {
			for x := 0; x < nx; x++ {
				if m.index(x, y, z)%4096 == 0 && ctx.Err() != nil {
					return nil, ctx.Err()
				}
				if x == 0 || y == 0 || z == 0 || x == nx-1 || y == ny-1 || z == nz-1 {
					push(x, y, z)
				}
			}
		}
	}
	for steps := 0; len(stack) > 0; steps++ {
		if steps%65536 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		i := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		x, y, z := i%nx, i/nx%ny, i/(nx*ny)
		if x > 0 {
			push(x-1, y, z)
		}
		if x < nx-1 {
			push(x+1, y, z)
		}
		if y > 0 {
			push(x, y-1, z)
		}
		if y < ny-1 {
			push(x, y+1, z)
		}
		if z > 0 {
			push(x, y, z-1)
		}
		if z < nz-1 {
			push(x, y, z+1)
		}
	}
	out := m.Clone()
	for i := range out.Voxels {
		if i%4096 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		out.Voxels[i] = !outside[i]
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// VolumeMask returns the segmentation as a dense mask over its geometry's
// slices, with the in-plane spacing of the first slice and the real slice
// positions.
func (s *Segmentation3D) VolumeMask() (*VolumeMask, error) {
	if s == nil || s.Columns <= 0 || s.Rows <= 0 || len(s.Geometry.Slices) == 0 {
		return nil, ErrInvalidVolumeMask
	}
	slices := len(s.Geometry.Slices)
	first := s.Geometry.Slices[0]
	m := NewVolumeMask(s.Columns, s.Rows, slices, [3]float64{first.ColSpacing, first.RowSpacing, 1})
	if len(s.Geometry.Positions) == slices {
		copy(m.Z, s.Geometry.Positions)
	}
	for z := 0; z < slices; z++ {
		mask, ok := s.MaskAt(z)
		if !ok {
			continue
		}
		mask.ForEachPixel(func(x, y int) {
			if x < s.Columns && y < s.Rows {
				m.Voxels[m.index(x, y, z)] = true
			}
		})
	}
	return m, m.validate()
}

// SetVolumeMask replaces the segmentation's slices with the mask's voxels.
func (s *Segmentation3D) SetVolumeMask(m *VolumeMask) error {
	if err := m.validate(); err != nil {
		return err
	}
	if s == nil || m.Columns != s.Columns || m.Rows != s.Rows {
		return ErrInvalidVolumeMask
	}
	for z := 0; z < m.Slices; z++ {
		mask := NewRasterMask(m.Columns, m.Rows)
		for y := 0; y < m.Rows; y++ {
			start := -1
			for x := 0; x <= m.Columns; x++ {
				on := x < m.Columns && m.Voxels[m.index(x, y, z)]
				if on && start < 0 {
					start = x
				} else if !on && start >= 0 {
					mask.SetRun(y, start, x)
					start = -1
				}
			}
		}
		s.SetMask(z, mask)
	}
	return nil
}

// OverlapMetrics compares two segmentations on the same grid (#1076).
// Surface distances run between the boundary voxels of each mask (voxels
// with a 6-neighbour outside the mask or on the volume border), in mm.
type OverlapMetrics struct {
	// Dice is 2|A∩B|/(|A|+|B|); Jaccard is |A∩B|/|A∪B|.
	Dice, Jaccard float64
	// VolumeA and VolumeB are the masks' volumes in mm³ (voxel counts times
	// the local voxel size).
	VolumeA, VolumeB float64
	// HausdorffMM is the largest surface distance either way; HD95MM the
	// larger of the two directed 95th percentiles; MeanSurfaceDistanceMM the
	// mean over both surfaces of the distance to the other.
	HausdorffMM, HD95MM, MeanSurfaceDistanceMM float64
}

// ErrEmptyMask reports a comparison with an empty mask, for which surface
// distances are undefined.
var ErrEmptyMask = errors.New("roi: empty mask")

// CompareMasks computes overlap and surface-distance metrics between a and
// b, which must share dimensions and coordinates. Cancellation returns zero
// metrics and the context error.
func CompareMasks(ctx context.Context, a, b *VolumeMask) (OverlapMetrics, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return OverlapMetrics{}, err
	}
	if err := a.validateContext(ctx); err != nil {
		return OverlapMetrics{}, err
	}
	if err := b.validateContext(ctx); err != nil {
		return OverlapMetrics{}, err
	}
	if a.Columns != b.Columns || a.Rows != b.Rows || a.Slices != b.Slices ||
		!equalAxes(a.X, b.X) || !equalAxes(a.Y, b.Y) || !equalAxes(a.Z, b.Z) {
		return OverlapMetrics{}, fmt.Errorf("%w: masks on different grids", ErrInvalidVolumeMask)
	}
	var m OverlapMetrics
	both, either, countA, countB := 0, 0, 0, 0
	for i := range a.Voxels {
		if i%4096 == 0 && ctx.Err() != nil {
			return OverlapMetrics{}, ctx.Err()
		}
		va, vb := a.Voxels[i], b.Voxels[i]
		if va {
			countA++
			m.VolumeA += a.voxelVolume(i)
		}
		if vb {
			countB++
			m.VolumeB += a.voxelVolume(i)
		}
		if va && vb {
			both++
		}
		if va || vb {
			either++
		}
	}
	if err := ctx.Err(); err != nil {
		return OverlapMetrics{}, err
	}
	if countA == 0 || countB == 0 {
		return m, ErrEmptyMask
	}
	m.Dice = 2 * float64(both) / float64(countA+countB)
	m.Jaccard = float64(both) / float64(either)
	surfaceA, err := a.surface(ctx)
	if err != nil {
		return OverlapMetrics{}, err
	}
	surfaceB, err := b.surface(ctx)
	if err != nil {
		return OverlapMetrics{}, err
	}
	toA, err := surfaceA.DistanceTransform(ctx, true)
	if err != nil {
		return OverlapMetrics{}, err
	}
	toB, err := surfaceB.DistanceTransform(ctx, true)
	if err != nil {
		return OverlapMetrics{}, err
	}
	var ab, ba []float64
	for i := range surfaceA.Voxels {
		if i%4096 == 0 && ctx.Err() != nil {
			return OverlapMetrics{}, ctx.Err()
		}
		if surfaceA.Voxels[i] {
			ab = append(ab, toB[i])
		}
		if surfaceB.Voxels[i] {
			ba = append(ba, toA[i])
		}
	}
	sum := 0.0
	for _, direction := range [][]float64{ab, ba} {
		for i, d := range direction {
			if i%4096 == 0 && ctx.Err() != nil {
				return OverlapMetrics{}, ctx.Err()
			}
			sum += d
			m.HausdorffMM = math.Max(m.HausdorffMM, d)
		}
	}
	m.MeanSurfaceDistanceMM = sum / float64(len(ab)+len(ba))
	ab95 := percentileInPlace(ab, 0.95)
	if err := ctx.Err(); err != nil {
		return OverlapMetrics{}, err
	}
	m.HD95MM = math.Max(ab95, percentileInPlace(ba, 0.95))
	if err := ctx.Err(); err != nil {
		return OverlapMetrics{}, err
	}
	return m, nil
}

func equalAxes(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Abs(a[i]-b[i]) > 1e-9*math.Max(1, math.Abs(a[i])) {
			return false
		}
	}
	return true
}

// voxelVolume is the volume (mm³) of voxel i: the product of the half
// distances to its neighbours on each axis (the spacing on a regular grid).
func (m *VolumeMask) voxelVolume(i int) float64 {
	x, y, z := i%m.Columns, i/m.Columns%m.Rows, i/(m.Columns*m.Rows)
	return axisExtent(m.X, x) * axisExtent(m.Y, y) * axisExtent(m.Z, z)
}

func axisExtent(c []float64, i int) float64 {
	switch {
	case len(c) == 1:
		return 1
	case i == 0:
		return c[1] - c[0]
	case i == len(c)-1:
		return c[i] - c[i-1]
	}
	return (c[i+1] - c[i-1]) / 2
}

// surface returns the mask voxels with a 6-neighbour outside the mask or on
// the volume border.
func (m *VolumeMask) surface(ctx context.Context) (*VolumeMask, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := m.Clone()
	nx, ny, nz := m.Columns, m.Rows, m.Slices
	for z := 0; z < nz; z++ {
		for y := 0; y < ny; y++ {
			for x := 0; x < nx; x++ {
				i := m.index(x, y, z)
				if i%4096 == 0 && ctx.Err() != nil {
					return nil, ctx.Err()
				}
				if !m.Voxels[i] {
					continue
				}
				interior := x > 0 && x < nx-1 && y > 0 && y < ny-1 && z > 0 && z < nz-1 &&
					m.Voxels[i-1] && m.Voxels[i+1] && m.Voxels[i-nx] && m.Voxels[i+nx] &&
					m.Voxels[i-nx*ny] && m.Voxels[i+nx*ny]
				out.Voxels[i] = !interior
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// percentileInPlace returns the nearest-rank p-quantile and sorts values.
func percentileInPlace(values []float64, p float64) float64 {
	sort.Float64s(values)
	rank := int(math.Ceil(p*float64(len(values)))) - 1
	return values[max(0, min(rank, len(values)-1))]
}
