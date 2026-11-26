package roi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/ThalesMMS/dicom-go/render"
)

// Volumes of interest in physical space (#1116): spheres, ellipsoids and
// boxes given in patient millimetres, rasterized onto a voxel grid with
// optional partial-volume weights, with weighted 3D statistics (voi.go) and
// the PERCIST peak (voi_peak.go).
//
// Membership follows ITK's spatial objects (Apache-2.0, Copyright NumFOCUS;
// behaviour re-implemented, not copied): EllipseSpatialObject counts a point
// inside when Σ((x_i−c_i)/r_i)² < 1, so the ellipsoid surface is outside,
// while BoxSpatialObject tests its bounding box, whose IsInside includes the
// faces. SpatialObjectToImageStatisticsCalculator walks the index region of
// the object's bounding box and keeps the voxels whose centre is inside;
// VOICentreInside is that rule. Modifications: explicit validation errors
// instead of degenerate-radius special cases, clipping to the grid reported
// in VOIMask.Clipped, and VOIPartialVolume weights for the covered fraction.

// MaxVOIVoxels bounds the voxels a rasterization or a value cache may visit
// (a 256³ box: a sphere of radius 64 mm on a 0.5 mm grid).
const MaxVOIVoxels = 1 << 24

// DefaultVOISupersampling is the per-axis subsample count of VOIPartialVolume
// (5³ = 125 points per boundary voxel); MaxVOISupersampling caps it.
const (
	DefaultVOISupersampling = 5
	MaxVOISupersampling     = 16
)

// orthonormalTolerance bounds |a·b| and ||a|−1| for primitive axes.
const orthonormalTolerance = 1e-6

// surfaceTolerance is the relative distance within which a point counts as
// on a primitive's surface. Voxel centres exactly on the surface (a 7 mm
// radius on a 0.5 mm grid) land a few ulps inside or outside depending on
// rounding; the tolerance keeps such ties on the surface, so the ITK rule
// (surface outside for spheres and ellipsoids, faces inside for boxes)
// applies to them and masks stay point-symmetric.
const surfaceTolerance = 1e-9

// ErrInvalidVOI reports an unusable grid, primitive or option.
var ErrInvalidVOI = errors.New("roi: invalid volume of interest")

// VoxelGrid places the voxels of a volume in patient space: the centre of
// voxel (i, j, k) is Origin + i·ColumnStep + j·RowStep + k·SliceStep (mm).
// The steps carry direction and spacing, so oblique and anisotropic grids
// need no separate fields; they must be linearly independent.
type VoxelGrid struct {
	Columns, Rows, Slices int
	// Origin is the patient position (mm) of the centre of voxel (0, 0, 0).
	Origin render.Vec3
	// ColumnStep, RowStep and SliceStep are the patient displacements (mm) of
	// one index along columns, rows and slices.
	ColumnStep, RowStep, SliceStep render.Vec3
}

// VoxelGridFromDescriptor returns the grid of a canonical volume snapshot:
// its dimensions and IndexToPatientLPS affine.
func VoxelGridFromDescriptor(d render.VolumeDescriptor) (VoxelGrid, error) {
	a := d.IndexToPatientLPS
	g := VoxelGrid{
		Columns: int(d.Dimensions[0]), Rows: int(d.Dimensions[1]), Slices: int(d.Dimensions[2]),
		Origin:     render.Vec3{X: a[3], Y: a[7], Z: a[11]},
		ColumnStep: render.Vec3{X: a[0], Y: a[4], Z: a[8]},
		RowStep:    render.Vec3{X: a[1], Y: a[5], Z: a[9]},
		SliceStep:  render.Vec3{X: a[2], Y: a[6], Z: a[10]},
	}
	return g, g.Validate()
}

// Validate reports non-positive dimensions, non-finite geometry or steps
// that do not span 3D space.
func (g VoxelGrid) Validate() error {
	_, err := g.transform()
	return err
}

// IndexToPoint returns the patient position (mm) of a continuous index.
func (g VoxelGrid) IndexToPoint(i, j, k float64) render.Vec3 {
	return g.Origin.Add(g.ColumnStep.Scale(i)).Add(g.RowStep.Scale(j)).Add(g.SliceStep.Scale(k))
}

