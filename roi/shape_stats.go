package roi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/ThalesMMS/dicom-go/internal/linalg"
	"github.com/ThalesMMS/dicom-go/render"
)

// Per-label shape statistics (#1097): centroid, principal moments and axes,
// elongation, flatness, equivalent sphere and ellipsoid, Feret diameter,
// Crofton perimeter (2D) or surface area (3D), sphericity, border contact
// and the oriented bounding box, for a VolumeMask, a Segmentation3D, a 2D
// RasterMask or every label of a LabelVolume (the resampling label map).
//
// The definitions follow ITK's ShapeLabelObject as computed by
// ShapeLabelMapFilter (Apache-2.0; re-implemented here, not copied), so
// results can be compared with SimpleITK's LabelShapeStatisticsImageFilter:
//
//   - Voxels are points at their centres for the centroid, the second
//     moments and the Feret diameter. The Feret diameter is the largest
//     distance between two voxel centres (ITK searches the boundary voxel
//     centres; the maximum is the same), not between voxel corners, so a
//     single voxel has a Feret diameter of 0 and an n-voxel line of spacing
//     s has (n-1)·s.
//   - Principal moments are the eigenvalues, ascending, of the covariance of
//     the voxel centres (mm²; no 1/12 voxel self-inertia term). Elongation is
//     √(λmax/λmid) and flatness √(λmid/λmin) in 3D (both √(λ1/λ0) in 2D),
//     0 when the denominator is 0.
//   - The oriented bounding box is aligned with the principal axes and
//     encloses the full voxel cells (corners), so one voxel gives a box the
//     size of the voxel.
//   - The perimeter is the Crofton estimate from intercept counts along 4
//     (2D) or 13 (3D) lattice directions with ITK's direction weights (see
//     shape_crofton.go for its accuracy). It is biased high for objects of
//     a few voxels, so sphericity can exceed 1 there.
//
// Differences from ITK: moments and centroid weight each voxel by its cell
// volume, which only matters for irregular slice positions (ITK images are
// always regular); principal-axis signs follow a documented convention
// (largest component positive, right-handed) rather than the eigen solver's
// arbitrary output, so the OBB origin can be another corner of the same box
// (the vertex set is identical); and a moment below 1e-12 of the largest is
// snapped to 0, which keeps flatness defined for flat objects in rotated
// frames where ITK reports round-off.

// ErrInvalidShapeInput reports geometry or options that shape statistics
// cannot use.
var ErrInvalidShapeInput = errors.New("roi: invalid shape statistics input")

// ShapeOptions place the voxel grid in patient space.
type ShapeOptions struct {
	// Origin is the patient position (mm) of the grid coordinate (0, 0, 0).
	// Segmentation3D and LabelVolume statistics take it from their geometry.
	Origin render.Vec3
	// Direction holds the patient unit vectors of the grid's x (column), y
	// (row) and z (slice) axes; all zero means identity. They must be
	// orthonormal. Segmentation3D and LabelVolume statistics take them from
	// their geometry.
	Direction [3]render.Vec3
	// Planar computes 2D statistics (area in mm², perimeter in mm) for a
	// VolumeMask or LabelVolume with exactly one slice.
	Planar bool
}

// OrientedBoundingBox is the smallest box aligned with the principal axes
// that contains every voxel cell.
type OrientedBoundingBox struct {
	// Origin is the patient-space corner at the minimum of every axis.
	Origin render.Vec3
	// Axes are the box edges' unit directions (the principal axes).
	Axes []render.Vec3
	// Size is the edge length (mm) along each axis.
	Size []float64
}

// Vertices returns the 2^dimension box corners; bit k of the vertex index
// selects the far end of axis k.
func (b OrientedBoundingBox) Vertices() []render.Vec3 {
	n := len(b.Axes)
	if n == 0 || len(b.Size) != n {
		return nil
	}
	out := make([]render.Vec3, 1<<n)
	for v := range out {
		p := b.Origin
		for k := 0; k < n; k++ {
			if v&(1<<k) != 0 {
				p = p.Add(b.Axes[k].Scale(b.Size[k]))
			}
		}
		out[v] = p
	}
	return out
}

