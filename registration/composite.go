package registration

import (
	"fmt"

	"github.com/ThalesMMS/dicom-go/render"
)

// Composite chains transforms, after ITK's CompositeTransform. Transforms
// are listed in application order: the first one is applied to the input
// point first, so Composite{A, B}.TransformPoint(x) = B(A(x)). (ITK's queue
// is the reverse: its last added transform is applied first. The parameter
// order below equals ITK's.)
//
// Only transforms flagged for optimization (all, by default) contribute to
// Parameters, UpdateParameters and the parameter Jacobian, concatenated in
// application order; the others stay fixed, for example an initial rigid
// alignment under an affine stage. Fixed parameters concatenate every
// transform's fixed parameters in application order.
type Composite struct {
	transforms []Transform
	optimize   []bool
}

// NewComposite returns a composite applying transforms in the given order,
// all flagged for optimization. The composite owns the transforms: mutating
// one afterwards changes the composite.
func NewComposite(transforms ...Transform) (*Composite, error) {
	c := &Composite{}
	for _, t := range transforms {
		if err := c.Append(t); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// Append adds t after the current transforms, flagged for optimization.
func (c *Composite) Append(t Transform) error {
	if t == nil {
		return fmt.Errorf("%w: nil transform in composite", ErrUnsupportedTransform)
	}
	c.transforms = append(c.transforms, t)
	c.optimize = append(c.optimize, true)
	return nil
}

// Kind returns KindComposite.
func (c *Composite) Kind() Kind { return KindComposite }

// Len returns the number of transforms.
func (c *Composite) Len() int { return len(c.transforms) }

// Transform returns the index-th transform in application order.
func (c *Composite) Transform(index int) Transform { return c.transforms[index] }

// Optimized reports whether the index-th transform is optimized.
func (c *Composite) Optimized(index int) bool { return c.optimize[index] }

// SetOptimized flags the index-th transform for optimization or freezes it.
func (c *Composite) SetOptimized(index int, on bool) error {
	if index < 0 || index >= len(c.transforms) {
		return fmt.Errorf("%w: composite index %d out of range", ErrInvalidParameters, index)
	}
	c.optimize[index] = on
	return nil
}

// TransformPoint applies the transforms in order.
func (c *Composite) TransformPoint(point render.Vec3) render.Vec3 {
	for _, t := range c.transforms {
		point = t.TransformPoint(point)
	}
	return point
}

// NumberOfParameters sums the parameters of the optimized transforms.
func (c *Composite) NumberOfParameters() int {
	n := 0
	for i, t := range c.transforms {
		if c.optimize[i] {
			n += t.NumberOfParameters()
		}
	}
	return n
}

// Parameters concatenates the parameters of the optimized transforms.
func (c *Composite) Parameters() []float64 {
	out := make([]float64, 0, c.NumberOfParameters())
	for i, t := range c.transforms {
		if c.optimize[i] {
			out = append(out, t.Parameters()...)
		}
	}
	return out
}

// SetParameters splits parameters over the optimized transforms. Every
// slice is validated against a clone first, so a rejected vector leaves the
// composite unchanged.
func (c *Composite) SetParameters(parameters []float64) error {
	if len(parameters) != c.NumberOfParameters() {
		return fmt.Errorf("%w: composite parameters have %d values, want %d", ErrInvalidParameters, len(parameters), c.NumberOfParameters())
	}
	return c.applyToOptimized(parameters, func(t Transform, part []float64) error { return t.SetParameters(part) })
}

// FixedParameters concatenates every transform's fixed parameters.
func (c *Composite) FixedParameters() []float64 {
	var out []float64
	for _, t := range c.transforms {
		out = append(out, t.FixedParameters()...)
	}
	return out
}

// SetFixedParameters splits fixed over every transform according to the
// current length of each transform's fixed parameters.
func (c *Composite) SetFixedParameters(fixed []float64) error {
	total := 0
	for _, t := range c.transforms {
		total += len(t.FixedParameters())
	}
	if len(fixed) != total {
		return fmt.Errorf("%w: composite fixed parameters have %d values, want %d", ErrInvalidParameters, len(fixed), total)
	}
	clones := make([]Transform, len(c.transforms))
	offset := 0
	for i, t := range c.transforms {
		n := len(t.FixedParameters())
		clones[i] = t.Clone()
		if err := clones[i].SetFixedParameters(fixed[offset : offset+n]); err != nil {
			return fmt.Errorf("composite transform %d: %w", i, err)
		}
		offset += n
	}
	offset = 0
	for _, t := range c.transforms {
		n := len(t.FixedParameters())
		if err := t.SetFixedParameters(fixed[offset : offset+n]); err != nil {
			return err
		}
		offset += n
	}
	return nil
}

// UpdateParameters splits the step over the optimized transforms, each
// applying its own update rule.
func (c *Composite) UpdateParameters(update []float64, factor float64) error {
	if err := checkUpdate(update, c.NumberOfParameters(), factor); err != nil {
		return err
	}
	return c.applyToOptimized(update, func(t Transform, part []float64) error { return t.UpdateParameters(part, factor) })
}

// applyToOptimized runs apply on a clone of every optimized transform with
// its slice of values and, when all succeed, on the transforms themselves.
func (c *Composite) applyToOptimized(values []float64, apply func(Transform, []float64) error) error {
	offset := 0
	for i, t := range c.transforms {
		if !c.optimize[i] {
			continue
		}
		n := t.NumberOfParameters()
		if err := apply(t.Clone(), values[offset:offset+n]); err != nil {
			return fmt.Errorf("composite transform %d: %w", i, err)
		}
		offset += n
	}
	offset = 0
	for i, t := range c.transforms {
		if !c.optimize[i] {
			continue
		}
		n := t.NumberOfParameters()
		if err := apply(t, values[offset:offset+n]); err != nil {
			return fmt.Errorf("composite transform %d: %w", i, err)
		}
		offset += n
	}
	return nil
}

// JacobianWithRespectToParameters writes ∂T(p)/∂parameters by the chain
// rule, as ITK's CompositeTransform does: the columns of transform k,
// evaluated at its own input point x_k, are left-multiplied by the position
// Jacobians of every transform applied after it.
func (c *Composite) JacobianWithRespectToParameters(point render.Vec3, out *Jacobian) {
	out.reset(c.NumberOfParameters())
	var local Jacobian
	filled := 0
	for i, t := range c.transforms {
		if filled > 0 {
			position := t.JacobianWithRespectToPosition(point)
			for column := 0; column < filled; column++ {
				v := position.MulVec(render.Vec3{X: out.At(0, column), Y: out.At(1, column), Z: out.At(2, column)})
				out.setColumn(column, v)
			}
		}
		if c.optimize[i] {
			t.JacobianWithRespectToParameters(point, &local)
			for column := 0; column < local.Columns; column++ {
				out.setColumn(filled+column, render.Vec3{X: local.At(0, column), Y: local.At(1, column), Z: local.At(2, column)})
			}
			filled += local.Columns
		}
		point = t.TransformPoint(point)
	}
}

// JacobianWithRespectToPosition returns the product of the position
// Jacobians along the chain.
func (c *Composite) JacobianWithRespectToPosition(point render.Vec3) Matrix3 {
	out := Identity3()
	for _, t := range c.transforms {
		out = t.JacobianWithRespectToPosition(point).Mul(out)
		point = t.TransformPoint(point)
	}
	return out
}

// Clone returns a deep copy.
func (c *Composite) Clone() Transform {
	out := &Composite{transforms: make([]Transform, len(c.transforms)), optimize: append([]bool(nil), c.optimize...)}
	for i, t := range c.transforms {
		out.transforms[i] = t.Clone()
	}
	return out
}

// CollapseToAffine returns the single Affine equal to the chain, centred at
// the origin. Every component must be a MatrixTransform or a Composite of
// them; an empty composite collapses to the identity.
func (c *Composite) CollapseToAffine() (*Affine, error) {
	m, offset, err := c.linear()
	if err != nil {
		return nil, err
	}
	out := NewAffine()
	out.matrix = m
	if err := out.SetOffset(offset); err != nil {
		return nil, err
	}
	return out, nil
}

// linear returns the matrix and offset of a chain of linear transforms.
func (c *Composite) linear() (Matrix3, render.Vec3, error) {
	m, offset := Identity3(), render.Vec3{}
	for i, t := range c.transforms {
		var tm Matrix3
		var to render.Vec3
		switch v := t.(type) {
		case MatrixTransform:
			tm, to = v.Matrix(), v.Offset()
		case *Composite:
			var err error
			if tm, to, err = v.linear(); err != nil {
				return Matrix3{}, render.Vec3{}, err
			}
		default:
			return Matrix3{}, render.Vec3{}, fmt.Errorf("%w: composite transform %d (%s) is not linear", ErrUnsupportedTransform, i, t.Kind())
		}
		m = tm.Mul(m)
		offset = tm.MulVec(offset).Add(to)
	}
	if !m.Finite() || !finiteVec(offset) {
		return Matrix3{}, render.Vec3{}, fmt.Errorf("%w: composite is not finite", ErrInvalidMatrix)
	}
	return m, offset, nil
}