// PointToIndex returns the continuous index of a patient position; ok is
// false for an invalid grid.
func (g VoxelGrid) PointToIndex(p render.Vec3) (render.Vec3, bool) {
	t, err := g.transform()
	if err != nil {
		return render.Vec3{}, false
	}
	return t.toIndex(p), true
}

// VoxelVolumeMM3 is the volume of one voxel, |det[ColumnStep RowStep
// SliceStep]|.
func (g VoxelGrid) VoxelVolumeMM3() float64 {
	return math.Abs(g.ColumnStep.Dot(g.RowStep.Cross(g.SliceStep)))
}

// gridTransform caches a grid's inverse for index lookups.
type gridTransform struct {
	grid VoxelGrid
	// inverse rows map (p − Origin) to (i, j, k).
	inverse [3]render.Vec3
	// halfDiagonal is the largest centre-to-corner distance of a voxel.
	halfDiagonal float64
}

func (g VoxelGrid) transform() (gridTransform, error) {
	if g.Columns <= 0 || g.Rows <= 0 || g.Slices <= 0 {
		return gridTransform{}, fmt.Errorf("%w: grid dimensions %dx%dx%d must be positive", ErrInvalidVOI, g.Columns, g.Rows, g.Slices)
	}
	for _, v := range []render.Vec3{g.Origin, g.ColumnStep, g.RowStep, g.SliceStep} {
		if !finiteVec3(v) {
			return gridTransform{}, fmt.Errorf("%w: grid geometry must be finite", ErrInvalidVOI)
		}
	}
	c, r, s := g.ColumnStep, g.RowStep, g.SliceStep
	det := c.Dot(r.Cross(s))
	scale := c.Length() * r.Length() * s.Length()
	if !(scale > 0) || math.Abs(det) <= 1e-12*scale {
		return gridTransform{}, fmt.Errorf("%w: grid steps must be non-zero and linearly independent", ErrInvalidVOI)
	}
	t := gridTransform{grid: g}
	// Rows of the inverse of the matrix whose columns are c, r, s.
	t.inverse[0] = r.Cross(s).Scale(1 / det)
	t.inverse[1] = s.Cross(c).Scale(1 / det)
	t.inverse[2] = c.Cross(r).Scale(1 / det)
	for _, sign := range [][2]float64{{1, 1}, {1, -1}, {-1, 1}, {-1, -1}} {
		t.halfDiagonal = math.Max(t.halfDiagonal, c.Add(r.Scale(sign[0])).Add(s.Scale(sign[1])).Length()/2)
	}
	return t, nil
}

func (t gridTransform) toIndex(p render.Vec3) render.Vec3 {
	d := p.Sub(t.grid.Origin)
	return render.Vec3{X: t.inverse[0].Dot(d), Y: t.inverse[1].Dot(d), Z: t.inverse[2].Dot(d)}
}

func (t gridTransform) inGrid(i, j, k int) bool {
	g := t.grid
	return i >= 0 && j >= 0 && k >= 0 && i < g.Columns && j < g.Rows && k < g.Slices
}

// VOIPrimitive is a solid in patient space (mm).
type VOIPrimitive interface {
	// Validate reports unusable parameters.
	Validate() error
	// Contains reports whether a patient point is inside.
	Contains(p render.Vec3) bool
	// DistanceBound is a signed lower bound on the distance from p to the
	// surface: negative inside, positive outside, and never larger in
	// magnitude than the true Euclidean distance. Rasterization uses it to
	// classify whole voxels without subsampling them.
	DistanceBound(p render.Vec3) float64
	// Bounds returns the patient-space axis-aligned bounding box.
	Bounds() (lo, hi render.Vec3)
	// VolumeMM3 is the analytic volume.
	VolumeMM3() float64
}

// Sphere is a ball of RadiusMM around Center. Its surface is outside
// (ITK EllipseSpatialObject), within surfaceTolerance.
type Sphere struct {
	Center   render.Vec3
	RadiusMM float64
}