// ShapeStatistics describes one binary object. Lengths are mm, areas mm²
// and volumes mm³; in 2D PhysicalSize is an area and Perimeter a length.
// Points and directions are in patient space. An empty object has
// VoxelCount 0 and every other field zero.
type ShapeStatistics struct {
	// Label is the label value for LabelShapeStatistics, 0 otherwise.
	Label     uint16
	Dimension int // 2 or 3

	VoxelCount   int
	PhysicalSize float64 // mm³ in 3D, mm² in 2D
	// BoundingBoxMin and BoundingBoxMax are the inclusive (column, row,
	// slice) index bounds.
	BoundingBoxMin, BoundingBoxMax [3]int
	Centroid                       render.Vec3

	// PrincipalMoments are the covariance eigenvalues (mm²), ascending, and
	// PrincipalAxes the matching unit vectors, forming a right-handed basis
	// (in patient space in 3D, in the grid plane in 2D) with each vector's
	// largest component positive where that allows.
	PrincipalMoments []float64
	PrincipalAxes    []render.Vec3
	Elongation       float64
	Flatness         float64

	// EquivalentSphericalRadius is the radius of the sphere (circle in 2D)
	// of the same size; EquivalentSphericalPerimeter is its surface area
	// (circumference in 2D).
	EquivalentSphericalRadius    float64
	EquivalentSphericalPerimeter float64
	// EquivalentEllipsoidDiameter are the axis lengths, in principal-axis
	// order, of the ellipsoid with the same size and moment ratios (all 0
	// when a principal moment is 0).
	EquivalentEllipsoidDiameter []float64

	// FeretDiameter is the largest distance between two voxel centres and
	// FeretEndpoints those centres.
	FeretDiameter  float64
	FeretEndpoints [2]render.Vec3

	// Perimeter is the Crofton estimate of the surface area (3D) or the
	// perimeter (2D). Sphericity is EquivalentSphericalPerimeter/Perimeter
	// (ITK's "roundness"; Wadell's π^⅓(6V)^⅔/A in 3D).
	Perimeter  float64
	Sphericity float64

	// VoxelsOnBorder counts voxels on the first or last index of an axis;
	// PerimeterOnBorder is the area (length in 2D) of their cell faces lying
	// on the grid boundary, and PerimeterOnBorderRatio its share of
	// Perimeter.
	VoxelsOnBorder         int
	PerimeterOnBorder      float64
	PerimeterOnBorderRatio float64

	OrientedBoundingBox OrientedBoundingBox
}

// TouchesBorder reports whether the object has voxels on the grid boundary,
// so its shape may be truncated.
func (s ShapeStatistics) TouchesBorder() bool { return s.VoxelsOnBorder > 0 }

// ShapeStatistics computes the shape statistics of the set voxels, placing
// the per-axis coordinates in patient space with opts.
func (m *VolumeMask) ShapeStatistics(opts ShapeOptions) (ShapeStatistics, error) {
	if err := m.validate(); err != nil {
		return ShapeStatistics{}, err
	}
	g, err := newShapeGrid(opts.Planar, [3]int{m.Columns, m.Rows, m.Slices}, [3][]float64{m.X, m.Y, m.Z}, [3]float64{1, 1, 1}, opts)
	if err != nil {
		return ShapeStatistics{}, err
	}
	var runs []shapeRun
	nx := m.Columns
	for z := 0; z < m.Slices; z++ {
		for y := 0; y < m.Rows; y++ {
			row := m.Voxels[m.index(0, y, z):][:nx]
			runs = appendRowRuns(runs, y, z, nx, func(x int) bool { return row[x] })
		}
	}
	return g.statistics(runs), nil
}

