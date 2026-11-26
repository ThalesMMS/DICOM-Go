package registration

import "fmt"

// Composite2D stores components in application order: [A,B] maps p to
// B(A(p)). Components are owned references, all optimized by default.
// It admits at most 1024 built-in linear components, without nesting or
// repeated references. Use Clone for independent occurrences of one mapping.
// Serialization preserves geometry/components but not optimization flags.
type Composite2D struct {
	transforms []Transform2D
	optimize   []bool
}

func NewComposite2D(ts ...Transform2D) (*Composite2D, error) {
	c := &Composite2D{}
	for _, t := range ts {
		if err := c.Append(t); err != nil {
			return nil, err
		}
	}
	return c, nil
}
func (*Composite2D) Kind() Kind                    { return KindComposite }
func (c *Composite2D) Len() int                    { return len(c.transforms) }
func (c *Composite2D) Transform(i int) Transform2D { return c.transforms[i] }
func (c *Composite2D) Append(t Transform2D) error {
	if len(c.transforms) >= 1024 {
		return fmt.Errorf("%w: 2D composite component limit", ErrInvalidParameters)
	}
	valid := false
	switch v := t.(type) {
	case *Translation2D:
		valid = v != nil
	case *Euler2D:
		valid = v != nil
	case *Similarity2D:
		valid = v != nil
	case *Affine2D:
		valid = v != nil
	}
	if !valid {
		return fmt.Errorf("%w: 2D composite requires built-in linear components", ErrUnsupportedTransform)
	}
	for _, existing := range c.transforms {
		if existing == t {
			return fmt.Errorf("%w: duplicate 2D component reference; use Clone for independent parameters", ErrInvalidParameters)
		}
	}
	c.transforms = append(c.transforms, t)
	c.optimize = append(c.optimize, true)
	return nil
}
func (c *Composite2D) Optimized(i int) bool { return c.optimize[i] }
func (c *Composite2D) SetOptimized(i int, on bool) error {
	if i < 0 || i >= c.Len() {
		return ErrInvalidParameters
	}
	c.optimize[i] = on
	return nil
}
func (c *Composite2D) TransformPoint(p Point2) Point2 {
	for _, t := range c.transforms {
		p = t.TransformPoint(p)
	}
	return p
}
func (c *Composite2D) NumberOfParameters() int {
	n := 0
	for i, t := range c.transforms {
		if c.optimize[i] {
			n += t.NumberOfParameters()
		}
	}
	return n
}
func (c *Composite2D) Parameters() []float64 {
	p := make([]float64, 0, c.NumberOfParameters())
	for i, t := range c.transforms {
		if c.optimize[i] {
			p = append(p, t.Parameters()...)
		}
	}
	return p
}
func (c *Composite2D) FixedParameters() []float64 {
	p := []float64{}
	for _, t := range c.transforms {
		p = append(p, t.FixedParameters()...)
	}
	return p
}
func (c *Composite2D) apply(p []float64, fixed bool, fn func(Transform2D, []float64) error) error {
	n := c.NumberOfParameters()
	if fixed {
		n = len(c.FixedParameters())
	}
	if err := checkLength(p, n, "2D composite parameters"); err != nil {
		return err
	}
	at := 0
	for i, t := range c.transforms {
		if !fixed && !c.optimize[i] {
			continue
		}
		count := t.NumberOfParameters()
		if fixed {
			count = len(t.FixedParameters())
		}
		if err := fn(t.Clone(), p[at:at+count]); err != nil {
			return fmt.Errorf("component%d: %w", i, err)
		}
		at += count
	}
	at = 0
	for i, t := range c.transforms {
		if !fixed && !c.optimize[i] {
			continue
		}
		count := t.NumberOfParameters()
		if fixed {
			count = len(t.FixedParameters())
		}
		if err := fn(t, p[at:at+count]); err != nil {
			return err
		}
		at += count
	}
	return nil
}
func (c *Composite2D) SetParameters(p []float64) error {
	return c.apply(p, false, func(t Transform2D, p []float64) error { return t.SetParameters(p) })
}
func (c *Composite2D) SetFixedParameters(p []float64) error {
	return c.apply(p, true, func(t Transform2D, p []float64) error { return t.SetFixedParameters(p) })
}
func (c *Composite2D) UpdateParameters(p []float64, f float64) error {
	if err := checkUpdate(p, c.NumberOfParameters(), f); err != nil {
		return err
	}
	return c.apply(p, false, func(t Transform2D, p []float64) error { return t.UpdateParameters(p, f) })
}
func (c *Composite2D) Clone() Transform2D {
	out := &Composite2D{optimize: append([]bool(nil), c.optimize...)}
	for _, t := range c.transforms {
		out.transforms = append(out.transforms, t.Clone())
	}
	return out
}
func (c *Composite2D) JacobianWithRespectToPosition(p Point2) Matrix2 {
	m := Identity2()
	for _, t := range c.transforms {
		m = t.JacobianWithRespectToPosition(p).Mul(m)
		p = t.TransformPoint(p)
	}
	return m
}
func (c *Composite2D) JacobianWithRespectToParameters(p Point2, j *Jacobian2D) {
	j.reset(c.NumberOfParameters())
	points := make([]Point2, c.Len())
	cols := make([]int, c.Len())
	at := 0
	for i, t := range c.transforms {
		points[i] = p
		cols[i] = at
		if c.optimize[i] {
			at += t.NumberOfParameters()
		}
		p = t.TransformPoint(p)
	}
	suffix := Identity2()
	var part Jacobian2D
	for i := c.Len() - 1; i >= 0; i-- {
		t := c.transforms[i]
		if c.optimize[i] {
			t.JacobianWithRespectToParameters(points[i], &part)
			for k := 0; k < part.Columns; k++ {
				v := suffix.MulVec(Point2{part.At(0, k), part.At(1, k)})
				j.set(0, cols[i]+k, v[0])
				j.set(1, cols[i]+k, v[1])
			}
		}
		suffix = suffix.Mul(t.JacobianWithRespectToPosition(points[i]))
	}
}
