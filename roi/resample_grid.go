package roi

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/render"
)

// Patient-space grids for resampling label maps and segmentations (#1114).
//
// A Grid places a Columns x Rows x Slices voxel lattice in patient
// coordinates (mm). Resampling maps every target voxel centre into the
// source grid through its inverse affine, so the source and target may
// differ in size, spacing, orientation (oblique), slice direction (gantry
// tilt shear) and slice regularity, as long as both share one Frame of
// Reference. The caller checks the Frame of Reference.

var (
	// ErrInvalidGrid reports a grid whose size, spacing, directions or slice
	// offsets cannot describe a voxel lattice.
	ErrInvalidGrid = errors.New("roi: invalid resampling grid")
	// ErrInvalidResample reports an unusable resampling source or mode.
	ErrInvalidResample = errors.New("roi: invalid resampling request")
	// ErrResampleTooLarge reports a target grid beyond the voxel budget.
	ErrResampleTooLarge = errors.New("roi: resampling target exceeds the voxel budget")
)

// DefaultMaxResampleVoxels bounds the target voxels of one resampling call
// (256 MiB as a dense 16-bit label volume) unless the mode sets MaxVoxels.
const DefaultMaxResampleVoxels = 1 << 27

// Grid is a voxel lattice in patient coordinates. Directions follow
// render.SliceGeometry: RowDir is the direction of increasing column index
// (x) and ColDir the direction of increasing row index (y); ColSpacing is the
// distance between columns and RowSpacing the distance between rows.
//
// The centre of voxel (x, y, z) is
//
//	Origin + RowDir*(x*ColSpacing) + ColDir*(y*RowSpacing) + SliceDir*offset(z)
//
// where offset(z) is SliceOffsets[z] when SliceOffsets is set (irregular
// stacks) and z*SliceSpacing otherwise. SliceDir need not be perpendicular to
// the image plane (gantry-tilted stacks). For a single-slice grid,
// SliceSpacing is the slab thickness that the slice covers.
type Grid struct {
	Columns, Rows, Slices int
	Origin                render.Vec3
	RowDir, ColDir        render.Vec3
	SliceDir              render.Vec3
	ColSpacing            float64
	RowSpacing            float64
	SliceSpacing          float64
	// SliceOffsets, when not empty, holds Slices strictly ascending offsets
	// (mm) of the slice centres along SliceDir.
	SliceOffsets []float64
}

// IndexBox is a half-open voxel index box [Min, Max) of a grid, x, y, z.
type IndexBox struct {
	Min, Max [3]int
}

// Empty reports whether the box holds no voxel.
func (b IndexBox) Empty() bool {
	return b.Max[0] <= b.Min[0] || b.Max[1] <= b.Min[1] || b.Max[2] <= b.Min[2]
}

func (b IndexBox) contains(x, y, z int) bool {
	return x >= b.Min[0] && x < b.Max[0] && y >= b.Min[1] && y < b.Max[1] && z >= b.Min[2] && z < b.Max[2]
}

// GridFromSlice returns the single-slice grid of one image plane covering a
// slab of the given thickness (mm) centred on the plane.
func GridFromSlice(slice render.SliceGeometry, thickness float64) Grid {
	rowDir, colDir := slice.RowDir.Normalize(), slice.ColDir.Normalize()
	normal := slice.Normal.Normalize()
	if normal.Length() == 0 {
		normal = rowDir.Cross(colDir).Normalize()
	}
	return Grid{
		Columns: slice.Columns, Rows: slice.Rows, Slices: 1,
		Origin: slice.Origin, RowDir: rowDir, ColDir: colDir, SliceDir: normal,
		ColSpacing: slice.ColSpacing, RowSpacing: slice.RowSpacing, SliceSpacing: thickness,
	}
}

