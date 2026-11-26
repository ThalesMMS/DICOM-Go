package registration

// Metric is an image similarity metric to minimize over a transform's
// parameters: MeanSquares, Correlation or MattesMutualInformation.
type Metric interface {
	Value(transform Transform) (float64, error)
	ValueAndDerivative(transform Transform) (float64, []float64, error)
}

// Objective adapts a metric and a transform to the optimize package's
// Function and GradientFunction: the optimized vector is the transform's
// parameter vector, which Objective sets before each evaluation (the
// transform is owned by the objective during an optimization; read the
// result from Result.X or from the transform afterwards).
type Objective struct {
	Metric    Metric
	Transform Transform
}

// Value is the metric at parameters x.
func (o Objective) Value(x []float64) (float64, error) {
	if err := o.Transform.SetParameters(x); err != nil {
		return 0, err
	}
	return o.Metric.Value(o.Transform)
}

// ValueAndGradient is the metric and its gradient at parameters x.
func (o Objective) ValueAndGradient(x []float64) (float64, []float64, error) {
	if err := o.Transform.SetParameters(x); err != nil {
		return 0, nil, err
	}
	return o.Metric.ValueAndDerivative(o.Transform)
}
