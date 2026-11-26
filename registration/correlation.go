package registration

// Correlation is the negated squared normalized cross-correlation of the
// fixed image and the mapped moving image, for images whose intensities are
// linearly related:
//
//	V = −(Σ F·M)² / (Σ F² · Σ M²),  F = f − mean(f), M = m − mean(m)
//
// over the N fixed points of the domain that map inside the moving image;
// V is −1 for a perfect linear match and 0 without correlation.
type Correlation struct {
	evaluator *evaluator
}

// NewCorrelation compares fixed and moving over domain.
func NewCorrelation(fixed, moving *Image, domain MetricDomain) (*Correlation, error) {
	e, err := newEvaluator(fixed, moving, domain)
	if err != nil {
		return nil, err
	}
	return &Correlation{evaluator: e}, nil
}

// Value is V for transform.
func (c *Correlation) Value(transform Transform) (float64, error) {
	value, _, err := c.evaluate(transform, false)
	return value, err
}

// ValueAndDerivative is V and its gradient with respect to the transform
// parameters. With g = ∇m(T(x)) · ∂T/∂p(x) and the sums above,
//
//	∂V/∂p = −2 ΣFM / (ΣF² ΣM²) · Σ (F − (ΣFM/ΣM²)·M) g
func (c *Correlation) ValueAndDerivative(transform Transform) (float64, []float64, error) {
	return c.evaluate(transform, true)
}

// correlationPartial holds raw moments, which one pass accumulates.
type correlationPartial struct {
	count            int
	f, m, ff, mm, fm float64
	g, fg, mg        []float64 // Σg, Σf·g, Σm·g per parameter
}

func (c *Correlation) evaluate(transform Transform, derivatives bool) (float64, []float64, error) {
	parameters := transform.NumberOfParameters()
	var total correlationPartial
	if derivatives {
		total.g, total.fg, total.mg = make([]float64, parameters), make([]float64, parameters), make([]float64, parameters)
	}
	reduceChunks(c.evaluator, transform, derivatives, 0, func(samples []sample) correlationPartial {
		partial := correlationPartial{count: len(samples)}
		var jacobian Jacobian
		if derivatives {
			partial.g, partial.fg, partial.mg = make([]float64, parameters), make([]float64, parameters), make([]float64, parameters)
		}
		for _, s := range samples {
			partial.f += s.fixed
			partial.m += s.moving
			partial.ff += s.fixed * s.fixed
			partial.mm += s.moving * s.moving
			partial.fm += s.fixed * s.moving
			if derivatives {
				transform.JacobianWithRespectToParameters(s.fixedPoint, &jacobian)
				for p := 0; p < parameters; p++ {
					g := s.gradient.X*jacobian.At(0, p) + s.gradient.Y*jacobian.At(1, p) + s.gradient.Z*jacobian.At(2, p)
					partial.g[p] += g
					partial.fg[p] += s.fixed * g
					partial.mg[p] += s.moving * g
				}
			}
		}
		return partial
	}, func(partial correlationPartial) {
		total.count += partial.count
		total.f += partial.f
		total.m += partial.m
		total.ff += partial.ff
		total.mm += partial.mm
		total.fm += partial.fm
		for p := range partial.g {
			total.g[p] += partial.g[p]
			total.fg[p] += partial.fg[p]
			total.mg[p] += partial.mg[p]
		}
	})
	if total.count < 2 {
		return 0, nil, ErrTooFewSamples
	}
	n := float64(total.count)
	meanF, meanM := total.f/n, total.m/n
	sff := total.ff - n*meanF*meanF
	smm := total.mm - n*meanM*meanM
	sfm := total.fm - n*meanF*meanM
	if sff <= 0 || smm <= 0 {
		// A constant image has no correlation to measure.
		return 0, nil, ErrTooFewSamples
	}
	value := -sfm * sfm / (sff * smm)
	if !derivatives {
		return value, nil, nil
	}
	derivative := make([]float64, parameters)
	scale := -2 * sfm / (sff * smm)
	for p := range derivative {
		sumFG := total.fg[p] - meanF*total.g[p] // Σ F·g
		sumMG := total.mg[p] - meanM*total.g[p] // Σ M·g
		derivative[p] = scale * (sumFG - sfm/smm*sumMG)
	}
	return value, derivative, nil
}