// GridFromSlices returns the grid of a stack of two or more parallel image
// planes of equal size and spacing, in stack order. The slice direction runs
// from the first to the last origin, so gantry-tilted stacks keep their
// shear; unequal gaps give SliceOffsets. The origins must be collinear and
// strictly ordered. Use GridFromSlice for a single plane.
func GridFromSlices(slices []render.SliceGeometry) (Grid, error) {
	if len(slices) < 2 {
		return Grid{}, fmt.Errorf("%w: %d slices; use GridFromSlice for one plane", ErrInvalidGrid, len(slices))
	}
	tolerance := render.DefaultGeometryTolerances()
	first := slices[0]
	grid := GridFromSlice(first, 0)
	grid.Slices = len(slices)
	for i, slice := range slices {
		if slice.Rows != first.Rows || slice.Columns != first.Columns ||
			!closeSpacing(slice.RowSpacing, first.RowSpacing) || !closeSpacing(slice.ColSpacing, first.ColSpacing) {
			return Grid{}, fmt.Errorf("%w: slice %d size or spacing differs from slice 0", ErrInvalidGrid, i)
		}
		if slice.RowDir.Normalize().Dot(grid.RowDir) < tolerance.OrientationCos ||
			slice.ColDir.Normalize().Dot(grid.ColDir) < tolerance.OrientationCos {
			return Grid{}, fmt.Errorf("%w: slice %d orientation differs from slice 0", ErrInvalidGrid, i)
		}
	}
	span := slices[len(slices)-1].Origin.Sub(first.Origin)
	direction := span.Normalize()
	normal := grid.RowDir.Cross(grid.ColDir).Normalize()
	if span.Length() == 0 || math.Abs(direction.Dot(normal)) < 1e-3 {
		return Grid{}, fmt.Errorf("%w: slice origins do not advance out of the image plane", ErrInvalidGrid)
	}
	grid.SliceDir = direction
	offsets := make([]float64, len(slices))
	regular := true
	meanGap := span.Length() / float64(len(slices)-1)
	// Origins off the stack line by up to the DICOM grid tolerance, a
	// twentieth of a pixel, are rounding (#1115).
	offLine := render.DICOMGridCoordinateTolerance * math.Min(first.RowSpacing, first.ColSpacing)
	for i, slice := range slices {
		delta := slice.Origin.Sub(first.Origin)
		offsets[i] = delta.Dot(direction)
		if off := delta.Sub(direction.Scale(offsets[i])).Length(); off > offLine {
			return Grid{}, fmt.Errorf("%w: slice %d origin is %.3g mm off the stack line", ErrInvalidGrid, i, off)
		}
		if i > 0 {
			gap := offsets[i] - offsets[i-1]
			if !(gap > 1e-6) {
				return Grid{}, fmt.Errorf("%w: slice %d is not after slice %d along the stack", ErrInvalidGrid, i, i-1)
			}
			if math.Abs(gap-meanGap) > math.Max(0.01, 1e-3*meanGap) {
				regular = false
			}
		}
	}
	grid.SliceSpacing = meanGap
	if !regular {
		grid.SliceOffsets = offsets
	}
	return grid, nil
}

// GridFromVolumeGeometry returns the grid of a volume geometry's slices; a
// single slice uses MeanSpacing as its thickness.
func GridFromVolumeGeometry(geometry render.VolumeGeometry) (Grid, error) {
	switch len(geometry.Slices) {
	case 0:
		return Grid{}, fmt.Errorf("%w: volume geometry has no slices", ErrInvalidGrid)
	case 1:
		grid := GridFromSlice(geometry.Slices[0], geometry.MeanSpacing)
		return grid, grid.Validate()
	}
	return GridFromSlices(geometry.Slices)
}

func closeSpacing(a, b float64) bool {
	return math.Abs(a-b) <= math.Max(1e-4, 1e-4*math.Abs(b))
}

