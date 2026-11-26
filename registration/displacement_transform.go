package registration

import (
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/render"
)

// KindDisplacementField names ITK's DisplacementFieldTransform.
const KindDisplacementField Kind = "DisplacementFieldTransform"

// DisplacementFieldFixedParameters is the length of a displacement field
// transform's fixed parameters: ITK's size, origin, spacing and row-major
// direction of the field.
const DisplacementFieldFixedParameters = 18

// DisplacementFieldTransform is a dense displacement field as a Transform,
// after ITK's DisplacementFieldTransform<double, 3> (#1252), the
// non-linear stage ANTs writes into composite transform files: a point x
// maps to x + f(x), f interpolated linearly in the field (the outer
// vectors extend half a sample), and points more than half a sample
// outside the field's grid stay in place, as in ITK. (FieldMapping, by
// contrast, leaves such points undefined.)
//
// The parameters are the field's vectors, interleaved (x, y, z of the
// first sample, then of the next, x fastest), and the fixed parameters
// the field's size, origin, spacing and row-major direction, ITK's
// layouts. The Jacobian with respect to the parameters is the trilinear
// weights; the one with respect to position is the analytic derivative of
// the interpolation. ITK's optional inverse field is not modelled.
type DisplacementFieldTransform struct {
	field DisplacementField
	grid  Image
}

// NewDisplacementFieldTransform wraps field, sharing its vectors.
func NewDisplacementFieldTransform(field *DisplacementField) (*DisplacementFieldTransform, error) {
	if field == nil {
		return nil, fmt.Errorf("%w: nil displacement field", ErrInvalidParameters)
	}
	t := &DisplacementFieldTransform{}
	if err := t.setGeometry(field.Size, field.Origin, field.Spacing, field.Direction); err != nil {
		return nil, err
	}
	if len(field.Vectors) != len(t.field.Vectors) {
		return nil, fmt.Errorf("%w: %d vectors for size %v", ErrInvalidParameters, len(field.Vectors), field.Size)
	}
	for _, v := range field.Vectors {
		if !finiteVec(v) {
			return nil, fmt.Errorf("%w: non-finite displacement", ErrInvalidParameters)
		}
	}
	t.field.Vectors = field.Vectors
	return t, nil
}

func (t *DisplacementFieldTransform) setGeometry(size [3]int, origin, spacing render.Vec3, direction Matrix3) error {
	for _, n := range size {
		if n < 1 || n > 1<<20 {
			return fmt.Errorf("%w: displacement field size %v", ErrInvalidParameters, size)
		}
	}
	count := size[0] * size[1] * size[2]
	if count/size[0]/size[1] != size[2] || count > 1<<26 {
		return fmt.Errorf("%w: displacement field size %v is too large", ErrInvalidParameters, size)
	}
	if !finiteVec(origin) || !direction.Finite() {
		return fmt.Errorf("%w: non-finite displacement field geometry", ErrInvalidParameters)
	}
	grid, err := newImageGeometry(size, origin, spacing, direction)
	if err != nil {
		return fmt.Errorf("%w: displacement field: %v", ErrInvalidParameters, err)
	}
	vectors := t.field.Vectors
	if len(vectors) != count {
		vectors = make([]render.Vec3, count)
	}
	t.field = DisplacementField{Size: size, Origin: origin, Spacing: spacing, Direction: direction, Vectors: vectors}
	t.grid = grid
	return nil
}

// Kind returns KindDisplacementField.
func (t *DisplacementFieldTransform) Kind() Kind { return KindDisplacementField }

// Field returns the field, sharing its vectors.
func (t *DisplacementFieldTransform) Field() *DisplacementField {
	f := t.field
	return &f
}

// TransformPoint returns x + f(x), or x outside the field.
func (t *DisplacementFieldTransform) TransformPoint(point render.Vec3) render.Vec3 {
	index := t.grid.ContinuousIndex(point)
	if !t.grid.inside(index) {
		return point
	}
	return point.Add(sampleVectors(t.field.Vectors, t.field.Size, index))
}

// NumberOfParameters is three per field sample.
func (t *DisplacementFieldTransform) NumberOfParameters() int { return 3 * len(t.field.Vectors) }

// Parameters returns the interleaved vectors.
func (t *DisplacementFieldTransform) Parameters() []float64 {
	out := make([]float64, 0, 3*len(t.field.Vectors))
	for _, v := range t.field.Vectors {
		out = append(out, v.X, v.Y, v.Z)
	}
	return out
}

// SetParameters replaces the vectors (the field is not shared afterwards).
func (t *DisplacementFieldTransform) SetParameters(parameters []float64) error {
	if len(parameters) != 3*len(t.field.Vectors) {
		return fmt.Errorf("%w: displacement field parameters have %d values, want %d", ErrInvalidParameters, len(parameters), 3*len(t.field.Vectors))
	}
	if !finiteSlice(parameters) {
		return fmt.Errorf("%w: displacement field parameters contain a non-finite value", ErrInvalidParameters)
	}
	vectors := make([]render.Vec3, len(t.field.Vectors))
	for i := range vectors {
		vectors[i] = render.Vec3{X: parameters[3*i], Y: parameters[3*i+1], Z: parameters[3*i+2]}
	}
	t.field.Vectors = vectors
	return nil
}

