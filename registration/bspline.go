package registration

import (
	"context"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/render"
)

// KindBSpline names ITK's BSplineTransform (cubic, three-dimensional).
const KindBSpline Kind = "BSplineTransform"

// bsplineOrder is the spline order; only cubic B-splines are implemented.
const bsplineOrder = 3

// bsplineSupport is the number of control points that move one point: four
// per axis.
const bsplineSupport = (bsplineOrder + 1) * (bsplineOrder + 1) * (bsplineOrder + 1)

// BSplineFixedParameters is the length of a BSpline's fixed parameters:
// ITK's grid size, grid origin, grid spacing and row-major grid direction.
const BSplineFixedParameters = 18

// BSpline is a cubic B-spline free-form deformation after ITK's
// BSplineTransform<double, 3, 3> (#1184): a fixed point x maps to
// x + Σ_k β3(i − k)·c_k, the sum over the 4×4×4 control points k around
// the continuous index i of x in the control-point grid, β3 the cubic
// B-spline kernel per axis and c_k the control point's displacement in
// millimetres. Each control point moves only the points within two grid
// spacings of it, so the Jacobian with respect to the parameters is sparse
// and analytic: the B-spline weights themselves.
//
// The layouts are ITK's. The parameters are the x displacements of every
// control point (x fastest, then y, then z), then the y and then the z
// displacements. The fixed parameters are the grid size (control points
// per axis, the mesh size plus three), the grid origin (the position of
// control point (0, 0, 0)), the grid spacing and the grid direction (the
// unit index axes as columns, written row by row). As in ITK, a point
// whose 4×4×4 support is not entirely inside the grid is left in place
// (zero displacement and zero Jacobian), and an index within four ULPs of
// the upper limit counts as inside.
//
// NewBSplineForDomain places the grid over a physical domain as ITK's
// transform domain does; NewBSplineForImage covers an image.
type BSpline struct {
	size      [3]int
	origin    render.Vec3
	spacing   render.Vec3
	direction Matrix3
	// physicalToIndex is (direction·diag(spacing))⁻¹.
	physicalToIndex Matrix3
	// coefficients holds the parameters: count x values, then y, then z.
	coefficients []float64
}

// NewBSpline returns a zero (identity) deformation on a control-point grid
// of size points per axis (at least four: a mesh of one interval) placed by
// origin, spacing and direction, ITK's fixed parameters.
func NewBSpline(size [3]int, origin, spacing render.Vec3, direction Matrix3) (*BSpline, error) {
	b := &BSpline{}
	if err := b.setGrid(size, origin, spacing, direction); err != nil {
		return nil, err
	}
	return b, nil
}

// NewBSplineForDomain places a grid of meshSize intervals per axis over a
// physical domain, as ITK's SetTransformDomain* methods do: the domain
// starts at origin, spans physicalDimensions millimetres along the columns
// of direction, the grid spacing is the dimension over the mesh size and
// the grid starts one spacing before the domain, so that every point of
// the domain has a full support.
func NewBSplineForDomain(origin, physicalDimensions render.Vec3, direction Matrix3, meshSize [3]int) (*BSpline, error) {
	var size [3]int
	var spacing [3]float64
	dims := [3]float64{physicalDimensions.X, physicalDimensions.Y, physicalDimensions.Z}
	for axis := range size {
		if meshSize[axis] < 1 {
			return nil, fmt.Errorf("%w: B-spline mesh size %v", ErrInvalidParameters, meshSize)
		}
		size[axis] = meshSize[axis] + bsplineOrder
		spacing[axis] = dims[axis] / float64(meshSize[axis])
	}
	shift := render.Vec3{X: spacing[0], Y: spacing[1], Z: spacing[2]}.Scale(-0.5 * (bsplineOrder - 1))
	gridOrigin := origin.Add(direction.MulVec(shift))
	return NewBSpline(size, gridOrigin, render.Vec3{X: spacing[0], Y: spacing[1], Z: spacing[2]}, direction)
}