// Validate reports whether the grid describes a non-degenerate lattice.
func (g Grid) Validate() error {
	if g.Columns <= 0 || g.Rows <= 0 || g.Slices <= 0 {
		return fmt.Errorf("%w: size %dx%dx%d", ErrInvalidGrid, g.Columns, g.Rows, g.Slices)
	}
	if int64(g.Columns)*int64(g.Rows) > math.MaxInt32 || int64(g.Columns)*int64(g.Rows)*int64(g.Slices) > math.MaxInt64/4 {
		return fmt.Errorf("%w: size %dx%dx%d overflows", ErrInvalidGrid, g.Columns, g.Rows, g.Slices)
	}
	for _, value := range []float64{g.ColSpacing, g.RowSpacing} {
		if !(value > 0) || math.IsInf(value, 0) {
			return fmt.Errorf("%w: in-plane spacing must be positive and finite", ErrInvalidGrid)
		}
	}
	if len(g.SliceOffsets) == 0 {
		if !(g.SliceSpacing > 0) || math.IsInf(g.SliceSpacing, 0) {
			return fmt.Errorf("%w: slice spacing (or single-slice thickness) must be positive and finite", ErrInvalidGrid)
		}
	} else {
		if len(g.SliceOffsets) != g.Slices {
			return fmt.Errorf("%w: %d slice offsets for %d slices", ErrInvalidGrid, len(g.SliceOffsets), g.Slices)
		}
		for i, value := range g.SliceOffsets {
			if math.IsNaN(value) || math.IsInf(value, 0) || i > 0 && !(value > g.SliceOffsets[i-1]) {
				return fmt.Errorf("%w: slice offsets must be finite and strictly ascending", ErrInvalidGrid)
			}
		}
		if g.Slices == 1 && !(g.SliceSpacing > 0) {
			return fmt.Errorf("%w: a single slice needs a positive thickness", ErrInvalidGrid)
		}
	}
	for _, v := range []render.Vec3{g.Origin, g.RowDir, g.ColDir, g.SliceDir} {
		if math.IsNaN(v.X+v.Y+v.Z) || math.IsInf(v.X+v.Y+v.Z, 0) {
			return fmt.Errorf("%w: non-finite origin or direction", ErrInvalidGrid)
		}
	}
	a, b, c := g.RowDir.Normalize(), g.ColDir.Normalize(), g.SliceDir.Normalize()
	if a.Length() == 0 || b.Length() == 0 || c.Length() == 0 || math.Abs(a.Cross(b).Dot(c)) < 1e-6 {
		return fmt.Errorf("%w: directions are zero or coplanar", ErrInvalidGrid)
	}
	return nil
}

// VoxelCount returns Columns*Rows*Slices.
func (g Grid) VoxelCount() int64 {
	return int64(g.Columns) * int64(g.Rows) * int64(g.Slices)
}

// SliceOffset returns the offset (mm) of slice z along SliceDir.
func (g Grid) SliceOffset(z int) float64 {
	if len(g.SliceOffsets) > 0 {
		return g.SliceOffsets[z]
	}
	return float64(z) * g.SliceSpacing
}

// VoxelCenter returns the patient coordinates of a voxel centre.
func (g Grid) VoxelCenter(x, y, z int) render.Vec3 {
	return g.Origin.
		Add(g.RowDir.Normalize().Scale(float64(x) * g.ColSpacing)).
		Add(g.ColDir.Normalize().Scale(float64(y) * g.RowSpacing)).
		Add(g.SliceDir.Normalize().Scale(g.SliceOffset(z)))
}

// SliceGeometry returns the image plane of slice z.
func (g Grid) SliceGeometry(z int) render.SliceGeometry {
	rowDir, colDir := g.RowDir.Normalize(), g.ColDir.Normalize()
	return render.SliceGeometry{
		Origin: g.VoxelCenter(0, 0, z), RowDir: rowDir, ColDir: colDir,
		Normal:     rowDir.Cross(colDir).Normalize(),
		RowSpacing: g.RowSpacing, ColSpacing: g.ColSpacing, Rows: g.Rows, Columns: g.Columns,
	}
}

// ContinuousIndex maps a patient point to continuous voxel indices (voxel
// centres at integers; irregular slices interpolate piecewise linearly
// between their offsets). inside reports whether the point lies within the
// grid's half-voxel extent, [-0.5, n-0.5) on every axis, which is where
// resampling reads the grid.
func (g Grid) ContinuousIndex(p render.Vec3) (x, y, z float64, inside bool) {
	m, err := newGridMap(g)
	if err != nil {
		return 0, 0, 0, false
	}
	u, v, s := m.coords(p)
	x, y, z = u/g.ColSpacing, v/g.RowSpacing, m.axes[2].continuous(s)
	return x, y, z, m.inside(u, v, s)
}

