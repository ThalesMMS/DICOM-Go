package spatialreg

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"sync"

	"github.com/ThalesMMS/dicom-go/render"
)

// Regional, cancellable Jacobian determinant of a deformable registration
// (#1257). The determinant det(A + ∇D)/det(A) at every grid node is the
// local volume change, after the behaviour of ITK's
// DisplacementFieldJacobianDeterminantFilter (central differences inside the
// grid, one-sided ones on its faces); this is an independent Go
// implementation. A summary of the whole grid is computed without
// materializing it, and a region (an index box of nodes) yields only its own
// values, computed from the same neighbours as the whole grid would use.

// GridRegion is a box of grid nodes: Min inclusive, Max exclusive, per axis.
type GridRegion struct {
	Min, Max [3]int
}

// Size is the number of nodes along each axis (zero when empty).
func (r GridRegion) Size() [3]int {
	var size [3]int
	for axis := range size {
		size[axis] = max(0, r.Max[axis]-r.Min[axis])
	}
	return size
}

// Empty reports whether the region holds no node.
func (r GridRegion) Empty() bool {
	size := r.Size()
	return size[0] == 0 || size[1] == 0 || size[2] == 0
}

// Contains reports whether node (x, y, z) is inside the region.
func (r GridRegion) Contains(x, y, z int) bool {
	at := [3]int{x, y, z}
	for axis := range at {
		if at[axis] < r.Min[axis] || at[axis] >= r.Max[axis] {
			return false
		}
	}
	return true
}

// Intersect is the part of r inside other.
func (r GridRegion) Intersect(other GridRegion) GridRegion {
	var out GridRegion
	for axis := 0; axis < 3; axis++ {
		out.Min[axis] = max(r.Min[axis], other.Min[axis])
		out.Max[axis] = max(out.Min[axis], min(r.Max[axis], other.Max[axis]))
	}
	return out
}

// WholeRegion is every node of the grid.
func (g *Grid) WholeRegion() GridRegion {
	if g == nil {
		return GridRegion{}
	}
	return GridRegion{Max: [3]int{int(g.Dimensions[0]), int(g.Dimensions[1]), int(g.Dimensions[2])}}
}

// ContinuousIndex is the grid index (x, y, z) of a patient point in the
// registered Frame of Reference.
func (g *Grid) ContinuousIndex(point render.Vec3) render.Vec3 {
	delta := point.Sub(g.Origin)
	return render.Vec3{
		X: delta.Dot(g.RowDirection) / g.Spacing.X,
		Y: delta.Dot(g.ColumnDirection) / g.Spacing.Y,
		Z: delta.Dot(g.NormalDirection) / g.Spacing.Z,
	}
}

// NodePoint is the patient position of node (x, y, z).
func (g *Grid) NodePoint(x, y, z int) render.Vec3 {
	return g.Origin.
		Add(g.RowDirection.Scale(float64(x) * g.Spacing.X)).
		Add(g.ColumnDirection.Scale(float64(y) * g.Spacing.Y)).
		Add(g.NormalDirection.Scale(float64(z) * g.Spacing.Z))
}

// VolumeChange is the local volume change det(A + ∇D)/det(A) of a
// deformable registration at the nodes of a block of its grid, for quality
// assurance: central differences inside the grid, one-sided ones on its
// faces (see Registration.Jacobian).
type VolumeChange struct {
	// Dimensions is the size of Region, the block of nodes computed.
	Dimensions [3]uint32
	Region     GridRegion
	// Values holds a determinant per node of Region, x fastest; NaN where a
	// neighbouring vector is undefined. It is nil for a summary.
	Values   []float64
	Min, Max float64
	// MinNode is the grid index of the smallest determinant.
	MinNode [3]int
	// Folded counts nodes with a determinant ≤ 0, FoldedBounds the block
	// of nodes holding them (empty when none), Undefined the nodes next to
	// an undefined vector.
	Folded       int
	FoldedBounds GridRegion
	Undefined    int
}

// VolumeChange computes the volume change map of the registration's whole
// grid.
func (r *Registration) VolumeChange() (VolumeChange, error) {
	return r.Jacobian(context.Background(), JacobianOptions{})
}

// JacobianOptions configure Registration.Jacobian.
type JacobianOptions struct {
	// Region restricts the computation to a box of nodes (nil: the whole
	// grid); it is clipped to the grid.
	Region *GridRegion
	// SummaryOnly computes the bounds and counts without the per-node
	// values: nothing proportional to the region is allocated.
	SummaryOnly bool
	// Workers bounds the goroutines (0: GOMAXPROCS).
	Workers int
}