// bsplineDomainMargin widens the domain of NewBSplineForImage beyond the
// image's outer sample edges by this fraction of a sample on every side,
// ITK's BSplineTransformInitializer epsilon.
const bsplineDomainMargin = 1.0 / 8

// NewBSplineForImage covers an image's extent (the outer edges of its
// samples, widened by an eighth of a sample) with a grid of meshSize
// intervals per axis, along the image's own axes. ITK's
// BSplineTransformInitializer covers the same box but reorders the axes to
// follow the patient axes; for an image with the identity direction both
// give the same grid.
func NewBSplineForImage(im *Image, meshSize [3]int) (*BSpline, error) {
	if im == nil {
		return nil, fmt.Errorf("%w: nil image", ErrImage)
	}
	start := -0.5 - bsplineDomainMargin
	origin := im.Point(render.Vec3{X: start, Y: start, Z: start})
	extent := func(axis int, spacing float64) float64 {
		return (float64(im.size[axis]) + 2*bsplineDomainMargin) * spacing
	}
	dims := render.Vec3{X: extent(0, im.spacing.X), Y: extent(1, im.spacing.Y), Z: extent(2, im.spacing.Z)}
	return NewBSplineForDomain(origin, dims, im.direction, meshSize)
}

func (b *BSpline) setGrid(size [3]int, origin, spacing render.Vec3, direction Matrix3) error {
	for _, n := range size {
		if n < bsplineOrder+1 {
			return fmt.Errorf("%w: B-spline grid size %v, at least %d per axis", ErrInvalidParameters, size, bsplineOrder+1)
		}
	}
	if count := size[0] * size[1] * size[2]; count/size[0]/size[1] != size[2] || count > 1<<26 {
		return fmt.Errorf("%w: B-spline grid size %v is too large", ErrInvalidParameters, size)
	}
	if !finiteVec(origin) || !direction.Finite() {
		return fmt.Errorf("%w: non-finite B-spline grid geometry", ErrInvalidParameters)
	}
	geometry, err := newImageGeometry([3]int{1, 1, 1}, origin, spacing, direction)
	if err != nil {
		return fmt.Errorf("%w: B-spline grid: %v", ErrInvalidParameters, err)
	}
	count := size[0] * size[1] * size[2]
	if b.size != size || len(b.coefficients) != 3*count {
		b.coefficients = make([]float64, 3*count)
	}
	b.size, b.origin, b.spacing, b.direction = size, origin, spacing, direction
	b.physicalToIndex = geometry.physicalToIndex
	return nil
}

// Kind returns KindBSpline.
func (b *BSpline) Kind() Kind { return KindBSpline }

// GridSize is the number of control points per axis.
func (b *BSpline) GridSize() [3]int { return b.size }

// GridOrigin is the position of control point (0, 0, 0).
func (b *BSpline) GridOrigin() render.Vec3 { return b.origin }

// GridSpacing is the control-point spacing in millimetres.
func (b *BSpline) GridSpacing() render.Vec3 { return b.spacing }

// GridDirection holds the grid's index axes as columns.
func (b *BSpline) GridDirection() Matrix3 { return b.direction }

// MeshSize is the number of grid intervals per axis, the grid size minus
// three.
func (b *BSpline) MeshSize() [3]int {
	return [3]int{b.size[0] - bsplineOrder, b.size[1] - bsplineOrder, b.size[2] - bsplineOrder}
}

func (b *BSpline) count() int { return b.size[0] * b.size[1] * b.size[2] }

// NumberOfParameters is three displacements per control point.
func (b *BSpline) NumberOfParameters() int { return len(b.coefficients) }

// Parameters returns a copy of the control-point displacements.
func (b *BSpline) Parameters() []float64 { return clone(b.coefficients) }