// SphereRadiusForVolume returns the radius (mm) of a sphere of volumeMM3:
// 6.2035 mm for the 1 cm³ PERCIST peak sphere.
func SphereRadiusForVolume(volumeMM3 float64) float64 {
	return math.Cbrt(3 * volumeMM3 / (4 * math.Pi))
}

func (s Sphere) Validate() error {
	if !finiteVec3(s.Center) {
		return fmt.Errorf("%w: sphere centre must be finite", ErrInvalidVOI)
	}
	if !finitePositive(s.RadiusMM) {
		return fmt.Errorf("%w: sphere radius %g mm must be finite and positive", ErrInvalidVOI, s.RadiusMM)
	}
	return nil
}

func (s Sphere) Contains(p render.Vec3) bool {
	d := p.Sub(s.Center)
	return d.Dot(d) < s.RadiusMM*s.RadiusMM*(1-2*surfaceTolerance)
}

func (s Sphere) DistanceBound(p render.Vec3) float64 {
	return p.Sub(s.Center).Length() - s.RadiusMM
}

func (s Sphere) Bounds() (render.Vec3, render.Vec3) {
	r := render.Vec3{X: s.RadiusMM, Y: s.RadiusMM, Z: s.RadiusMM}
	return s.Center.Sub(r), s.Center.Add(r)
}

func (s Sphere) VolumeMM3() float64 { return 4.0 / 3.0 * math.Pi * math.Pow(s.RadiusMM, 3) }

// Ellipsoid has semi-axes RadiiMM[i] along Axes[i] around Center. Axes must
// be orthonormal; the zero value means the patient X, Y and Z axes. Its
// surface is outside (ITK EllipseSpatialObject), within surfaceTolerance.
type Ellipsoid struct {
	Center  render.Vec3
	RadiiMM [3]float64
	Axes    [3]render.Vec3
}

func (e Ellipsoid) Validate() error {
	if !finiteVec3(e.Center) {
		return fmt.Errorf("%w: ellipsoid centre must be finite", ErrInvalidVOI)
	}
	for _, r := range e.RadiiMM {
		if !finitePositive(r) {
			return fmt.Errorf("%w: ellipsoid radii %v mm must be finite and positive", ErrInvalidVOI, e.RadiiMM)
		}
	}
	return validateAxes(e.Axes)
}

// normalizedRadius is sqrt(Σ(u_i/r_i)²) in the ellipsoid frame.
func (e Ellipsoid) normalizedRadius(p render.Vec3) float64 {
	axes := primitiveAxes(e.Axes)
	d := p.Sub(e.Center)
	sum := 0.0
	for i, axis := range axes {
		u := d.Dot(axis) / e.RadiiMM[i]
		sum += u * u
	}
	return math.Sqrt(sum)
}

func (e Ellipsoid) Contains(p render.Vec3) bool {
	axes := primitiveAxes(e.Axes)
	d := p.Sub(e.Center)
	sum := 0.0
	for i, axis := range axes {
		u := d.Dot(axis)
		sum += u * u / (e.RadiiMM[i] * e.RadiiMM[i])
	}
	return sum < 1-2*surfaceTolerance
}

// DistanceBound uses (q−1)·r_min, q the normalized radius: q is
// (1/r_min)-Lipschitz and equals 1 on the surface.
func (e Ellipsoid) DistanceBound(p render.Vec3) float64 {
	return (e.normalizedRadius(p) - 1) * math.Min(e.RadiiMM[0], math.Min(e.RadiiMM[1], e.RadiiMM[2]))
}

func (e Ellipsoid) Bounds() (render.Vec3, render.Vec3) {
	axes := primitiveAxes(e.Axes)
	var half [3]float64
	for k := range half {
		sum := 0.0
		for i, axis := range axes {
			c := e.RadiiMM[i] * vecComponent(axis, k)
			sum += c * c
		}
		half[k] = math.Sqrt(sum)
	}
	h := render.Vec3{X: half[0], Y: half[1], Z: half[2]}
	return e.Center.Sub(h), e.Center.Add(h)
}

func (e Ellipsoid) VolumeMM3() float64 {
	return 4.0 / 3.0 * math.Pi * e.RadiiMM[0] * e.RadiiMM[1] * e.RadiiMM[2]
}

