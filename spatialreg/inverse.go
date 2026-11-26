package spatialreg

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/render"
)

// ErrInverseNotConverged reports a deformable registration whose inverse
// could not be found within the residual tolerance at a point.
var ErrInverseNotConverged = errors.New("dicom/spatialreg: inverse deformation did not converge")

// ErrFolding reports a deformation that folds space (a Jacobian
// determinant of zero or of the opposite sign to the pre-matrix's), where
// no unique inverse exists.
var ErrFolding = errors.New("dicom/spatialreg: deformation folds")

// Inverse defaults, from ITK's InvertDisplacementFieldImageFilter test: 50
// iterations and a largest residual of 0.1 mm.
const (
	DefaultInverseIterations  = 50
	DefaultInverseMaxResidual = 0.1
	// DefaultInverseTolerance ends the iteration early.
	DefaultInverseTolerance = 1e-4
)

// InverseOptions bound the inversion of a deformable registration.
type InverseOptions struct {
	// MaximumIterations bounds the fixed-point iterations
	// (DefaultInverseIterations when zero).
	MaximumIterations int
	// Tolerance, in source millimetres, stops the iteration once reached
	// (DefaultInverseTolerance when zero).
	Tolerance float64
	// MaxResidual is the largest accepted residual in source millimetres
	// (DefaultInverseMaxResidual when zero): above it the inverse fails.
	MaxResidual float64
}

func (o InverseOptions) normalized() InverseOptions {
	if o.MaximumIterations <= 0 {
		o.MaximumIterations = DefaultInverseIterations
	}
	if !(o.Tolerance > 0) {
		o.Tolerance = DefaultInverseTolerance
	}
	if !(o.MaxResidual > 0) {
		o.MaxResidual = DefaultInverseMaxResidual
	}
	return o
}

// InverseResult describes one inverted point.
type InverseResult struct {
	Registered render.Vec3
	// Residual is |forward(Registered) − source| in millimetres.
	Residual   float64
	Iterations int
	// JacobianDeterminant is det(Pre + ∇D) at Registered divided by the
	// pre-matrix determinant: the local volume change of the deformation.
	JacobianDeterminant float64
}

// MapSourceToRegistered inverts MapRegisteredToSource at a point with the
// default options.
func (r *Registration) MapSourceToRegistered(point render.Vec3) (render.Vec3, error) {
	result, err := r.InvertPoint(point, InverseOptions{})
	return result.Registered, err
}

// InvertPoint finds the registered point x that MapRegisteredToSource maps
// onto source, s = Post(Pre(x) + D(x)), by fixed-point iteration as ITK's
// InvertDisplacementFieldImageFilter does for a whole field: with
// y = Post⁻¹(s) and Pre(x) = A·x + b, x ← x + ε·A⁻¹(y − A·x − b − D(x)),
// ε halved while a step does not reduce the residual. It fails closed:
// ErrInverseNotConverged when the residual stays above MaxResidual,
// ErrFolding when the deformation folds at the solution, and the grid's
// domain errors when the iteration leaves the deformation grid.
func (r *Registration) InvertPoint(source render.Vec3, opts InverseOptions) (InverseResult, error) {
	opts = opts.normalized()
	if r == nil || !finiteVec(source) {
		return InverseResult{}, fmt.Errorf("%w: non-finite source point", ErrInvalidObject)
	}
	pre, post := r.Pre.Values, r.Post.Values
	if !pre.Finite() || !post.Finite() {
		return InverseResult{}, fmt.Errorf("%w: pre or post matrix is invalid", ErrInvalidObject)
	}
	postInverse, _, err := inverseAffine(post)
	if err != nil {
		return InverseResult{}, err
	}
	preInverse, _, err := inverseAffine(pre)
	if err != nil {
		return InverseResult{}, err
	}
	target := postInverse.TransformPoint(source)
	// deformed is Pre(x) + D(x), the point before the post-matrix.
	deformed := func(x render.Vec3) (render.Vec3, error) {
		out := pre.TransformPoint(x)
		if r.Grid == nil {
			return out, nil
		}
		displacement, err := r.Grid.Sample(x)
		if err != nil {
			return render.Vec3{}, err
		}
		return out.Add(displacement), nil
	}
	residualOf := func(y render.Vec3) float64 {
		return post.TransformPoint(y).Sub(source).Length()
	}
	x := preInverse.TransformPoint(target)
	y, err := deformed(x)
	if err != nil {
		return InverseResult{}, err
	}
	residual := residualOf(y)
	step := 1.0
	iterations := 0
	for iterations < opts.MaximumIterations && residual > opts.Tolerance {
		iterations++
		// A⁻¹ applied to a vector is the linear part of Pre⁻¹.
		update := linearPart(preInverse).MulVec(target.Sub(y))
		next := x.Add(update.Scale(step))
		nextY, err := deformed(next)
		if err != nil {
			if step > 1.0/64 {
				step /= 2
				continue
			}
			return InverseResult{}, err
		}
		nextResidual := residualOf(nextY)
		if nextResidual >= residual {
			if step > 1.0/64 {
				step /= 2
				continue
			}
			break
		}
		x, y, residual = next, nextY, nextResidual
		step = math.Min(1, step*2)
	}
	result := InverseResult{Registered: x, Residual: residual, Iterations: iterations}
	// A fold stalls the iteration (it moves away from a solution where the
	// map reverses orientation), so it is checked first.
	determinant, err := r.jacobianDeterminant(x)
	if err != nil {
		return result, err
	}
	result.JacobianDeterminant = determinant
	if !(determinant > 0) {
		return result, fmt.Errorf("%w: Jacobian determinant %.3g at the inverse", ErrFolding, determinant)
	}
	if residual > opts.MaxResidual {
		return result, fmt.Errorf("%w: residual %.3g mm after %d iterations", ErrInverseNotConverged, residual, iterations)
	}
	return result, nil
}