// SetParameters sets every control-point displacement.
func (b *BSpline) SetParameters(parameters []float64) error {
	if len(parameters) != len(b.coefficients) {
		return fmt.Errorf("%w: B-spline parameters have %d values, want %d", ErrInvalidParameters, len(parameters), len(b.coefficients))
	}
	if !finiteSlice(parameters) {
		return fmt.Errorf("%w: B-spline parameters contain a non-finite value", ErrInvalidParameters)
	}
	copy(b.coefficients, parameters)
	return nil
}

// FixedParameters returns the grid size, origin, spacing and row-major
// direction, ITK's eighteen fixed parameters.
func (b *BSpline) FixedParameters() []float64 {
	out := make([]float64, 0, BSplineFixedParameters)
	out = append(out, float64(b.size[0]), float64(b.size[1]), float64(b.size[2]))
	out = append(out, b.origin.X, b.origin.Y, b.origin.Z, b.spacing.X, b.spacing.Y, b.spacing.Z)
	for row := 0; row < 3; row++ {
		out = append(out, b.direction[row][0], b.direction[row][1], b.direction[row][2])
	}
	return out
}

// SetFixedParameters places the grid from ITK's eighteen fixed parameters.
// A change of grid size resets the displacements to zero, as ITK resizes
// its coefficient images.
func (b *BSpline) SetFixedParameters(fixed []float64) error {
	if err := checkLength(fixed, BSplineFixedParameters, "B-spline fixed parameters"); err != nil {
		return err
	}
	var size [3]int
	for axis := range size {
		v := fixed[axis]
		if v != math.Trunc(v) || v < bsplineOrder+1 || v > 1<<20 {
			return fmt.Errorf("%w: B-spline grid size %v", ErrInvalidParameters, fixed[:3])
		}
		size[axis] = int(v)
	}
	var direction Matrix3
	for row := 0; row < 3; row++ {
		direction[row] = [3]float64{fixed[9+3*row], fixed[10+3*row], fixed[11+3*row]}
	}
	return b.setGrid(size, vecFrom(fixed[3:6]), vecFrom(fixed[6:9]), direction)
}

// UpdateParameters adds factor·update to the displacements.
func (b *BSpline) UpdateParameters(update []float64, factor float64) error {
	if err := checkUpdate(update, len(b.coefficients), factor); err != nil {
		return err
	}
	for i, u := range update {
		b.coefficients[i] += factor * u
	}
	return nil
}

// Clone returns a deep copy.
func (b *BSpline) Clone() Transform {
	out := *b
	out.coefficients = clone(b.coefficients)
	return &out
}

// bsplineWeights is the support of one point: the first control point of
// the 4×4×4 block and the kernel values (and their derivatives along the
// index axis) per axis.
type bsplineWeights struct {
	start  [3]int
	value  [3][4]float64
	derive [3][4]float64
}

// support returns the weights at a point; false outside the valid region.
func (b *BSpline) support(point render.Vec3, derivatives bool) (bsplineWeights, bool) {
	var w bsplineWeights
	index := b.physicalToIndex.MulVec(point.Sub(b.origin))
	position := [3]float64{index.X, index.Y, index.Z}
	const low = 0.5 * (bsplineOrder - 1)
	for axis := 0; axis < 3; axis++ {
		v := position[axis]
		high := float64(b.size[axis]) - 0.5*(bsplineOrder-1) - 1
		switch {
		case almostEqualULPs(v, high, 4):
			v = nextAfterN(high, -6)
		case v >= high || v < low || math.IsNaN(v):
			return w, false
		}
		start := int(math.Floor(v + 0.5 - bsplineOrder/2.0))
		w.start[axis] = start
		u := v - float64(start)
		for k := 0; k < 4; k++ {
			w.value[axis][k] = cubicBSpline(u - float64(k))
			if derivatives {
				w.derive[axis][k] = cubicBSplineDerivative(u - float64(k))
			}
		}
	}
	return w, true
}

// Displacement is D(x), the displacement the control points give a point
// (zero outside the valid region).
func (b *BSpline) Displacement(point render.Vec3) render.Vec3 {
	w, ok := b.support(point, false)
	if !ok {
		return render.Vec3{}
	}
	return b.displacement(&w)
}