// FixedParameters returns the field's size, origin, spacing and row-major
// direction.
func (t *DisplacementFieldTransform) FixedParameters() []float64 {
	f := t.field
	out := []float64{float64(f.Size[0]), float64(f.Size[1]), float64(f.Size[2]), f.Origin.X, f.Origin.Y, f.Origin.Z, f.Spacing.X, f.Spacing.Y, f.Spacing.Z}
	for row := 0; row < 3; row++ {
		out = append(out, f.Direction[row][0], f.Direction[row][1], f.Direction[row][2])
	}
	return out
}

// SetFixedParameters places the field; a change of size resets the vectors
// to zero, as ITK allocates a new field.
func (t *DisplacementFieldTransform) SetFixedParameters(fixed []float64) error {
	if err := checkLength(fixed, DisplacementFieldFixedParameters, "displacement field fixed parameters"); err != nil {
		return err
	}
	var size [3]int
	for axis := range size {
		v := fixed[axis]
		if v != math.Trunc(v) || v < 1 || v > 1<<20 {
			return fmt.Errorf("%w: displacement field size %v", ErrInvalidParameters, fixed[:3])
		}
		size[axis] = int(v)
	}
	var direction Matrix3
	for row := 0; row < 3; row++ {
		direction[row] = [3]float64{fixed[9+3*row], fixed[10+3*row], fixed[11+3*row]}
	}
	if size != t.field.Size {
		t.field.Vectors = nil
	}
	return t.setGeometry(size, vecFrom(fixed[3:6]), vecFrom(fixed[6:9]), direction)
}

// UpdateParameters adds factor·update to the vectors.
func (t *DisplacementFieldTransform) UpdateParameters(update []float64, factor float64) error {
	if err := checkUpdate(update, t.NumberOfParameters(), factor); err != nil {
		return err
	}
	parameters := t.Parameters()
	for i, u := range update {
		parameters[i] += factor * u
	}
	return t.SetParameters(parameters)
}

// Clone returns a deep copy.
func (t *DisplacementFieldTransform) Clone() Transform {
	out := *t
	out.field.Vectors = append([]render.Vec3(nil), t.field.Vectors...)
	return &out
}

// trilinear returns the (up to) eight samples around a continuous index,
// their weights and the derivatives of the weights along each index axis,
// with the clamping of sampleVectors.
func (t *DisplacementFieldTransform) trilinear(index render.Vec3) (at [8]int, weight [8]float64, slope [8][3]float64) {
	size := t.field.Size
	var base [3]int
	var frac [3]float64
	var moves [3]bool
	for axis, value := range [3]float64{index.X, index.Y, index.Z} {
		clamped := math.Min(math.Max(value, 0), float64(size[axis]-1))
		floor := math.Floor(clamped)
		base[axis], frac[axis] = int(floor), clamped-floor
		moves[axis] = value > 0 && value < float64(size[axis]-1)
		if base[axis] >= size[axis]-1 {
			base[axis], frac[axis] = size[axis]-1, 0
		}
	}
	for corner := 0; corner < 8; corner++ {
		c := base
		w := 1.0
		var factors [3]float64
		var derivatives [3]float64
		for axis := 0; axis < 3; axis++ {
			high := corner>>axis&1 == 1
			if high {
				c[axis] = min(c[axis]+1, size[axis]-1)
				factors[axis], derivatives[axis] = frac[axis], 1
			} else {
				factors[axis], derivatives[axis] = 1-frac[axis], -1
			}
			if !moves[axis] {
				derivatives[axis] = 0
			}
			w *= factors[axis]
		}
		at[corner] = (c[2]*size[1]+c[1])*size[0] + c[0]
		weight[corner] = w
		for axis := 0; axis < 3; axis++ {
			s := derivatives[axis]
			for other := 0; other < 3; other++ {
				if other != axis {
					s *= factors[other]
				}
			}
			slope[corner][axis] = s
		}
	}
	return at, weight, slope
}

// JacobianWithRespectToParameters writes the trilinear weight of each of
// the eight surrounding samples into the rows of its vector components
// (zero outside the field).
func (t *DisplacementFieldTransform) JacobianWithRespectToParameters(point render.Vec3, out *Jacobian) {
	out.reset(t.NumberOfParameters())
	index := t.grid.ContinuousIndex(point)
	if !t.grid.inside(index) {
		return
	}
	at, weight, _ := t.trilinear(index)
	for corner := 0; corner < 8; corner++ {
		for d := 0; d < 3; d++ {
			out.Values[d*out.Columns+3*at[corner]+d] += weight[corner]
		}
	}
}

// JacobianWithRespectToPosition returns I + ∂f/∂x of the linear
// interpolation (the identity outside the field and along axes where the
// interpolation is clamped).
func (t *DisplacementFieldTransform) JacobianWithRespectToPosition(point render.Vec3) Matrix3 {
	out := Identity3()
	index := t.grid.ContinuousIndex(point)
	if !t.grid.inside(index) {
		return out
	}
	at, _, slope := t.trilinear(index)
	var perIndex [3][3]float64
	for corner := 0; corner < 8; corner++ {
		v := t.field.Vectors[at[corner]]
		for a := 0; a < 3; a++ {
			perIndex[0][a] += slope[corner][a] * v.X
			perIndex[1][a] += slope[corner][a] * v.Y
			perIndex[2][a] += slope[corner][a] * v.Z
		}
	}
	p := t.grid.physicalToIndex
	for d := 0; d < 3; d++ {
		for a := 0; a < 3; a++ {
			out[d][a] += perIndex[d][0]*p[0][a] + perIndex[d][1]*p[1][a] + perIndex[d][2]*p[2][a]
		}
	}
	return out
}
