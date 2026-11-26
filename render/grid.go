package render

import (
	"errors"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/internal/gridindex"
)

// Canonical grid conventions (#1115), those of ITK (itkImageBase.h,
// itkImageFunction.hxx at 35b196e1; written from the conventions, no ITK code
// is copied): integer indices are voxel centres; a
// continuous index maps to the discrete index floor(x+0.5)
// (itk::Math::RoundHalfIntegerUp, so -0.5 goes to 0 where math.Round gives
// -1); and a continuous index is inside an axis of n voxels when
// -0.5 <= x < n-0.5, so an axis of zero voxels holds nothing. Every
// point→index conversion, "inside" test and grid comparison uses these.

// NearestIndex is the discrete index of continuous index x, floor(x+0.5).
func NearestIndex(x float64) int { return gridindex.Nearest(x) }

// ContinuousIndexInside reports whether continuous index x lies on an axis of
// n voxels, -0.5 <= x < n-0.5.
func ContinuousIndexInside(x float64, n int) bool { return gridindex.Inside(x, n) }

// ClampContinuousIndex clamps continuous index x to the voxel centres [0,n-1]
// when it lies inside the axis widened by tolerance voxels on both sides
// (tolerance 0 is ContinuousIndexInside); ok is false outside. Samplers use
// it to repeat the edge voxels over the outer half voxel.
func ClampContinuousIndex(x float64, n int, tolerance float64) (clamped float64, ok bool) {
	return gridindex.Clamp(x, n, tolerance)
}

// NearestIndexInside is the discrete index of x on an axis of n voxels, with
// ok false outside it.
func NearestIndexInside(x float64, n int) (int, bool) {
	if !ContinuousIndexInside(x, n) {
		return 0, false
	}
	return min(NearestIndex(x), n-1), true
}

// Default grid comparison tolerances, those of ITK: the coordinate tolerance
// is a fraction of a voxel, the direction tolerance an absolute difference of
// direction cosines.
const (
	DefaultGridCoordinateTolerance = 1e-6
	DefaultGridDirectionTolerance  = 1e-6
)

// Tolerances for grids read from DICOM, whose positions, spacings and
// orientations are decimal strings of limited precision (SEG frames,
// parametric maps, dose grids): a twentieth of a voxel anywhere on the grid,
// and direction cosines within 1e-3.
const (
	DICOMGridCoordinateTolerance = 0.05
	DICOMGridDirectionTolerance  = 1e-3
)

// ErrInvalidGrid reports a grid without a usable point↔index mapping.
var ErrInvalidGrid = errors.New("render: invalid grid")

// Grid is an image grid: Size voxels along three index axes, the patient
// position of index (0,0,0), and the spacing and patient direction of each
// axis. Directions need not be orthogonal but must be independent.
type Grid struct {
	Size      [3]int
	Origin    Vec3
	Spacing   [3]float64
	Direction [3]Vec3
}

// NewGrid validates and returns a grid.
func NewGrid(size [3]int, origin Vec3, spacing [3]float64, direction [3]Vec3) (Grid, error) {
	g := Grid{Size: size, Origin: origin, Spacing: spacing, Direction: direction}
	return g, g.Validate()
}

// Validate rejects negative sizes, non-finite or non-positive spacing, and
// directions that are not finite or do not span space (zero determinant).
func (g Grid) Validate() error {
	for axis := 0; axis < 3; axis++ {
		if g.Size[axis] < 0 {
			return fmt.Errorf("%w: size %v", ErrInvalidGrid, g.Size)
		}
		if !finite(g.Spacing[axis]) || g.Spacing[axis] <= 0 {
			return fmt.Errorf("%w: spacing %v", ErrInvalidGrid, g.Spacing)
		}
		d := g.Direction[axis]
		if !finite(d.X) || !finite(d.Y) || !finite(d.Z) || d.Length() == 0 {
			return fmt.Errorf("%w: direction %v", ErrInvalidGrid, g.Direction)
		}
	}
	if !finite(g.Origin.X) || !finite(g.Origin.Y) || !finite(g.Origin.Z) {
		return fmt.Errorf("%w: origin %v", ErrInvalidGrid, g.Origin)
	}
	if _, ok := g.indexMatrix(); !ok {
		return fmt.Errorf("%w: direction matrix is singular", ErrInvalidGrid)
	}
	return nil
}

// IndexToPoint is the patient position of continuous index ci.
func (g Grid) IndexToPoint(ci Vec3) Vec3 {
	return g.Origin.
		Add(g.Direction[0].Scale(g.Spacing[0] * ci.X)).
		Add(g.Direction[1].Scale(g.Spacing[1] * ci.Y)).
		Add(g.Direction[2].Scale(g.Spacing[2] * ci.Z))
}

// ContinuousIndex is the continuous index of patient point p; ok is false for
// an invalid grid.
func (g Grid) ContinuousIndex(p Vec3) (Vec3, bool) {
	m, ok := g.indexMatrix()
	if !ok {
		return Vec3{}, false
	}
	d := p.Sub(g.Origin)
	return Vec3{
		X: m[0]*d.X + m[1]*d.Y + m[2]*d.Z,
		Y: m[3]*d.X + m[4]*d.Y + m[5]*d.Z,
		Z: m[6]*d.X + m[7]*d.Y + m[8]*d.Z,
	}, true
}