// crop returns the sub-grid of the box, which must lie inside the grid.
func (g Grid) crop(box IndexBox) Grid {
	out := g
	out.Origin = g.VoxelCenter(box.Min[0], box.Min[1], box.Min[2])
	out.Columns, out.Rows, out.Slices = box.Max[0]-box.Min[0], box.Max[1]-box.Min[1], box.Max[2]-box.Min[2]
	if len(g.SliceOffsets) > 0 {
		base := g.SliceOffsets[box.Min[2]]
		out.SliceOffsets = make([]float64, out.Slices)
		for i := range out.SliceOffsets {
			out.SliceOffsets[i] = g.SliceOffsets[box.Min[2]+i] - base
		}
	}
	return out
}

// gridAxis is one axis of a grid in mm along its direction.
type gridAxis struct {
	n       int
	spacing float64
	offsets []float64 // nil for a regular axis
}

// center returns the coordinate (mm) of voxel centre i.
func (a gridAxis) center(i int) float64 {
	if a.offsets != nil {
		return a.offsets[i]
	}
	return float64(i) * a.spacing
}

// lowGap and highGap are the gaps (mm) below the first and above the last
// voxel centre that bound the axis' half-voxel extent.
func (a gridAxis) lowGap() float64 {
	if a.offsets != nil && a.n > 1 {
		return a.offsets[1] - a.offsets[0]
	}
	return a.spacing
}

func (a gridAxis) highGap() float64 {
	if a.offsets != nil && a.n > 1 {
		return a.offsets[a.n-1] - a.offsets[a.n-2]
	}
	return a.spacing
}

// edges returns the voxel box [lo, hi) (mm) of voxel i: halfway to its
// neighbours, and half a gap beyond the first and last centres.
func (a gridAxis) edges(i int) (float64, float64) {
	if a.offsets == nil {
		return (float64(i) - 0.5) * a.spacing, (float64(i) + 0.5) * a.spacing
	}
	lo := a.offsets[i] - a.lowGap()/2
	if i > 0 {
		lo = (a.offsets[i-1] + a.offsets[i]) / 2
	}
	hi := a.offsets[i] + a.highGap()/2
	if i+1 < a.n {
		hi = (a.offsets[i] + a.offsets[i+1]) / 2
	}
	return lo, hi
}

// extent returns the half-voxel extent [lo, hi) of voxels [i0, i1).
func (a gridAxis) extent(i0, i1 int) (float64, float64) {
	lo, _ := a.edges(i0)
	_, hi := a.edges(i1 - 1)
	return lo, hi
}

// continuous maps a coordinate (mm) to a continuous index: voxel centres at
// integers, linear between irregular centres and beyond the end gaps.
func (a gridAxis) continuous(c float64) float64 {
	if a.offsets == nil {
		return c / a.spacing
	}
	first, last := a.offsets[0], a.offsets[a.n-1]
	switch {
	case a.n == 1:
		return (c - first) / a.spacing
	case c <= first:
		return (c - first) / a.lowGap()
	case c >= last:
		return float64(a.n-1) + (c-last)/a.highGap()
	}
	lo, hi := 0, a.n-1
	for hi-lo > 1 {
		mid := (lo + hi) / 2
		if a.offsets[mid] <= c {
			lo = mid
		} else {
			hi = mid
		}
	}
	return float64(lo) + (c-a.offsets[lo])/(a.offsets[hi]-a.offsets[lo])
}

// nearest returns the index of the voxel whose box holds c; callers check
// the extent first.
func (a gridAxis) nearest(c float64) int {
	return min(max(render.NearestIndex(a.continuous(c)), 0), a.n-1)
}

// gridMap maps patient points to a grid's axis coordinates (mm).
type gridMap struct {
	grid   Grid
	origin render.Vec3
	inv    [3][3]float64
	axes   [3]gridAxis
	lo, hi [3]float64 // half-voxel extent (mm) per axis
}

