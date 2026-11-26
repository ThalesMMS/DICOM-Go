package registration

// MeanSquares is the mean squared intensity difference between the fixed
// image and the moving image mapped by a transform, for images of the same
// modality:
//
//	V = (1/N) Σ (f(x) − m(T(x)))²
//
// over the N fixed points of the domain that map inside the moving image.
type MeanSquares struct {
	evaluator *evaluator
}

// NewMeanSquares compares fixed and moving over domain.
func NewMeanSquares(fixed, moving *Image, domain MetricDomain) (*MeanSquares, error) {
	e, err := newEvaluator(fixed, moving, domain)
	if err != nil {
		return nil, err
	}
	return &MeanSquares{evaluator: e}, nil
}

// Value is V for transform.
func (m *MeanSquares) Value(transform Transform) (float64, error) {
	value, _, err := m.evaluate(transform, false)
	return value, err
}

// ValueAndDerivative is V and ∂V/∂p, the gradient with respect to the
// transform parameters (an optimizer minimizing V steps against it):
//
//	∂V/∂p = (2/N) Σ (m(T(x)) − f(x)) ∇m(T(x)) · ∂T/∂p(x)
func (m *MeanSquares) ValueAndDerivative(transform Transform) (float64, []float64, error) {
	return m.evaluate(transform, true)
}

type meanSquaresPartial struct {
	count      int
	sum        float64
	derivative []float64
}

func (m *MeanSquares) evaluate(transform Transform, derivatives bool) (float64, []float64, error) {
	parameters := transform.NumberOfParameters()
	var count int
	var sum float64
	derivative := make([]float64, parameters)
	reduceChunks(m.evaluator, transform, derivatives, 0, func(samples []sample) meanSquaresPartial {
		partial := meanSquaresPartial{count: len(samples)}
		var jacobian Jacobian
		if derivatives {
			partial.derivative = make([]float64, parameters)
		}
		for _, s := range samples {
			difference := s.moving - s.fixed
			partial.sum += difference * difference
			if derivatives {
				accumulateDerivative(partial.derivative, &jacobian, transform, s, difference)
			}
		}
		return partial
	}, func(partial meanSquaresPartial) {
		count += partial.count
		sum += partial.sum
		for p, d := range partial.derivative {
			derivative[p] += d
		}
	})
	if count == 0 {
		return 0, nil, ErrTooFewSamples
	}
	for p := range derivative {
		derivative[p] *= 2 / float64(count)
	}
	if !derivatives {
		derivative = nil
	}
	return sum / float64(count), derivative, nil
}