// Index is the discrete index of patient point p (floor(x+0.5) per axis) and
// whether p lies inside the grid.
func (g Grid) Index(p Vec3) (index [3]int, inside bool) {
	ci, ok := g.ContinuousIndex(p)
	if !ok {
		return index, false
	}
	index = [3]int{NearestIndex(ci.X), NearestIndex(ci.Y), NearestIndex(ci.Z)}
	return index, g.IsInsideContinuousIndex(ci)
}

// IsInside reports whether patient point p lies inside the grid.
func (g Grid) IsInside(p Vec3) bool {
	ci, ok := g.ContinuousIndex(p)
	return ok && g.IsInsideContinuousIndex(ci)
}

// IsInsideContinuousIndex reports whether ci lies inside the grid,
// -0.5 <= ci < Size-0.5 on every axis.
func (g Grid) IsInsideContinuousIndex(ci Vec3) bool {
	return ContinuousIndexInside(ci.X, g.Size[0]) &&
		ContinuousIndexInside(ci.Y, g.Size[1]) &&
		ContinuousIndexInside(ci.Z, g.Size[2])
}

// Congruent reports whether g and other occupy the same physical space with
// every index of g at the same place in both, within coordinateTolerance
// times g's smallest spacing, and with direction cosines within
// directionTolerance. Sizes are not compared.
//
// ITK's IsCongruentImageGeometry compares the origins and the spacings each
// against the tolerance; that lets a spacing difference accumulate across the
// grid (1% of a voxel per voxel is 5 voxels at the far edge of 512). Congruent
// bounds the position error itself: it is affine in the index, so checking
// the corners of g bounds it at every index.
func (g Grid) Congruent(other Grid, coordinateTolerance, directionTolerance float64) bool {
	for axis := 0; axis < 3; axis++ {
		if !vecWithin(g.Direction[axis], other.Direction[axis], math.Abs(directionTolerance)) {
			return false
		}
	}
	tolerance := math.Abs(coordinateTolerance) * math.Min(g.Spacing[0], math.Min(g.Spacing[1], g.Spacing[2]))
	if !(tolerance >= 0) {
		return false
	}
	var last [3]float64
	for axis := 0; axis < 3; axis++ {
		last[axis] = float64(max(g.Size[axis]-1, 0))
	}
	for corner := 0; corner < 8; corner++ {
		ci := Vec3{}
		if corner&1 != 0 {
			ci.X = last[0]
		}
		if corner&2 != 0 {
			ci.Y = last[1]
		}
		if corner&4 != 0 {
			ci.Z = last[2]
		}
		if g.IndexToPoint(ci).Sub(other.IndexToPoint(ci)).Length() > tolerance {
			return false
		}
	}
	return true
}

// SameGeometry is Congruent with equal sizes (the same-geometry fast path must
// compare the size too, ITK c6f1a74012).
func (g Grid) SameGeometry(other Grid, coordinateTolerance, directionTolerance float64) bool {
	return g.Size == other.Size && g.Congruent(other, coordinateTolerance, directionTolerance)
}

func vecWithin(a, b Vec3, tolerance float64) bool {
	return math.Abs(a.X-b.X) <= tolerance && math.Abs(a.Y-b.Y) <= tolerance && math.Abs(a.Z-b.Z) <= tolerance
}

// indexMatrix is the inverse of the index→patient matrix, row-major.
func (g Grid) indexMatrix() ([9]float64, bool) {
	c0 := g.Direction[0].Scale(g.Spacing[0])
	c1 := g.Direction[1].Scale(g.Spacing[1])
	c2 := g.Direction[2].Scale(g.Spacing[2])
	// Rows of the inverse are the cross products of the columns over det.
	r0, r1, r2 := c1.Cross(c2), c2.Cross(c0), c0.Cross(c1)
	det := c0.Dot(r0)
	scale := c0.Length() * c1.Length() * c2.Length()
	if !finite(det) || scale == 0 || math.Abs(det) <= 1e-12*scale {
		return [9]float64{}, false
	}
	return [9]float64{
		r0.X / det, r0.Y / det, r0.Z / det,
		r1.X / det, r1.Y / det, r1.Z / det,
		r2.X / det, r2.Y / det, r2.Z / det,
	}, true
}

// Grid returns the grid of a regular volume (uniform slice spacing, no gantry
// tilt correction); ok is false for stacks whose slices are not one affine
// grid.
func (v *Volume) Grid() (Grid, bool) {
	if v == nil || !v.regular || v.tilt.Applied {
		return Grid{}, false
	}
	g := Grid{
		Size:      [3]int{v.Cols, v.Rows, v.Depth},
		Origin:    v.Origin,
		Spacing:   [3]float64{v.ColSpacing, v.RowSpacing, v.effectiveSliceSpacing()},
		Direction: [3]Vec3{v.AxisX, v.AxisY, v.Normal},
	}
	if g.Validate() != nil {
		return Grid{}, false
	}
	return g, true
}

// Grid returns the grid of one slice plane, thickness mm thick (1 when not
// positive); ok is false for a plane without a usable geometry.
func (s SliceGeometry) Grid(thickness float64) (Grid, bool) {
	if !(thickness > 0) {
		thickness = 1
	}
	row, column := s.RowDir.Normalize(), s.ColDir.Normalize()
	g := Grid{
		Size:      [3]int{s.Columns, s.Rows, 1},
		Origin:    s.Origin,
		Spacing:   [3]float64{s.ColSpacing, s.RowSpacing, thickness},
		Direction: [3]Vec3{row, column, row.Cross(column).Normalize()},
	}
	return g, g.Validate() == nil
}