func newGridMap(g Grid) (*gridMap, error) {
	if err := g.Validate(); err != nil {
		return nil, err
	}
	a, b, c := g.RowDir.Normalize(), g.ColDir.Normalize(), g.SliceDir.Normalize()
	// Columns of M are the unit axes; inv = M^-1 by the adjugate.
	m := [3][3]float64{{a.X, b.X, c.X}, {a.Y, b.Y, c.Y}, {a.Z, b.Z, c.Z}}
	det := m[0][0]*(m[1][1]*m[2][2]-m[1][2]*m[2][1]) -
		m[0][1]*(m[1][0]*m[2][2]-m[1][2]*m[2][0]) +
		m[0][2]*(m[1][0]*m[2][1]-m[1][1]*m[2][0])
	if math.Abs(det) < 1e-6 {
		return nil, fmt.Errorf("%w: degenerate directions", ErrInvalidGrid)
	}
	var inv [3][3]float64
	inv[0][0] = (m[1][1]*m[2][2] - m[1][2]*m[2][1]) / det
	inv[0][1] = (m[0][2]*m[2][1] - m[0][1]*m[2][2]) / det
	inv[0][2] = (m[0][1]*m[1][2] - m[0][2]*m[1][1]) / det
	inv[1][0] = (m[1][2]*m[2][0] - m[1][0]*m[2][2]) / det
	inv[1][1] = (m[0][0]*m[2][2] - m[0][2]*m[2][0]) / det
	inv[1][2] = (m[0][2]*m[1][0] - m[0][0]*m[1][2]) / det
	inv[2][0] = (m[1][0]*m[2][1] - m[1][1]*m[2][0]) / det
	inv[2][1] = (m[0][1]*m[2][0] - m[0][0]*m[2][1]) / det
	inv[2][2] = (m[0][0]*m[1][1] - m[0][1]*m[1][0]) / det
	out := &gridMap{grid: g, origin: g.Origin, inv: inv}
	out.axes[0] = gridAxis{n: g.Columns, spacing: g.ColSpacing}
	out.axes[1] = gridAxis{n: g.Rows, spacing: g.RowSpacing}
	out.axes[2] = gridAxis{n: g.Slices, spacing: g.SliceSpacing}
	if len(g.SliceOffsets) > 0 {
		out.axes[2].offsets = g.SliceOffsets
		if g.Slices > 1 {
			out.axes[2].spacing = (g.SliceOffsets[g.Slices-1] - g.SliceOffsets[0]) / float64(g.Slices-1)
		}
	}
	for i := range out.axes {
		out.lo[i], out.hi[i] = out.axes[i].extent(0, out.axes[i].n)
	}
	return out, nil
}

// vector maps a patient-space displacement to axis coordinates (mm).
func (m *gridMap) vector(d render.Vec3) (float64, float64, float64) {
	return m.inv[0][0]*d.X + m.inv[0][1]*d.Y + m.inv[0][2]*d.Z,
		m.inv[1][0]*d.X + m.inv[1][1]*d.Y + m.inv[1][2]*d.Z,
		m.inv[2][0]*d.X + m.inv[2][1]*d.Y + m.inv[2][2]*d.Z
}

func (m *gridMap) coords(p render.Vec3) (float64, float64, float64) {
	return m.vector(p.Sub(m.origin))
}

func (m *gridMap) inside(u, v, s float64) bool {
	return u >= m.lo[0] && u < m.hi[0] && v >= m.lo[1] && v < m.hi[1] && s >= m.lo[2] && s < m.hi[2]
}

// boxExtent returns the half-voxel extent (mm) of an index box.
func (m *gridMap) boxExtent(box IndexBox) (lo, hi [3]float64) {
	for i := range m.axes {
		lo[i], hi[i] = m.axes[i].extent(box.Min[i], box.Max[i])
	}
	return lo, hi
}

// rowLine is one target row expressed in source axis coordinates: the
// coordinates of target column x are base + x*step.
type rowLine struct {
	base, step [3]float64
}

func (r rowLine) at(x int) (float64, float64, float64) {
	fx := float64(x)
	return r.base[0] + fx*r.step[0], r.base[1] + fx*r.step[1], r.base[2] + fx*r.step[2]
}