// Box is a rectangular cuboid of edge lengths SizeMM[i] along Axes[i],
// centred on Center. Axes must be orthonormal; the zero value means the
// patient X, Y and Z axes. Its faces are inside (ITK BoxSpatialObject),
// within surfaceTolerance.
type Box struct {
	Center render.Vec3
	SizeMM [3]float64
	Axes   [3]render.Vec3
}

func (b Box) Validate() error {
	if !finiteVec3(b.Center) {
		return fmt.Errorf("%w: box centre must be finite", ErrInvalidVOI)
	}
	for _, s := range b.SizeMM {
		if !finitePositive(s) {
			return fmt.Errorf("%w: box size %v mm must be finite and positive", ErrInvalidVOI, b.SizeMM)
		}
	}
	return validateAxes(b.Axes)
}

func (b Box) Contains(p render.Vec3) bool {
	axes := primitiveAxes(b.Axes)
	d := p.Sub(b.Center)
	for i, axis := range axes {
		if half := b.SizeMM[i] / 2; math.Abs(d.Dot(axis)) > half*(1+surfaceTolerance) {
			return false
		}
	}
	return true
}

// DistanceBound is max_i(|u_i| − h_i): exact inside, a lower bound outside.
func (b Box) DistanceBound(p render.Vec3) float64 {
	axes := primitiveAxes(b.Axes)
	d := p.Sub(b.Center)
	bound := math.Inf(-1)
	for i, axis := range axes {
		bound = math.Max(bound, math.Abs(d.Dot(axis))-b.SizeMM[i]/2)
	}
	return bound
}

func (b Box) Bounds() (render.Vec3, render.Vec3) {
	axes := primitiveAxes(b.Axes)
	var half [3]float64
	for k := range half {
		for i, axis := range axes {
			half[k] += b.SizeMM[i] / 2 * math.Abs(vecComponent(axis, k))
		}
	}
	h := render.Vec3{X: half[0], Y: half[1], Z: half[2]}
	return b.Center.Sub(h), b.Center.Add(h)
}

func (b Box) VolumeMM3() float64 { return b.SizeMM[0] * b.SizeMM[1] * b.SizeMM[2] }

func primitiveAxes(axes [3]render.Vec3) [3]render.Vec3 {
	if axes == ([3]render.Vec3{}) {
		return [3]render.Vec3{{X: 1}, {Y: 1}, {Z: 1}}
	}
	return axes
}

func validateAxes(axes [3]render.Vec3) error {
	if axes == ([3]render.Vec3{}) {
		return nil
	}
	for i, a := range axes {
		if !finiteVec3(a) || math.Abs(a.Length()-1) > orthonormalTolerance {
			return fmt.Errorf("%w: axis %d must be a unit vector", ErrInvalidVOI, i)
		}
		for _, b := range axes[i+1:] {
			if math.Abs(a.Dot(b)) > orthonormalTolerance {
				return fmt.Errorf("%w: axes must be orthogonal", ErrInvalidVOI)
			}
		}
	}
	return nil
}

func vecComponent(v render.Vec3, k int) float64 {
	switch k {
	case 0:
		return v.X
	case 1:
		return v.Y
	}
	return v.Z
}

func finitePositive(v float64) bool { return v > 0 && !math.IsInf(v, 0) }

// VOIWeighting selects how rasterization weighs a voxel.
type VOIWeighting int

const (
	// VOICentreInside keeps a voxel with weight 1 when its centre is inside
	// (ITK SpatialObjectToImageStatisticsCalculator).
	VOICentreInside VOIWeighting = iota
	// VOIPartialVolume weighs a voxel by the fraction of it inside the
	// primitive, estimated on a Supersampling³ lattice of subvoxel centres;
	// voxels wholly inside or outside are classified without subsampling.
	VOIPartialVolume
)

func (w VOIWeighting) String() string {
	if w == VOIPartialVolume {
		return "partial volume"
	}
	return "voxel centre"
}

// VOIRasterOptions configures RasterizeVOI.
type VOIRasterOptions struct {
	Weighting VOIWeighting
	// Supersampling is the per-axis subsample count of VOIPartialVolume;
	// 0 means DefaultVOISupersampling.
	Supersampling int
}

