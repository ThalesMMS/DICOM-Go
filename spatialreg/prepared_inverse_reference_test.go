package spatialreg

// Frozen original inversion and determinant from admission c6f51dd, namespaced
// test-only. Iteration arithmetic/error precedence is not rewritten to follow
// the candidate. matrix3/grid.gradient/inverseAffine dependencies are unchanged.
import (
	"fmt"
	"github.com/ThalesMMS/dicom-go/render"
	"math"
)

func (r *Registration) invert1531Original(source render.Vec3, opts InverseOptions) (InverseResult, error) {
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
	determinant, err := r.jacobian1531Original(x)
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

// jacobian1531Original is det(A + ∇D(x)) / det(A), with ∇D the gradient of
// the trilinear interpolant in the cell holding x.
func (r *Registration) jacobian1531Original(x render.Vec3) (float64, error) {
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