// columnRange returns the target columns [x0, x1) of the row whose
// coordinates may fall in [lo, hi) on every axis, widened by one column
// against rounding; callers test each column exactly.
func (r rowLine) columnRange(columns int, lo, hi [3]float64) (int, int) {
	start, end := 0.0, float64(columns)
	for i := 0; i < 3; i++ {
		b, d := r.base[i], r.step[i]
		if math.Abs(d) < 1e-12 {
			if b < lo[i] || b >= hi[i] {
				return 0, 0
			}
			continue
		}
		t0, t1 := (lo[i]-b)/d, (hi[i]-b)/d
		if t0 > t1 {
			t0, t1 = t1, t0
		}
		start, end = math.Max(start, t0), math.Min(end, t1)
	}
	if !(end >= start) {
		return 0, 0
	}
	x0 := max(0, int(math.Floor(start))-1)
	x1 := min(columns, int(math.Ceil(end))+1)
	if x1 < x0 {
		return 0, 0
	}
	return x0, x1
}

// targetRows maps the rows of a target grid into a source grid.
type targetRows struct {
	target Grid
	src    *gridMap
	rowDX  [3]float64 // source coordinates per target column
	rowDY  [3]float64 // per target row
	slices [][3]float64
}

func newTargetRows(src *gridMap, target Grid) *targetRows {
	t := &targetRows{target: target, src: src, slices: make([][3]float64, target.Slices)}
	t.rowDX[0], t.rowDX[1], t.rowDX[2] = src.vector(target.RowDir.Normalize().Scale(target.ColSpacing))
	t.rowDY[0], t.rowDY[1], t.rowDY[2] = src.vector(target.ColDir.Normalize().Scale(target.RowSpacing))
	for z := range t.slices {
		t.slices[z][0], t.slices[z][1], t.slices[z][2] = src.coords(target.VoxelCenter(0, 0, z))
	}
	return t
}

func (t *targetRows) line(y, z int) rowLine {
	var out rowLine
	fy := float64(y)
	for i := 0; i < 3; i++ {
		out.base[i] = t.slices[z][i] + fy*t.rowDY[i]
	}
	out.step = t.rowDX
	return out
}

// ResampleCoverage reports which target voxels lie inside the source grid.
// Uncovered voxels carry no information (label 0 or fraction 0) and must not
// be read as background.
type ResampleCoverage struct {
	Columns, Rows, Slices int
	// Masks holds, per target slice, its covered pixels; nil when the slice
	// has none.
	Masks []*RasterMask
	// SlicePixels is the number of covered pixels of each target slice.
	SlicePixels   []int
	CoveredVoxels int64
}

// TotalVoxels returns the number of target voxels.
func (c ResampleCoverage) TotalVoxels() int64 {
	return int64(c.Columns) * int64(c.Rows) * int64(c.Slices)
}

// Full reports whether every target voxel is covered.
func (c ResampleCoverage) Full() bool {
	return c.TotalVoxels() > 0 && c.CoveredVoxels == c.TotalVoxels()
}

// Partial reports whether some, but not all, target voxels are covered.
func (c ResampleCoverage) Partial() bool {
	return c.CoveredVoxels > 0 && c.CoveredVoxels < c.TotalVoxels()
}

// Fraction returns the covered share of the target voxels.
func (c ResampleCoverage) Fraction() float64 {
	if c.TotalVoxels() == 0 {
		return 0
	}
	return float64(c.CoveredVoxels) / float64(c.TotalVoxels())
}

// CoveredSlices returns the target slices with at least one covered pixel.
func (c ResampleCoverage) CoveredSlices() []int {
	var out []int
	for z, n := range c.SlicePixels {
		if n > 0 {
			out = append(out, z)
		}
	}
	return out
}

// SliceFraction returns the covered share of the pixels of slice z.
func (c ResampleCoverage) SliceFraction(z int) float64 {
	if z < 0 || z >= len(c.SlicePixels) || c.Columns*c.Rows == 0 {
		return 0
	}
	return float64(c.SlicePixels[z]) / float64(c.Columns*c.Rows)
}

// Covered reports whether target voxel (x, y, z) is covered.
func (c ResampleCoverage) Covered(x, y, z int) bool {
	if z < 0 || z >= len(c.Masks) {
		return false
	}
	return c.Masks[z].Get(x, y)
}