func (o VOIRasterOptions) supersampling() (int, error) {
	switch o.Weighting {
	case VOICentreInside, VOIPartialVolume:
	default:
		return 0, fmt.Errorf("%w: unknown weighting %d", ErrInvalidVOI, o.Weighting)
	}
	n := o.Supersampling
	if n == 0 {
		n = DefaultVOISupersampling
	}
	if n < 1 || n > MaxVOISupersampling {
		return 0, fmt.Errorf("%w: supersampling %d must be in [1, %d]", ErrInvalidVOI, o.Supersampling, MaxVOISupersampling)
	}
	return n, nil
}

// VOIVoxel is one voxel of a rasterized VOI.
type VOIVoxel struct {
	Column, Row, Slice int
	// Weight is the covered fraction in (0, 1]; always 1 for
	// VOICentreInside.
	Weight float64
	// CentreInside reports that the voxel centre is inside the primitive.
	CentreInside bool
}

// VOIMask is a primitive rasterized on a grid: the voxels with a positive
// weight, x-fastest.
type VOIMask struct {
	Grid      VoxelGrid
	Primitive VOIPrimitive
	Weighting VOIWeighting
	Voxels    []VOIVoxel
	// Clipped reports that the primitive's bounding box reaches beyond the
	// grid's extent (the voxel centres plus a half-voxel border), so part of
	// the primitive may lie outside the image. It is conservative for
	// primitives oblique to the grid.
	Clipped bool
	// AnalyticVolumeMM3 is the primitive's exact volume.
	AnalyticVolumeMM3 float64
}

// VolumeMM3 is the rasterized volume, Σ weight · voxel volume.
func (m *VOIMask) VolumeMM3() float64 {
	if m == nil {
		return 0
	}
	sum := 0.0
	for _, v := range m.Voxels {
		sum += v.Weight
	}
	return sum * m.Grid.VoxelVolumeMM3()
}

// indexBox is an inclusive voxel index range.
type indexBox struct{ lo, hi [3]int }

func (b indexBox) empty() bool { return b.lo[0] > b.hi[0] || b.lo[1] > b.hi[1] || b.lo[2] > b.hi[2] }

func (b indexBox) count() int {
	if b.empty() {
		return 0
	}
	n := 1
	for i := range b.lo {
		n *= b.hi[i] - b.lo[i] + 1
		if n > MaxVOIVoxels {
			return MaxVOIVoxels + 1
		}
	}
	return n
}

// primitiveIndexBox returns the continuous index bounds of the primitive's
// bounding box (its eight corners mapped to index space).
func primitiveIndexBox(t gridTransform, primitive VOIPrimitive) (lo, hi render.Vec3) {
	pmin, pmax := primitive.Bounds()
	lo = render.Vec3{X: math.Inf(1), Y: math.Inf(1), Z: math.Inf(1)}
	hi = render.Vec3{X: math.Inf(-1), Y: math.Inf(-1), Z: math.Inf(-1)}
	for corner := 0; corner < 8; corner++ {
		p := pmin
		if corner&1 != 0 {
			p.X = pmax.X
		}
		if corner&2 != 0 {
			p.Y = pmax.Y
		}
		if corner&4 != 0 {
			p.Z = pmax.Z
		}
		idx := t.toIndex(p)
		lo = render.Vec3{X: math.Min(lo.X, idx.X), Y: math.Min(lo.Y, idx.Y), Z: math.Min(lo.Z, idx.Z)}
		hi = render.Vec3{X: math.Max(hi.X, idx.X), Y: math.Max(hi.Y, idx.Y), Z: math.Max(hi.Z, idx.Z)}
	}
	return lo, hi
}

// clippedIndexBox is the voxel range of a continuous index box intersected
// with the grid.
func clippedIndexBox(t gridTransform, lo, hi render.Vec3) indexBox {
	dims := [3]int{t.grid.Columns, t.grid.Rows, t.grid.Slices}
	los, his := [3]float64{lo.X, lo.Y, lo.Z}, [3]float64{hi.X, hi.Y, hi.Z}
	var box indexBox
	for i := range dims {
		box.lo[i] = int(math.Max(0, math.Floor(los[i])))
		box.hi[i] = int(math.Min(float64(dims[i]-1), math.Ceil(his[i])))
	}
	return box
}