// ShapeStatistics computes the shape statistics of a 2D mask with the given
// column and row spacing (mm); opts may place it in patient space (only
// Direction[0] and Direction[1] are used).
func (m *RasterMask) ShapeStatistics(columnSpacing, rowSpacing float64, opts ShapeOptions) (ShapeStatistics, error) {
	if m == nil || m.Columns <= 0 || m.Rows <= 0 || !(columnSpacing > 0) || !(rowSpacing > 0) ||
		math.IsInf(columnSpacing, 0) || math.IsInf(rowSpacing, 0) {
		return ShapeStatistics{}, fmt.Errorf("%w: mask size or spacing", ErrInvalidShapeInput)
	}
	g, err := newShapeGrid(true, [3]int{m.Columns, m.Rows, 1},
		[3][]float64{uniformAxis(m.Columns, columnSpacing), uniformAxis(m.Rows, rowSpacing), {0}},
		[3]float64{columnSpacing, rowSpacing, 1}, opts)
	if err != nil {
		return ShapeStatistics{}, err
	}
	var runs []shapeRun
	m.ForEachRun(func(y int, run MaskRun) {
		runs = appendClippedRun(runs, y, 0, run, m.Columns)
	})
	return g.statistics(runs), nil
}

// ShapeStatistics computes the shape statistics of the segmentation in the
// patient space of its geometry: column and row spacing of the first slice,
// the real slice positions along the normal (uniform MeanSpacing, or 1 mm,
// when positions are missing) and the row, column and normal directions.
// Gantry-tilt shear is ignored; resample tilted stacks first.
func (s *Segmentation3D) ShapeStatistics() (ShapeStatistics, error) {
	if s == nil || s.Columns <= 0 || s.Rows <= 0 {
		return ShapeStatistics{}, fmt.Errorf("%w: segmentation size", ErrInvalidShapeInput)
	}
	g, err := segmentationShapeGrid(s)
	if err != nil {
		return ShapeStatistics{}, err
	}
	var runs []shapeRun
	for _, z := range s.Slices() {
		if z >= g.size[2] {
			continue
		}
		s.masks[z].ForEachRun(func(y int, run MaskRun) {
			if y < s.Rows {
				runs = appendClippedRun(runs, y, z, run, s.Columns)
			}
		})
	}
	return g.statistics(runs), nil
}

// labelShapeGrid is the shape grid of a label volume's Grid: its slice
// offsets (or regular spacing) along SliceDir, placed in patient space by
// its origin and directions. SliceDir must be perpendicular to the image
// plane; resample gantry-tilted grids first.
func labelShapeGrid(v *LabelVolume, planar bool) (*shapeGrid, error) {
	if v == nil {
		return nil, fmt.Errorf("%w: nil label volume", ErrInvalidShapeInput)
	}
	grid := v.Grid
	if err := grid.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidShapeInput, err)
	}
	if int64(len(v.Labels)) != grid.VoxelCount() {
		return nil, fmt.Errorf("%w: %d labels for %d voxels", ErrInvalidShapeInput, len(v.Labels), grid.VoxelCount())
	}
	z := make([]float64, grid.Slices)
	for i := range z {
		z[i] = grid.SliceOffset(i)
	}
	thickness := grid.SliceSpacing
	if !(thickness > 0) {
		thickness = 1
	}
	opts := ShapeOptions{
		Origin:    grid.Origin,
		Direction: [3]render.Vec3{grid.RowDir.Normalize(), grid.ColDir.Normalize(), grid.SliceDir.Normalize()},
	}
	return newShapeGrid(planar, [3]int{grid.Columns, grid.Rows, grid.Slices},
		[3][]float64{uniformAxis(grid.Columns, grid.ColSpacing), uniformAxis(grid.Rows, grid.RowSpacing), z},
		[3]float64{grid.ColSpacing, grid.RowSpacing, thickness}, opts)
}