// GridCoverage reports which voxels of target lie inside source's
// half-voxel extent.
func GridCoverage(ctx context.Context, source, target Grid) (ResampleCoverage, error) {
	src, err := newGridMap(source)
	if err != nil {
		return ResampleCoverage{}, err
	}
	if err := target.Validate(); err != nil {
		return ResampleCoverage{}, err
	}
	rows := newTargetRows(src, target)
	coverage := newCoverage(target)
	err = parallelLines(ctx, target.Rows*target.Slices, func() func(int) {
		return func(line int) {
			y, z := line%target.Rows, line/target.Rows
			x0, x1 := coveredRun(src, rows.line(y, z), target.Columns)
			coverage.setRow(y, z, x0, x1)
		}
	})
	if err != nil {
		return ResampleCoverage{}, err
	}
	coverage.finish()
	return coverage.ResampleCoverage, nil
}

// coveredRun returns the covered columns [x0, x1) of a target row. The
// covered set of a straight row inside a convex extent is one run.
func coveredRun(src *gridMap, line rowLine, columns int) (int, int) {
	x0, x1 := line.columnRange(columns, src.lo, src.hi)
	// columnRange is widened against rounding: trim both ends exactly. The
	// columns in between are inside because the extent is convex.
	for x0 < x1 && !src.inside(line.at(x0)) {
		x0++
	}
	for x1 > x0 && !src.inside(line.at(x1-1)) {
		x1--
	}
	if x1 <= x0 {
		return 0, 0
	}
	return x0, x1
}

// PointMapper maps a target patient point into the source's patient space,
// as a Spatial Registration maps between Frames of Reference; an error
// leaves the target voxel uncovered.
type PointMapper func(render.Vec3) (render.Vec3, error)

// mapRow maps the voxel centres of target row (y, z) into source axis
// coordinates; inside marks the voxels mapped inside the source's half-voxel
// extent.
func mapRow(src *gridMap, target Grid, mapper PointMapper, y, z int, coords [][3]float64, inside []bool) {
	for x := 0; x < target.Columns; x++ {
		inside[x] = false
		q, err := mapper(target.VoxelCenter(x, y, z))
		if err != nil {
			continue
		}
		u, v, s := src.coords(q)
		coords[x] = [3]float64{u, v, s}
		inside[x] = src.inside(u, v, s)
	}
}

// coverageBuilder fills ResampleCoverage from concurrent rows.
type coverageBuilder struct {
	ResampleCoverage
	rowCounts []int32
}

func newCoverage(target Grid) *coverageBuilder {
	c := &coverageBuilder{rowCounts: make([]int32, target.Rows*target.Slices)}
	c.Columns, c.Rows, c.Slices = target.Columns, target.Rows, target.Slices
	c.Masks = make([]*RasterMask, target.Slices)
	for z := range c.Masks {
		c.Masks[z] = NewRasterMask(target.Columns, target.Rows)
	}
	c.SlicePixels = make([]int, target.Slices)
	return c
}

// setRow records the covered run of one row; rows are distinct per call.
func (c *coverageBuilder) setRow(y, z, x0, x1 int) {
	if x1 > x0 {
		c.Masks[z].SetRun(y, x0, x1)
		c.rowCounts[z*c.Rows+y] = int32(x1 - x0)
	}
}

// setMask records the covered pixels of one mapped row, which need not be
// one run.
func (c *coverageBuilder) setMask(y, z int, inside []bool) {
	n := 0
	for x := 0; x < len(inside); {
		if !inside[x] {
			x++
			continue
		}
		end := x + 1
		for end < len(inside) && inside[end] {
			end++
		}
		c.Masks[z].SetRun(y, x, end)
		n += end - x
		x = end
	}
	c.rowCounts[z*c.Rows+y] = int32(n)
}

func (c *coverageBuilder) finish() {
	for z := 0; z < c.Slices; z++ {
		n := 0
		for y := 0; y < c.Rows; y++ {
			n += int(c.rowCounts[z*c.Rows+y])
		}
		c.SlicePixels[z] = n
		c.CoveredVoxels += int64(n)
		if n == 0 {
			c.Masks[z] = nil
		}
	}
	c.rowCounts = nil
}