// RasterizeVOI rasterizes a primitive on a grid. Voxels outside the grid are
// never part of the mask; Clipped reports a primitive that may extend past
// it.
func RasterizeVOI(ctx context.Context, grid VoxelGrid, primitive VOIPrimitive, opts VOIRasterOptions) (*VOIMask, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	t, err := grid.transform()
	if err != nil {
		return nil, err
	}
	if primitive == nil {
		return nil, fmt.Errorf("%w: nil primitive", ErrInvalidVOI)
	}
	if err := primitive.Validate(); err != nil {
		return nil, err
	}
	supersampling, err := opts.supersampling()
	if err != nil {
		return nil, err
	}
	lo, hi := primitiveIndexBox(t, primitive)
	mask := &VOIMask{
		Grid: grid, Primitive: primitive, Weighting: opts.Weighting,
		AnalyticVolumeMM3: primitive.VolumeMM3(),
		Clipped: lo.X < -0.5 || lo.Y < -0.5 || lo.Z < -0.5 ||
			hi.X > float64(grid.Columns)-0.5 || hi.Y > float64(grid.Rows)-0.5 || hi.Z > float64(grid.Slices)-0.5,
	}
	box := clippedIndexBox(t, lo, hi)
	if box.count() > MaxVOIVoxels {
		return nil, fmt.Errorf("%w: the VOI covers more than %d voxels", ErrInvalidVOI, MaxVOIVoxels)
	}
	err = rasterizeBox(ctx, t, primitive, box, opts.Weighting, supersampling, func(i, j, k int, weight float64, centre bool) {
		mask.Voxels = append(mask.Voxels, VOIVoxel{Column: i, Row: j, Slice: k, Weight: weight, CentreInside: centre})
	})
	if err != nil {
		return nil, err
	}
	return mask, nil
}

// rasterizeBox visits the voxels of box (which may extend beyond the grid)
// and emits those with a positive weight.
func rasterizeBox(ctx context.Context, t gridTransform, primitive VOIPrimitive, box indexBox, weighting VOIWeighting, supersampling int, emit func(i, j, k int, weight float64, centre bool)) error {
	if box.empty() {
		return nil
	}
	g := t.grid
	var offsets []render.Vec3
	if weighting == VOIPartialVolume {
		offsets = make([]render.Vec3, 0, supersampling*supersampling*supersampling)
		for c := 0; c < supersampling; c++ {
			for b := 0; b < supersampling; b++ {
				for a := 0; a < supersampling; a++ {
					u := func(n int) float64 { return (float64(n)+0.5)/float64(supersampling) - 0.5 }
					offsets = append(offsets, g.ColumnStep.Scale(u(a)).Add(g.RowStep.Scale(u(b))).Add(g.SliceStep.Scale(u(c))))
				}
			}
		}
	}
	for k := box.lo[2]; k <= box.hi[2]; k++ {
		for j := box.lo[1]; j <= box.hi[1]; j++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			for i := box.lo[0]; i <= box.hi[0]; i++ {
				p := g.IndexToPoint(float64(i), float64(j), float64(k))
				if weighting == VOICentreInside {
					if primitive.Contains(p) {
						emit(i, j, k, 1, true)
					}
					continue
				}
				bound := primitive.DistanceBound(p)
				if bound >= t.halfDiagonal {
					continue
				}
				centre := primitive.Contains(p)
				if -bound >= t.halfDiagonal {
					emit(i, j, k, 1, centre)
					continue
				}
				inside := 0
				for _, offset := range offsets {
					if primitive.Contains(p.Add(offset)) {
						inside++
					}
				}
				if inside > 0 {
					emit(i, j, k, float64(inside)/float64(len(offsets)), centre)
				}
			}
		}
	}
	return nil
}

// VoxelValueFunc returns the value of a grid voxel; ok is false when the
// voxel has no value.
type VoxelValueFunc func(column, row, slice int) (value float64, ok bool)