func (b *BSpline) displacement(w *bsplineWeights) render.Vec3 {
	count := b.count()
	var d [3]float64
	for k := 0; k < 4; k++ {
		for j := 0; j < 4; j++ {
			wkj := w.value[2][k] * w.value[1][j]
			row := ((w.start[2]+k)*b.size[1]+w.start[1]+j)*b.size[0] + w.start[0]
			for i := 0; i < 4; i++ {
				weight := wkj * w.value[0][i]
				at := row + i
				d[0] += weight * b.coefficients[at]
				d[1] += weight * b.coefficients[count+at]
				d[2] += weight * b.coefficients[2*count+at]
			}
		}
	}
	return render.Vec3{X: d[0], Y: d[1], Z: d[2]}
}

// TransformPoint returns x + D(x).
func (b *BSpline) TransformPoint(point render.Vec3) render.Vec3 {
	return point.Add(b.Displacement(point))
}

// JacobianWithRespectToParameters writes the dense 3 × parameters
// Jacobian: the B-spline weight of each support control point in the row
// of its displacement component. Metrics use the sparse form instead.
func (b *BSpline) JacobianWithRespectToParameters(point render.Vec3, out *Jacobian) {
	out.reset(len(b.coefficients))
	var sparse sparseJacobian
	b.sparseJacobianAt(point, &sparse)
	for k := 0; k < sparse.n; k++ {
		for d := 0; d < 3; d++ {
			out.set(d, d*sparse.stride+sparse.indices[k], sparse.weights[k])
		}
	}
}

// JacobianWithRespectToPosition returns I + ∂D/∂x, the analytic derivative
// of the spline (the identity outside the valid region).
func (b *BSpline) JacobianWithRespectToPosition(point render.Vec3) Matrix3 {
	out := Identity3()
	w, ok := b.support(point, true)
	if !ok {
		return out
	}
	count := b.count()
	// perIndex[d][a] = ∂D_d/∂(index a).
	var perIndex [3][3]float64
	for k := 0; k < 4; k++ {
		for j := 0; j < 4; j++ {
			row := ((w.start[2]+k)*b.size[1]+w.start[1]+j)*b.size[0] + w.start[0]
			for i := 0; i < 4; i++ {
				gradient := [3]float64{
					w.derive[0][i] * w.value[1][j] * w.value[2][k],
					w.value[0][i] * w.derive[1][j] * w.value[2][k],
					w.value[0][i] * w.value[1][j] * w.derive[2][k],
				}
				at := row + i
				for d := 0; d < 3; d++ {
					c := b.coefficients[d*count+at]
					if c == 0 {
						continue
					}
					for a := 0; a < 3; a++ {
						perIndex[d][a] += c * gradient[a]
					}
				}
			}
		}
	}
	for d := 0; d < 3; d++ {
		for a := 0; a < 3; a++ {
			out[d][a] += perIndex[d][0]*b.physicalToIndex[0][a] + perIndex[d][1]*b.physicalToIndex[1][a] + perIndex[d][2]*b.physicalToIndex[2][a]
		}
	}
	return out
}

// sparseJacobian is ∂T/∂p at one point for a transform whose parameters
// act locally: parameter indices[k] + d·stride moves the mapped point by
// weights[k] times column d of post. Every other parameter has no effect.
type sparseJacobian struct {
	n       int
	indices [bsplineSupport]int
	weights [bsplineSupport]float64
	stride  int
	post    Matrix3
}

// localSupportTransform is implemented by transforms whose parameter
// Jacobian is sparse; metrics then accumulate their derivatives without a
// dense Jacobian. localSupport reports whether the sparse form describes
// every parameter (a composite only when its one optimized component has
// local support).
type localSupportTransform interface {
	localSupport() bool
	sparseJacobianAt(point render.Vec3, out *sparseJacobian)
}

func (b *BSpline) localSupport() bool { return true }