type inverseConstants struct {
	pre, post, preInverse, postInverse render.GeometryAffine
	preInverseLinear, preLinear        matrix3
	preDeterminant                     float64
}

func prepareInverseConstants(r *Registration) (inverseConstants, error) {
	pre, post := r.Pre.Values, r.Post.Values
	if !pre.Finite() || !post.Finite() {
		return inverseConstants{}, fmt.Errorf("%w: pre or post matrix is invalid", ErrInvalidObject)
	}
	postInverse, _, err := inverseAffine(post)
	if err != nil {
		return inverseConstants{}, err
	}
	preInverse, _, err := inverseAffine(pre)
	if err != nil {
		return inverseConstants{}, err
	}
	preLinear := linearPart(pre)
	return inverseConstants{pre: pre, post: post, preInverse: preInverse, postInverse: postInverse,
		preInverseLinear: linearPart(preInverse), preLinear: preLinear, preDeterminant: preLinear.Determinant()}, nil
}

// Prepared snapshots use cached constants and context-aware iteration. The
// public non-context point API retains its original direct implementation.
// Exact-oracle tests enforce the same iteration and diagnostic mathematics.
func (r *Registration) invertWithConstants(ctx context.Context, source render.Vec3, opts InverseOptions, c inverseConstants) (InverseResult, error) {
	target := c.postInverse.TransformPoint(source)
	// deformed is Pre(x) + D(x), the point before the post-matrix.
	deformed := func(x render.Vec3) (render.Vec3, error) {
		out := c.pre.TransformPoint(x)
		if r.Grid == nil {
			return out, nil
		}
		displacement, err := r.Grid.Sample(x)
		if err != nil {
			return render.Vec3{}, err
		}
		return out.Add(displacement), nil
	}
	residualOf := func(y render.Vec3) float64 {
		return c.post.TransformPoint(y).Sub(source).Length()
	}
	x := c.preInverse.TransformPoint(target)
	y, err := deformed(x)
	if err != nil {
		return InverseResult{}, err
	}
	residual := residualOf(y)
	step := 1.0
	iterations := 0
	for iterations < opts.MaximumIterations && residual > opts.Tolerance {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return InverseResult{}, err
			}
		}
		iterations++
		// A⁻¹ applied to a vector is the linear part of Pre⁻¹.
		update := c.preInverseLinear.MulVec(target.Sub(y))
		next := x.Add(update.Scale(step))
		nextY, err := deformed(next)
		if err != nil {
			if step > 1.0/64 {
				step /= 2
				continue
			}
			return InverseResult{}, err
		}
		nextResidual := residualOf(nextY)
		if nextResidual >= residual {
			if step > 1.0/64 {
				step /= 2
				continue
			}
			break
		}
		x, y, residual = next, nextY, nextResidual
		step = math.Min(1, step*2)
	}
	result := InverseResult{Registered: x, Residual: residual, Iterations: iterations}
	// A fold stalls the iteration (it moves away from a solution where the
	// map reverses orientation), so it is checked first.
	determinant, err := r.inverseJacobian(x, c.preLinear, c.preDeterminant)
	if err != nil {
		return result, err
	}
	result.JacobianDeterminant = determinant
	if !(determinant > 0) {
		return result, fmt.Errorf("%w: Jacobian determinant %.3g at the inverse", ErrFolding, determinant)
	}
	if residual > opts.MaxResidual {
		return result, fmt.Errorf("%w: residual %.3g mm after %d iterations", ErrInverseNotConverged, residual, iterations)
	}
	return result, nil
}