// labelRuns splits the volume into per-label run lists.
func labelRuns(ctx context.Context, v *LabelVolume) (map[uint16][]shapeRun, error) {
	out := map[uint16][]shapeRun{}
	nx, ny := v.Grid.Columns, v.Grid.Rows
	for z := 0; z < v.Grid.Slices; z++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for y := 0; y < ny; y++ {
			row := v.Labels[(z*ny+y)*nx:][:nx]
			for x := 0; x < nx; {
				label := row[x]
				end := x + 1
				for end < nx && row[end] == label {
					end++
				}
				if label != 0 {
					out[label] = append(out[label], shapeRun{y: y, z: z, x0: x, x1: end - 1})
				}
				x = end
			}
		}
	}
	return out, nil
}

// LabelShapeStatistics returns the shape statistics of every non-zero label
// of v, sorted by label, in the patient space of v.Grid. Planar computes 2D
// statistics for a single-slice volume; the other options are ignored.
func LabelShapeStatistics(ctx context.Context, v *LabelVolume, opts ShapeOptions) ([]ShapeStatistics, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	g, err := labelShapeGrid(v, opts.Planar)
	if err != nil {
		return nil, err
	}
	byLabel, err := labelRuns(ctx, v)
	if err != nil {
		return nil, err
	}
	labels := sortedLabels(byLabel)
	out := make([]ShapeStatistics, 0, len(labels))
	for _, label := range labels {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		stats := g.statistics(byLabel[label])
		stats.Label = label
		out = append(out, stats)
	}
	return out, nil
}