func (b *BSpline) sparseJacobianAt(point render.Vec3, out *sparseJacobian) {
	out.n, out.stride, out.post = 0, b.count(), Identity3()
	w, ok := b.support(point, false)
	if !ok {
		return
	}
	n := 0
	for k := 0; k < 4; k++ {
		for j := 0; j < 4; j++ {
			wkj := w.value[2][k] * w.value[1][j]
			row := ((w.start[2]+k)*b.size[1]+w.start[1]+j)*b.size[0] + w.start[0]
			for i := 0; i < 4; i++ {
				out.indices[n] = row + i
				out.weights[n] = wkj * w.value[0][i]
				n++
			}
		}
	}
	out.n = n
}

// localSupport is true when exactly one component is optimized and it has
// local support (for example a B-spline followed by a frozen affine).
func (c *Composite) localSupport() bool {
	optimized := -1
	for i := range c.transforms {
		if c.optimize[i] {
			if optimized >= 0 {
				return false
			}
			optimized = i
		}
	}
	if optimized < 0 {
		return false
	}
	local, ok := c.transforms[optimized].(localSupportTransform)
	return ok && local.localSupport()
}

// sparseJacobianAt evaluates the optimized component at its own input point
// and left-multiplies by the position Jacobians of the components applied
// after it. It is only valid when localSupport is true.
func (c *Composite) sparseJacobianAt(point render.Vec3, out *sparseJacobian) {
	out.n = 0
	found := false
	for i, t := range c.transforms {
		if !found && c.optimize[i] {
			t.(localSupportTransform).sparseJacobianAt(point, out)
			found = true
		} else if found && out.n > 0 {
			out.post = t.JacobianWithRespectToPosition(point).Mul(out.post)
		}
		point = t.TransformPoint(point)
	}
}

// Refine returns the same deformation on a grid with twice the mesh
// intervals along the chosen axes, over the same domain: the exact cubic
// B-spline subdivision (each coarse kernel is the sum of five fine ones
// weighted 1, 4, 6, 4, 1 over 8), so points map as before up to rounding.
// Multi-resolution free-form deformation refines the grid between levels,
// the part of ITK's BSplineTransformParametersAdaptor used for mesh
// doubling.
func (b *BSpline) Refine(axes [3]bool) (*BSpline, error) {
	size, spacing, origin := b.size, b.spacing, b.origin
	sp := [3]float64{b.spacing.X, b.spacing.Y, b.spacing.Z}
	for axis := 0; axis < 3; axis++ {
		if !axes[axis] {
			continue
		}
		size[axis] = 2*(b.size[axis]-bsplineOrder) + bsplineOrder
		// The grid starts one spacing before the domain; the domain stays.
		var shift [3]float64
		shift[axis] = sp[axis] / 2
		origin = origin.Add(b.direction.MulVec(render.Vec3{X: shift[0], Y: shift[1], Z: shift[2]}))
		sp[axis] /= 2
	}
	spacing = render.Vec3{X: sp[0], Y: sp[1], Z: sp[2]}
	out, err := NewBSpline(size, origin, spacing, b.direction)
	if err != nil {
		return nil, err
	}
	for d := 0; d < 3; d++ {
		values := b.coefficients[d*b.count() : (d+1)*b.count()]
		current, currentSize := values, b.size
		for axis := 0; axis < 3; axis++ {
			if axes[axis] {
				current, currentSize = refineAxis(current, currentSize, axis)
			}
		}
		copy(out.coefficients[d*out.count():(d+1)*out.count()], current)
	}
	return out, nil
}

// subdivision is the cubic B-spline two-scale mask: β3(t) = Σ_r a_r·β3(2t − r)
// for r = −2…2.
var subdivision = [5]float64{1.0 / 8, 4.0 / 8, 6.0 / 8, 4.0 / 8, 1.0 / 8}