// VOIStatistics are weighted statistics of the values inside a VOI.
type VOIStatistics struct {
	// VoxelCount counts the mask voxels with a finite value.
	VoxelCount int
	// MissingVoxels counts mask voxels without a value (or a NaN/Inf one);
	// they are left out of every statistic.
	MissingVoxels int
	// WeightSum is Σ weight over the counted voxels.
	WeightSum float64
	// VolumeMM3 is WeightSum times the voxel volume.
	VolumeMM3 float64
	// Sum is Σ weight · value.
	Sum float64
	// Mean, Variance and StdDev are weighted by the voxel weights; the
	// variance is the population form (normalized by WeightSum).
	Mean, Variance, StdDev float64
	// Median is the weighted median: the value where the cumulative weight
	// reaches half of WeightSum, averaging the two neighbours on an exact tie.
	Median float64
	// Min and Max are unweighted extremes over the voxels whose centre is
	// inside, so they do not depend on the weighting mode; over all counted
	// voxels when no centre is inside. MaxVoxel and MaxPoint locate Max (the
	// first in x-fastest order on ties).
	Min, Max float64
	MaxVoxel [3]int
	MaxPoint render.Vec3
}

// ErrEmptyVOI reports a VOI without a single voxel that has a value.
var ErrEmptyVOI = errors.New("roi: the VOI contains no voxel with a value")

type weightedValue struct {
	value, weight float64
	index         int
}

// Statistics computes the weighted statistics of the mask over valueAt.
func (m *VOIMask) Statistics(valueAt VoxelValueFunc) (VOIStatistics, error) {
	if m == nil || valueAt == nil {
		return VOIStatistics{}, fmt.Errorf("%w: statistics need a mask and a value accessor", ErrInvalidVOI)
	}
	var stats VOIStatistics
	values := make([]weightedValue, 0, len(m.Voxels))
	for index, voxel := range m.Voxels {
		v, ok := valueAt(voxel.Column, voxel.Row, voxel.Slice)
		if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
			stats.MissingVoxels++
			continue
		}
		values = append(values, weightedValue{value: v, weight: voxel.Weight, index: index})
	}
	if len(values) == 0 {
		return stats, ErrEmptyVOI
	}
	stats.VoxelCount = len(values)
	// West's weighted incremental mean and variance.
	var mean, m2 float64
	extremesFromCentre := false
	for _, v := range values {
		if m.Voxels[v.index].CentreInside {
			extremesFromCentre = true
			break
		}
	}
	first := true
	for _, v := range values {
		stats.WeightSum += v.weight
		stats.Sum += v.weight * v.value
		delta := v.value - mean
		mean += delta * v.weight / stats.WeightSum
		m2 += v.weight * delta * (v.value - mean)
		voxel := m.Voxels[v.index]
		if extremesFromCentre && !voxel.CentreInside {
			continue
		}
		if first || v.value < stats.Min {
			stats.Min = v.value
		}
		if first || v.value > stats.Max {
			stats.Max = v.value
			stats.MaxVoxel = [3]int{voxel.Column, voxel.Row, voxel.Slice}
		}
		first = false
	}
	stats.Mean = mean
	stats.Variance = math.Max(0, m2/stats.WeightSum)
	stats.StdDev = math.Sqrt(stats.Variance)
	stats.VolumeMM3 = stats.WeightSum * m.Grid.VoxelVolumeMM3()
	stats.MaxPoint = m.Grid.IndexToPoint(float64(stats.MaxVoxel[0]), float64(stats.MaxVoxel[1]), float64(stats.MaxVoxel[2]))
	stats.Median = weightedMedian(values, stats.WeightSum)
	return stats, nil
}

// weightedMedian sorts values in place.
func weightedMedian(values []weightedValue, total float64) float64 {
	sort.SliceStable(values, func(a, b int) bool { return values[a].value < values[b].value })
	half := total / 2
	tolerance := 1e-12 * total
	cumulative := 0.0
	for i, v := range values {
		cumulative += v.weight
		if math.Abs(cumulative-half) <= tolerance && i+1 < len(values) {
			return (v.value + values[i+1].value) / 2
		}
		if cumulative > half {
			return v.value
		}
	}
	return values[len(values)-1].value
}