func sortedLabels[T any](m map[uint16]T) []uint16 {
	out := make([]uint16, 0, len(m))
	for label := range m {
		out = append(out, label)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// shapeRun is an inclusive span [x0, x1] of set voxels in row (y, z).
type shapeRun struct{ y, z, x0, x1 int }

func appendRowRuns(runs []shapeRun, y, z, n int, set func(x int) bool) []shapeRun {
	for x := 0; x < n; x++ {
		if !set(x) {
			continue
		}
		start := x
		for x+1 < n && set(x+1) {
			x++
		}
		runs = append(runs, shapeRun{y: y, z: z, x0: start, x1: x})
	}
	return runs
}

func appendClippedRun(runs []shapeRun, y, z int, run MaskRun, columns int) []shapeRun {
	x0, x1 := max(run.Start, 0), min(run.End, columns)-1
	if x1 < x0 {
		return runs
	}
	// Merge a run touching the previous one so every row is maximal.
	if n := len(runs); n > 0 && runs[n-1].y == y && runs[n-1].z == z && runs[n-1].x1 >= x0-1 {
		runs[n-1].x1 = max(runs[n-1].x1, x1)
		return runs
	}
	return append(runs, shapeRun{y: y, z: z, x0: x0, x1: x1})
}

func uniformAxis(n int, step float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = float64(i) * step
	}
	return out
}

// shapeGrid is the voxel grid: per-axis centre coordinates and cell
// boundaries in a local orthonormal frame, mapped to patient space by
// origin + Σ dir[k]·local[k].
type shapeGrid struct {
	dim    int
	size   [3]int
	coord  [3][]float64
	bound  [3][]float64 // cell boundaries, size[k]+1 each
	space  [3]float64   // mean spacing per axis, for the Crofton estimate
	origin render.Vec3
	dir    [3]render.Vec3
}

// newShapeGrid validates coordinates and directions. single is the cell
// width of an axis with one sample.
func newShapeGrid(planar bool, size [3]int, coords [3][]float64, single [3]float64, opts ShapeOptions) (*shapeGrid, error) {
	g := &shapeGrid{dim: 3, size: size, coord: coords, origin: opts.Origin, dir: opts.Direction}
	if planar {
		if size[2] != 1 {
			return nil, fmt.Errorf("%w: planar statistics need exactly one slice", ErrInvalidShapeInput)
		}
		g.dim = 2
		g.coord[2] = []float64{0}
		single[2] = 1
	}
	for k := 0; k < 3; k++ {
		c := g.coord[k]
		if size[k] <= 0 || len(c) != size[k] {
			return nil, fmt.Errorf("%w: axis %d has %d coordinates for %d samples", ErrInvalidShapeInput, k, len(c), size[k])
		}
		for i, v := range c {
			if math.IsNaN(v) || math.IsInf(v, 0) || i > 0 && v <= c[i-1] {
				return nil, fmt.Errorf("%w: axis %d coordinates must be finite and ascending", ErrInvalidShapeInput, k)
			}
		}
		g.bound[k] = cellBoundaries(c, single[k])
		g.space[k] = g.bound[k][1] - g.bound[k][0]
		if n := len(c); n > 1 {
			g.space[k] = (c[n-1] - c[0]) / float64(n-1)
		}
	}
	used := g.dir[:g.dim]
	allZero := true
	for _, d := range used {
		if d != (render.Vec3{}) {
			allZero = false
		}
	}
	if allZero {
		g.dir = [3]render.Vec3{{X: 1}, {Y: 1}, {Z: 1}}
	} else {
		for i, d := range used {
			if math.Abs(d.Length()-1) > 1e-6 {
				return nil, fmt.Errorf("%w: direction %d is not a unit vector", ErrInvalidShapeInput, i)
			}
			for j := 0; j < i; j++ {
				if math.Abs(d.Dot(used[j])) > 1e-6 {
					return nil, fmt.Errorf("%w: directions %d and %d are not orthogonal", ErrInvalidShapeInput, j, i)
				}
			}
		}
	}
	if !finiteVec(g.origin) {
		return nil, fmt.Errorf("%w: origin is not finite", ErrInvalidShapeInput)
	}
	return g, nil
}

func finiteVec(v render.Vec3) bool {
	for _, c := range [3]float64{v.X, v.Y, v.Z} {
		if math.IsNaN(c) || math.IsInf(c, 0) {
			return false
		}
	}
	return true
}

// cellBoundaries puts each boundary halfway between centres and extends the
// end cells by half the adjacent gap, matching sliceThickness.
func cellBoundaries(c []float64, single float64) []float64 {
	n := len(c)
	b := make([]float64, n+1)
	if n == 1 {
		b[0], b[1] = c[0]-single/2, c[0]+single/2
		return b
	}
	for i := 1; i < n; i++ {
		b[i] = (c[i-1] + c[i]) / 2
	}
	b[0] = c[0] - (c[1]-c[0])/2
	b[n] = c[n-1] + (c[n-1]-c[n-2])/2
	return b
}

func segmentationShapeGrid(s *Segmentation3D) (*shapeGrid, error) {
	geom := s.Geometry
	slices := s.Slices()
	n := len(geom.Slices)
	if n == 0 {
		n = len(geom.Positions)
	}
	if len(slices) > 0 {
		if slices[0] < 0 {
			return nil, fmt.Errorf("%w: negative slice index %d", ErrInvalidShapeInput, slices[0])
		}
		if last := slices[len(slices)-1]; last >= n {
			n = last + 1
		}
	}
	if n == 0 {
		n = 1
	}
	colSp, rowSp := sliceInPlaneSpacing(geom, 0)
	z := make([]float64, n)
	if len(geom.Positions) >= n {
		for i := range z {
			z[i] = geom.Positions[i] - geom.Positions[0]
		}
	} else {
		step := geom.MeanSpacing
		if !(step > 0) {
			step = 1
		}
		z = uniformAxis(n, step)
	}
	opts := ShapeOptions{Direction: [3]render.Vec3{geom.RowDir, geom.ColDir, geom.Normal}}
	if len(geom.Slices) > 0 {
		first := geom.Slices[0]
		opts.Origin = first.Origin
		if opts.Direction[0] == (render.Vec3{}) {
			opts.Direction = [3]render.Vec3{first.RowDir, first.ColDir, first.Normal}
		}
	}
	if d := &opts.Direction; d[2] == (render.Vec3{}) && d[0] != (render.Vec3{}) {
		d[2] = d[0].Cross(d[1])
	}
	return newShapeGrid(false, [3]int{s.Columns, s.Rows, n},
		[3][]float64{uniformAxis(s.Columns, colSp), uniformAxis(s.Rows, rowSp), z},
		[3]float64{colSp, rowSp, sliceThickness(geom, 0)}, opts)
}

// toPatient maps a local point to patient space.
func (g *shapeGrid) toPatient(p [3]float64) render.Vec3 {
	return g.origin.Add(g.toPatientVector(p))
}

func (g *shapeGrid) toPatientVector(v [3]float64) render.Vec3 {
	out := render.Vec3{}
	for k := 0; k < g.dim; k++ {
		out = out.Add(g.dir[k].Scale(v[k]))
	}
	return out
}

func (g *shapeGrid) width(k, i int) float64 { return g.bound[k][i+1] - g.bound[k][i] }

// statistics computes every measure for one object given as maximal runs
// sorted by slice, row and column.
func (g *shapeGrid) statistics(runs []shapeRun) ShapeStatistics {
	out := ShapeStatistics{Dimension: g.dim}
	if len(runs) == 0 {
		return out
	}
	d := g.dim
	cx, cy, cz := g.coord[0], g.coord[1], g.coord[2]

	// Pass 1: count, size, bounding box, border contact and centroid,
	// accumulated relative to the first voxel for numerical stability.
	ref := [3]float64{cx[runs[0].x0], cy[runs[0].y], cz[runs[0].z]}
	lo := [3]int{runs[0].x0, runs[0].y, runs[0].z}
	hi := lo
	var weight float64
	var first [3]float64
	last := [3]int{g.size[0] - 1, g.size[1] - 1, g.size[2] - 1}
	for _, r := range runs {
		wyz := g.width(1, r.y)
		if d == 3 {
			wyz *= g.width(2, r.z)
		}
		dy, dz := cy[r.y]-ref[1], cz[r.z]-ref[2]
		for x := r.x0; x <= r.x1; x++ {
			w := g.width(0, x) * wyz
			weight += w
			first[0] += w * (cx[x] - ref[0])
			first[1] += w * dy
			first[2] += w * dz
		}
		out.VoxelCount += r.x1 - r.x0 + 1
		lo[0], hi[0] = min(lo[0], r.x0), max(hi[0], r.x1)
		lo[1], hi[1] = min(lo[1], r.y), max(hi[1], r.y)
		lo[2], hi[2] = min(lo[2], r.z), max(hi[2], r.z)
		g.borderContact(&out, r, last)
	}
	out.PhysicalSize = weight
	out.BoundingBoxMin, out.BoundingBoxMax = lo, hi
	var mean [3]float64
	for k := 0; k < 3; k++ {
		mean[k] = ref[k] + first[k]/weight
	}
	if d == 2 {
		mean[2] = 0
	}
	out.Centroid = g.toPatient(mean)

	// Pass 2: central second moments.
	var cov [3][3]float64
	for _, r := range runs {
		wyz := g.width(1, r.y)
		if d == 3 {
			wyz *= g.width(2, r.z)
		}
		dy, dz := cy[r.y]-mean[1], cz[r.z]-mean[2]
		if d == 2 {
			dz = 0
		}
		for x := r.x0; x <= r.x1; x++ {
			w := g.width(0, x) * wyz
			dx := cx[x] - mean[0]
			cov[0][0] += w * dx * dx
			cov[0][1] += w * dx * dy
			cov[0][2] += w * dx * dz
			cov[1][1] += w * dy * dy
			cov[1][2] += w * dy * dz
			cov[2][2] += w * dz * dz
		}
	}
	matrix := make([][]float64, d)
	for i := 0; i < d; i++ {
		matrix[i] = make([]float64, d)
		for j := 0; j < d; j++ {
			a, b := min(i, j), max(i, j)
			matrix[i][j] = cov[a][b] / weight
		}
	}
	moments, vectors, err := linalg.SymmetricEigen(matrix)
	if err != nil { // unreachable for finite coordinates
		moments = make([]float64, d)
		vectors = make([][]float64, d)
		for i := range vectors {
			vectors[i] = make([]float64, d)
			vectors[i][i] = 1
		}
	}
	top := math.Max(moments[d-1], 0)
	for i := range moments {
		if moments[i] <= 1e-12*top {
			moments[i] = 0
		}
	}
	axes := g.principalAxes(vectors)
	out.PrincipalMoments = moments
	out.PrincipalAxes = make([]render.Vec3, d)
	for k := range axes {
		out.PrincipalAxes[k] = g.toPatientVector(axes[k])
	}
	if moments[0] > 0 {
		out.Flatness = math.Sqrt(moments[1] / moments[0])
	}
	if moments[d-2] > 0 {
		out.Elongation = math.Sqrt(moments[d-1] / moments[d-2])
	}

	// Equivalent sphere (circle) and ellipsoid (ellipse).
	if d == 3 {
		out.EquivalentSphericalRadius = math.Cbrt(3 * weight / (4 * math.Pi))
		out.EquivalentSphericalPerimeter = 4 * math.Pi * out.EquivalentSphericalRadius * out.EquivalentSphericalRadius
	} else {
		out.EquivalentSphericalRadius = math.Sqrt(weight / math.Pi)
		out.EquivalentSphericalPerimeter = 2 * math.Pi * out.EquivalentSphericalRadius
	}
	out.EquivalentEllipsoidDiameter = make([]float64, d)
	det := 1.0
	for _, m := range moments {
		det *= m
	}
	if det > 0 {
		det = math.Pow(det, 1/float64(d))
		for i, m := range moments {
			out.EquivalentEllipsoidDiameter[i] = 2 * out.EquivalentSphericalRadius * math.Sqrt(m/det)
		}
	}

	feret, a, b := g.feretDiameter(runs, lo, hi)
	out.FeretDiameter = feret
	out.FeretEndpoints = [2]render.Vec3{g.toPatient(a), g.toPatient(b)}

	out.Perimeter = g.croftonPerimeter(runs, lo, hi)
	if out.Perimeter > 0 {
		out.Sphericity = out.EquivalentSphericalPerimeter / out.Perimeter
		out.PerimeterOnBorderRatio = out.PerimeterOnBorder / out.Perimeter
	}
	out.OrientedBoundingBox = g.orientedBoundingBox(runs, mean, axes, out.Centroid, out.PrincipalAxes)
	return out
}

// borderContact adds the run's voxels and cell faces on the grid boundary.
func (g *shapeGrid) borderContact(out *ShapeStatistics, r shapeRun, last [3]int) {
	n := r.x1 - r.x0 + 1
	wy, wz := g.width(1, r.y), 1.0
	if g.dim == 3 {
		wz = g.width(2, r.z)
	}
	onRowBorder := r.y == 0 || r.y == last[1] || g.dim == 3 && (r.z == 0 || r.z == last[2])
	switch {
	case onRowBorder:
		out.VoxelsOnBorder += n
	default:
		if r.x0 == 0 {
			out.VoxelsOnBorder++
		}
		if r.x1 == last[0] && (r.x0 != 0 || n > 1) {
			out.VoxelsOnBorder++
		}
	}
	if r.x0 == 0 {
		out.PerimeterOnBorder += wy * wz
	}
	if r.x1 == last[0] {
		out.PerimeterOnBorder += wy * wz
	}
	runWidth := g.bound[0][r.x1+1] - g.bound[0][r.x0]
	for _, edge := range [2]bool{r.y == 0, r.y == last[1]} {
		if edge {
			out.PerimeterOnBorder += runWidth * wz
		}
	}
	if g.dim == 3 {
		for _, edge := range [2]bool{r.z == 0, r.z == last[2]} {
			if edge {
				out.PerimeterOnBorder += runWidth * wy
			}
		}
	}
}

// principalAxes returns the eigenvectors (columns of vectors) as local
// vectors with the sign convention: the largest patient-space component of
// each axis is positive (the first such component on ties), then the last
// axis is flipped if needed so the basis is right-handed (in patient space
// in 3D, in the grid plane in 2D).
func (g *shapeGrid) principalAxes(vectors [][]float64) [][3]float64 {
	d := g.dim
	axes := make([][3]float64, d)
	for k := 0; k < d; k++ {
		for i := 0; i < d; i++ {
			axes[k][i] = vectors[i][k]
		}
		p := g.toPatientVector(axes[k])
		comps := [3]float64{p.X, p.Y, p.Z}
		best := 0.0
		for _, c := range comps {
			best = math.Max(best, math.Abs(c))
		}
		for _, c := range comps {
			if math.Abs(c) >= best-1e-9 {
				if c < 0 {
					for i := range axes[k] {
						axes[k][i] = -axes[k][i]
					}
				}
				break
			}
		}
	}
	var det float64
	if d == 3 {
		a, b, c := g.toPatientVector(axes[0]), g.toPatientVector(axes[1]), g.toPatientVector(axes[2])
		det = a.Cross(b).Dot(c)
	} else {
		det = axes[0][0]*axes[1][1] - axes[0][1]*axes[1][0]
	}
	if det < 0 {
		for i := range axes[d-1] {
			axes[d-1][i] = -axes[d-1][i]
		}
	}
	return axes
}

// orientedBoundingBox projects every run's cell box corners on the local
// principal axes.
func (g *shapeGrid) orientedBoundingBox(runs []shapeRun, mean [3]float64, axes [][3]float64, centroid render.Vec3, patientAxes []render.Vec3) OrientedBoundingBox {
	d := g.dim
	lo := make([]float64, d)
	hi := make([]float64, d)
	for k := range lo {
		lo[k], hi[k] = math.Inf(1), math.Inf(-1)
	}
	for _, r := range runs {
		xs := [2]float64{g.bound[0][r.x0] - mean[0], g.bound[0][r.x1+1] - mean[0]}
		ys := [2]float64{g.bound[1][r.y] - mean[1], g.bound[1][r.y+1] - mean[1]}
		zs := [2]float64{0, 0}
		if d == 3 {
			zs = [2]float64{g.bound[2][r.z] - mean[2], g.bound[2][r.z+1] - mean[2]}
		}
		for k := 0; k < d; k++ {
			a := axes[k]
			// The extremes of a linear function over a box are at the
			// corners chosen per axis by the coefficient's sign.
			pmin, pmax := 0.0, 0.0
			for i, span := range [3][2]float64{xs, ys, zs} {
				u, v := a[i]*span[0], a[i]*span[1]
				pmin += math.Min(u, v)
				pmax += math.Max(u, v)
			}
			lo[k], hi[k] = math.Min(lo[k], pmin), math.Max(hi[k], pmax)
		}
	}
	box := OrientedBoundingBox{Origin: centroid, Axes: append([]render.Vec3(nil), patientAxes...), Size: make([]float64, d)}
	for k := 0; k < d; k++ {
		box.Size[k] = hi[k] - lo[k]
		box.Origin = box.Origin.Add(patientAxes[k].Scale(lo[k]))
	}
	return box
}