// jacobianDeterminant is det(A + ∇D(x)) / det(A), with ∇D the gradient of
// the trilinear interpolant in the cell holding x.
func (r *Registration) jacobianDeterminant(x render.Vec3) (float64, error) {
	a := linearPart(r.Pre.Values)
	base := a.Determinant()
	if base == 0 || !finite(base) {
		return 0, fmt.Errorf("%w: singular pre-matrix", ErrInvalidObject)
	}
	if r.Grid == nil {
		return 1, nil
	}
	gradient, err := r.Grid.gradient(x)
	if err != nil {
		return 0, err
	}
	var sum matrix3
	for i := range sum {
		for j := range sum[i] {
			sum[i][j] = a[i][j] + gradient[i][j]
		}
	}
	return sum.Determinant() / base, nil
}

func (r *Registration) inverseJacobian(x render.Vec3, a matrix3, base float64) (float64, error) {
	if base == 0 || !finite(base) {
		return 0, fmt.Errorf("%w: singular pre-matrix", ErrInvalidObject)
	}
	if r.Grid == nil {
		return 1, nil
	}
	gradient, err := r.Grid.gradient(x)
	if err != nil {
		return 0, err
	}
	var sum matrix3
	for i := range sum {
		for j := range sum[i] {
			sum[i][j] = a[i][j] + gradient[i][j]
		}
	}
	return sum.Determinant() / base, nil
}

// matrix3 is a row-major 3 × 3 matrix.
type matrix3 [3][3]float64

func linearPart(m render.GeometryAffine) matrix3 {
	return matrix3{{m[0], m[1], m[2]}, {m[4], m[5], m[6]}, {m[8], m[9], m[10]}}
}

func (m matrix3) MulVec(v render.Vec3) render.Vec3 {
	return render.Vec3{
		X: m[0][0]*v.X + m[0][1]*v.Y + m[0][2]*v.Z,
		Y: m[1][0]*v.X + m[1][1]*v.Y + m[1][2]*v.Z,
		Z: m[2][0]*v.X + m[2][1]*v.Y + m[2][2]*v.Z,
	}
}

func (m matrix3) Determinant() float64 {
	return m[0][0]*(m[1][1]*m[2][2]-m[1][2]*m[2][1]) -
		m[0][1]*(m[1][0]*m[2][2]-m[1][2]*m[2][0]) +
		m[0][2]*(m[1][0]*m[2][1]-m[1][1]*m[2][0])
}

// gradient is ∂D_i/∂x_j of the trilinear interpolant at a registered point:
// the derivative along each grid axis within the cell holding the point,
// turned into patient axes by the grid's directions and spacing.
func (g *Grid) gradient(point render.Vec3) (matrix3, error) {
	if _, err := g.Sample(point); err != nil {
		return matrix3{}, err
	}
	delta := point.Sub(g.Origin)
	directions := [3]render.Vec3{g.RowDirection, g.ColumnDirection, g.NormalDirection}
	spacing := [3]float64{g.Spacing.X, g.Spacing.Y, g.Spacing.Z}
	var lower [3]int
	var fraction [3]float64
	for axis := range directions {
		maximum := float64(g.Dimensions[axis] - 1)
		coordinate := math.Max(0, math.Min(maximum, delta.Dot(directions[axis])/spacing[axis]))
		lower[axis] = int(math.Floor(coordinate))
		if lower[axis] >= int(g.Dimensions[axis])-1 {
			lower[axis] = max(0, int(g.Dimensions[axis])-2)
		}
		fraction[axis] = coordinate - float64(lower[axis])
	}
	var out matrix3
	for axis := range directions {
		if g.Dimensions[axis] < 2 {
			continue // a single plane has no derivative across it
		}
		// d/du_axis of the trilinear interpolant: the difference of the two
		// faces of the cell, interpolated over the other two axes.
		var derivative render.Vec3
		for corner := 0; corner < 4; corner++ {
			var index [3]int
			weight := 1.0
			bit := 0
			for other := 0; other < 3; other++ {
				if other == axis {
					continue
				}
				f := fraction[other]
				index[other] = lower[other]
				if corner>>bit&1 == 1 {
					if lower[other]+1 >= int(g.Dimensions[other]) {
						weight = 0
					}
					index[other]++
					weight *= f
				} else {
					weight *= 1 - f
				}
				bit++
			}
			if weight == 0 {
				continue
			}
			index[axis] = lower[axis]
			low, ok := g.VectorAt(index[0], index[1], index[2])
			index[axis]++
			high, ok2 := g.VectorAt(index[0], index[1], index[2])
			if !ok || !ok2 {
				return matrix3{}, fmt.Errorf("%w: vector near (%d,%d,%d)", ErrUndefinedDomain, index[0], index[1], index[2])
			}
			derivative = derivative.Add(high.Sub(low).Scale(weight))
		}
		// ∂u_axis/∂x = direction/spacing.
		d := directions[axis].Scale(1 / spacing[axis])
		for i, component := range [3]float64{derivative.X, derivative.Y, derivative.Z} {
			out[i][0] += component * d.X
			out[i][1] += component * d.Y
			out[i][2] += component * d.Z
		}
	}
	return out, nil
}