// refineAxis doubles the mesh of one axis: coarse point j sits where fine
// point 2j − 1 does, so fine coefficient k gathers a_(k−2j+1)·c_j.
func refineAxis(values []float64, size [3]int, axis int) ([]float64, [3]int) {
	fine := size
	fine[axis] = 2*(size[axis]-bsplineOrder) + bsplineOrder
	out := make([]float64, fine[0]*fine[1]*fine[2])
	strides := [3]int{1, size[0], size[0] * size[1]}
	fineStrides := [3]int{1, fine[0], fine[0] * fine[1]}
	for z := 0; z < fine[2]; z++ {
		for y := 0; y < fine[1]; y++ {
			for x := 0; x < fine[0]; x++ {
				at := [3]int{x, y, z}
				k := at[axis]
				var sum float64
				for j := (k - 1) / 2; j <= (k+3)/2; j++ {
					r := k - 2*j + 1
					if j < 0 || j >= size[axis] || r < -2 || r > 2 {
						continue
					}
					coarse := at
					coarse[axis] = j
					sum += subdivision[r+2] * values[coarse[0]*strides[0]+coarse[1]*strides[1]+coarse[2]*strides[2]]
				}
				out[x*fineStrides[0]+y*fineStrides[1]+z*fineStrides[2]] = sum
			}
		}
	}
	return out, fine
}

// TransformDisplacementField samples T(p) − p at every point p of grid: a
// dense field (Demons' type) for FieldMapping, Resample or a Deformable
// Spatial Registration grid (DisplacementField.Grid). It fails where T
// gives a non-finite point.
func TransformDisplacementField(ctx context.Context, t Transform, grid Grid) (*DisplacementField, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if t == nil {
		return nil, fmt.Errorf("%w: nil transform", ErrUnsupportedTransform)
	}
	geometry, err := newImageGeometry(grid.Size, grid.Origin, grid.Spacing, grid.Direction)
	if err != nil {
		return nil, err
	}
	field := &DisplacementField{Size: grid.Size, Origin: grid.Origin, Spacing: grid.Spacing, Direction: grid.Direction}
	if count := grid.Size[0] * grid.Size[1] * grid.Size[2]; count/grid.Size[0]/grid.Size[1] != grid.Size[2] {
		return nil, fmt.Errorf("%w: grid size %v", ErrImage, grid.Size)
	}
	field.Vectors = make([]render.Vec3, grid.Size[0]*grid.Size[1]*grid.Size[2])
	bad := make([]bool, grid.Size[2])
	parallelSlices(grid.Size[2], func(z int) {
		if ctx.Err() != nil {
			return
		}
		for y := 0; y < grid.Size[1]; y++ {
			if ctx.Err() != nil {
				return
			}
			for x := 0; x < grid.Size[0]; x++ {
				p := geometry.Point(render.Vec3{X: float64(x), Y: float64(y), Z: float64(z)})
				q := t.TransformPoint(p)
				if !finiteVec(q) {
					bad[z] = true
				}
				field.Vectors[(z*grid.Size[1]+y)*grid.Size[0]+x] = q.Sub(p)
			}
		}
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for z, b := range bad {
		if b {
			return nil, fmt.Errorf("%w: the transform is not finite on slice %d", ErrInvalidParameters, z)
		}
	}
	return field, nil
}

// almostEqualULPs reports whether a and b are within ulps units in the last
// place, as ITK's Math::FloatAlmostEqual does for doubles.
func almostEqualULPs(a, b float64, ulps int64) bool {
	if a == b {
		return true
	}
	if math.IsNaN(a) || math.IsNaN(b) || math.Signbit(a) != math.Signbit(b) {
		return math.Abs(a-b) <= 0.1*epsilon
	}
	ia, ib := int64(math.Float64bits(a)), int64(math.Float64bits(b))
	diff := ia - ib
	if diff < 0 {
		diff = -diff
	}
	return diff <= ulps
}

// nextAfterN moves v by n units in the last place (negative: down).
func nextAfterN(v float64, n int) float64 {
	target := math.Inf(1)
	if n < 0 {
		target, n = math.Inf(-1), -n
	}
	for ; n > 0; n-- {
		v = math.Nextafter(v, target)
	}
	return v
}