// Jacobian computes the volume change det(A + ∇D)/det(A) over a region of
// the registration's grid, or only its summary, on several goroutines; ctx
// cancels it between planes. The result's Dimensions and Region describe
// the region; its Values (x fastest) are nil in SummaryOnly mode.
func (r *Registration) Jacobian(ctx context.Context, opts JacobianOptions) (VolumeChange, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if r == nil || r.Grid == nil {
		return VolumeChange{}, fmt.Errorf("%w: no deformation grid", ErrInvalidObject)
	}
	a := linearPart(r.Pre.Values)
	base := a.Determinant()
	if base == 0 || !finite(base) {
		return VolumeChange{}, fmt.Errorf("%w: singular pre-matrix", ErrInvalidObject)
	}
	g := r.Grid
	region := g.WholeRegion()
	if opts.Region != nil {
		region = opts.Region.Intersect(region)
	}
	size := region.Size()
	out := VolumeChange{
		Dimensions: [3]uint32{uint32(size[0]), uint32(size[1]), uint32(size[2])},
		Region:     region, Min: math.Inf(1), Max: math.Inf(-1),
	}
	if region.Empty() {
		return out, ctx.Err()
	}
	if !opts.SummaryOnly {
		out.Values = make([]float64, size[0]*size[1]*size[2])
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	workers = min(workers, size[2])
	// Each worker summarizes whole planes; the partial summaries are merged
	// in plane order, so the result does not depend on the scheduling.
	partial := make([]jacobianSummary, size[2])
	var next int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				mu.Lock()
				plane := next
				next++
				mu.Unlock()
				if plane >= size[2] {
					return
				}
				partial[plane] = r.jacobianPlane(a, base, region, region.Min[2]+plane, out.Values)
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return VolumeChange{}, err
	}
	summary := newJacobianSummary()
	for _, p := range partial {
		summary.merge(p)
	}
	out.Min, out.Max, out.Folded, out.Undefined = summary.min, summary.max, summary.folded, summary.undefined
	out.MinNode, out.FoldedBounds = summary.minNode, summary.foldedBounds
	if summary.folded == 0 {
		out.FoldedBounds = GridRegion{}
	}
	return out, nil
}

// jacobianSummary accumulates one plane or the whole region.
type jacobianSummary struct {
	min, max          float64
	minNode           [3]int
	folded, undefined int
	foldedBounds      GridRegion
}

func newJacobianSummary() jacobianSummary {
	s := jacobianSummary{min: math.Inf(1), max: math.Inf(-1)}
	for axis := 0; axis < 3; axis++ {
		s.foldedBounds.Min[axis], s.foldedBounds.Max[axis] = math.MaxInt, math.MinInt
	}
	return s
}

func (s *jacobianSummary) add(value float64, node [3]int) {
	if value < s.min {
		s.min, s.minNode = value, node
	}
	s.max = math.Max(s.max, value)
	if !(value > 0) {
		s.folded++
		for axis := 0; axis < 3; axis++ {
			s.foldedBounds.Min[axis] = min(s.foldedBounds.Min[axis], node[axis])
			s.foldedBounds.Max[axis] = max(s.foldedBounds.Max[axis], node[axis]+1)
		}
	}
}

func (s *jacobianSummary) merge(o jacobianSummary) {
	if o.min < s.min {
		s.min, s.minNode = o.min, o.minNode
	}
	s.max = math.Max(s.max, o.max)
	s.folded += o.folded
	s.undefined += o.undefined
	for axis := 0; axis < 3; axis++ {
		s.foldedBounds.Min[axis] = min(s.foldedBounds.Min[axis], o.foldedBounds.Min[axis])
		s.foldedBounds.Max[axis] = max(s.foldedBounds.Max[axis], o.foldedBounds.Max[axis])
	}
}

// jacobianPlane computes plane z of the region, writing its values when
// values is not nil.
func (r *Registration) jacobianPlane(a matrix3, base float64, region GridRegion, z int, values []float64) jacobianSummary {
	g := r.Grid
	n := [3]int{int(g.Dimensions[0]), int(g.Dimensions[1]), int(g.Dimensions[2])}
	size := region.Size()
	summary := newJacobianSummary()
	for y := region.Min[1]; y < region.Max[1]; y++ {
		for x := region.Min[0]; x < region.Max[0]; x++ {
			at := [3]int{x, y, z}
			value, ok := g.nodeDeterminant(a, n, at)
			if ok {
				value /= base
			} else {
				value = math.NaN()
			}
			if values != nil {
				values[((z-region.Min[2])*size[1]+(y-region.Min[1]))*size[0]+(x-region.Min[0])] = value
			}
			if !ok {
				summary.undefined++
				continue
			}
			summary.add(value, at)
		}
	}
	return summary
}

// nodeDeterminant is det(A + ∇D) at a node, with ∇D by central differences
// inside the grid and one-sided differences on its faces; false when a
// neighbouring vector is undefined.
func (g *Grid) nodeDeterminant(a matrix3, n [3]int, at [3]int) (float64, bool) {
	directions := [3]render.Vec3{g.RowDirection, g.ColumnDirection, g.NormalDirection}
	spacing := [3]float64{g.Spacing.X, g.Spacing.Y, g.Spacing.Z}
	m := a
	for axis := 0; axis < 3; axis++ {
		if n[axis] < 2 {
			continue
		}
		low, high := at, at
		low[axis] = max(0, at[axis]-1)
		high[axis] = min(n[axis]-1, at[axis]+1)
		lowV, ok := g.VectorAt(low[0], low[1], low[2])
		highV, ok2 := g.VectorAt(high[0], high[1], high[2])
		if !ok || !ok2 {
			return 0, false
		}
		derivative := highV.Sub(lowV).Scale(1 / float64(high[axis]-low[axis]))
		d := directions[axis].Scale(1 / spacing[axis])
		for i, component := range [3]float64{derivative.X, derivative.Y, derivative.Z} {
			m[i][0] += component * d.X
			m[i][1] += component * d.Y
			m[i][2] += component * d.Z
		}
	}
	return m.Determinant(), true
}

// VolumeChangeAt is the volume change det(A + ∇D)/det(A) at a registered
// point, with ∇D the gradient of the trilinear interpolant in the cell
// holding it (the determinant InvertPoint checks for folding).
func (r *Registration) VolumeChangeAt(point render.Vec3) (float64, error) {
	if r == nil {
		return 0, fmt.Errorf("%w: no registration", ErrInvalidObject)
	}
	return r.jacobianDeterminant(point)
}
