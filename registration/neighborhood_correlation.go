package registration

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"

	"github.com/ThalesMMS/dicom-go/render"
)

var (
	ErrNeighborhoodCorrelation      = errors.New("registration: invalid neighborhood correlation")
	ErrNeighborhoodCorrelationLimit = errors.New("registration: neighborhood correlation resource limit")
)

// NeighborhoodCorrelationOptions declares a rectangular neighborhood in the
// fixed-image index axes. Exactly one radius representation must be nonzero.
// RadiusMM converts each axis to floor(radius/spacing) fixed-image samples.
// A zero-radius axis is allowed; an entirely zero neighborhood is refused.
type NeighborhoodCorrelationOptions struct {
	Radius   [3]int
	RadiusMM [3]float64
	// MinimumVariance is the minimum mean squared centered intensity on each
	// image. Neighborhoods at or below it are excluded (zero: exact constants).
	MinimumVariance        float64
	MaxCenters             int   // zero: 1,000,000
	MaxNeighborhoodSamples int   // zero: 4,096
	MaxWorkingBytes        int64 // zero: 16 MiB; excludes caller-owned images/transform
	MaxWork                int64 // zero: 100,000,000 sample-parameter visits
}

// NeighborhoodCorrelation minimizes the mean negative squared Pearson
// correlation over fixed-centered neighborhoods. Both masks restrict pairs and
// centers; image boundaries clip each neighborhood. Degenerate neighborhoods
// are excluded; no eligible neighborhood returns ErrTooFewSamples.
//
// Its derivative includes every pair's transform Jacobian, unlike ANTs' fast
// center-only derivative approximation. It implements Metric but does not
// assert identical ANTs optimization trajectories. Evaluation is serial,
// deterministic, bounded, cancellable and safe for concurrent read-only use.
// Images and masks remain borrowed: do not mutate them while using the metric.
type NeighborhoodCorrelation struct {
	evaluator    *evaluator
	options      NeighborhoodCorrelationOptions
	radius       [3]int
	neighborhood int
	centerBytes  int64
}

func NewNeighborhoodCorrelation(fixed, moving *Image, domain MetricDomain, options NeighborhoodCorrelationOptions) (*NeighborhoodCorrelation, error) {
	e, err := newEvaluator(fixed, moving, domain)
	if err != nil {
		return nil, err
	}
	if !finite(options.MinimumVariance) || options.MinimumVariance < 0 || options.MaxCenters < 0 || options.MaxNeighborhoodSamples < 0 || options.MaxWorkingBytes < 0 || options.MaxWork < 0 || domain.Workers < 0 {
		return nil, ErrNeighborhoodCorrelation
	}
	if options.MaxCenters == 0 {
		options.MaxCenters = 1000000
	}
	if options.MaxNeighborhoodSamples == 0 {
		options.MaxNeighborhoodSamples = 4096
	}
	if options.MaxWorkingBytes == 0 {
		options.MaxWorkingBytes = 16 << 20
	}
	if options.MaxWork == 0 {
		options.MaxWork = 100000000
	}
	if e.pointCount() > options.MaxCenters {
		return nil, ErrNeighborhoodCorrelationLimit
	}
	radius := options.Radius
	if options.RadiusMM != [3]float64{} {
		if radius != [3]int{} {
			return nil, ErrNeighborhoodCorrelation
		}
		spacing := [3]float64{fixed.spacing.X, fixed.spacing.Y, fixed.spacing.Z}
		for a, mm := range options.RadiusMM {
			if !finite(mm) || mm < 0 {
				return nil, ErrNeighborhoodCorrelation
			}
			samples := math.Floor(mm / spacing[a])
			if !finite(samples) || samples > float64((options.MaxNeighborhoodSamples-1)/2) {
				return nil, ErrNeighborhoodCorrelationLimit
			}
			radius[a] = int(samples)
		}
	}
	if radius == [3]int{} {
		return nil, ErrNeighborhoodCorrelation
	}
	count := 1
	for _, r := range radius {
		if r < 0 {
			return nil, ErrNeighborhoodCorrelation
		}
		if r > (options.MaxNeighborhoodSamples-1)/2 {
			return nil, ErrNeighborhoodCorrelationLimit
		}
		width := 2*r + 1
		if count > options.MaxNeighborhoodSamples/width {
			return nil, ErrNeighborhoodCorrelationLimit
		}
		count *= width
	}
	if int64(len(domain.Points)) > options.MaxWorkingBytes/24 {
		return nil, ErrNeighborhoodCorrelationLimit
	}
	centerBytes := int64(len(domain.Points)) * 24
	if int64(count) > (options.MaxWorkingBytes-centerBytes)/64 || int64(e.pointCount()) > options.MaxWork/int64(count) {
		return nil, ErrNeighborhoodCorrelationLimit
	}
	for _, point := range domain.Points {
		if !finiteVec(point) {
			return nil, ErrNeighborhoodCorrelation
		}
	}
	// Capture sparse center locations; image/transform ownership follows Metric.
	if domain.Points != nil {
		e.domain.Points = append([]render.Vec3{}, domain.Points...)
	}
	return &NeighborhoodCorrelation{evaluator: e, options: options, radius: radius, neighborhood: count, centerBytes: centerBytes}, nil
}

func (c *NeighborhoodCorrelation) Value(transform Transform) (float64, error) {
	v, _, err := c.evaluate(context.Background(), transform, false)
	return v, err
}

func (c *NeighborhoodCorrelation) ValueAndDerivative(transform Transform) (float64, []float64, error) {
	return c.evaluate(context.Background(), transform, true)
}

func (c *NeighborhoodCorrelation) ValueContext(ctx context.Context, transform Transform) (float64, error) {
	v, _, err := c.evaluate(ctx, transform, false)
	return v, err
}

func (c *NeighborhoodCorrelation) ValueAndDerivativeContext(ctx context.Context, transform Transform) (float64, []float64, error) {
	return c.evaluate(ctx, transform, true)
}

func (c *NeighborhoodCorrelation) evaluate(ctx context.Context, transform Transform, derivatives bool) (float64, []float64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	if c == nil || c.evaluator == nil || transform == nil {
		return 0, nil, ErrNeighborhoodCorrelation
	}
	if value := reflect.ValueOf(transform); value.Kind() == reflect.Pointer && value.IsNil() {
		return 0, nil, ErrNeighborhoodCorrelation
	}
	parameters := 0
	if derivatives {
		parameters = transform.NumberOfParameters()
	}
	if parameters < 0 || parameters > 4096 {
		return 0, nil, ErrNeighborhoodCorrelationLimit
	}
	if int64(c.neighborhood) > c.options.MaxWork/int64(parameters+1) {
		return 0, nil, ErrNeighborhoodCorrelationLimit
	}
	workPerCenter := int64(c.neighborhood) * int64(parameters+1)
	if int64(c.evaluator.pointCount()) > c.options.MaxWork/workPerCenter || int64(parameters) > (c.options.MaxWorkingBytes-c.centerBytes-int64(c.neighborhood)*64)/40 {
		return 0, nil, ErrNeighborhoodCorrelationLimit
	}
	samples := make([]sample, 0, c.neighborhood)
	var derivative []float64
	if derivatives {
		derivative = make([]float64, parameters)
	}
	var jacobian Jacobian
	var total float64
	eligible := 0
	for center := 0; center < c.evaluator.pointCount(); center++ {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		point, _, ok := c.evaluator.fixedSample(center)
		if !ok {
			continue
		}
		if _, valid, err := c.pair(point, transform, false); err != nil {
			return 0, nil, err
		} else if !valid {
			continue
		}
		index := c.evaluator.fixed.ContinuousIndex(point)
		samples = samples[:0]
		var meanF, meanM float64
		for z := -c.radius[2]; z <= c.radius[2]; z++ {
			for y := -c.radius[1]; y <= c.radius[1]; y++ {
				for x := -c.radius[0]; x <= c.radius[0]; x++ {
					if err := ctx.Err(); err != nil {
						return 0, nil, err
					}
					neighbor := c.evaluator.fixed.Point(index.Add(render.Vec3{X: float64(x), Y: float64(y), Z: float64(z)}))
					s, valid, err := c.pair(neighbor, transform, derivatives)
					if err != nil {
						return 0, nil, err
					}
					if !valid {
						continue
					}
					samples = append(samples, s)
					// Online means followed by centered sums avoid subtracting large raw
					// moments when images have a large common intensity offset.
					n := float64(len(samples))
					meanF += (s.fixed - meanF) / n
					meanM += (s.moving - meanM) / n
				}
			}
		}
		if len(samples) < 2 {
			continue
		}
		var ff, mm, fm float64
		for _, s := range samples {
			f, m := s.fixed-meanF, s.moving-meanM
			ff += f * f
			mm += m * m
			fm += f * m
		}
		n := float64(len(samples))
		if ff/n <= c.options.MinimumVariance || mm/n <= c.options.MinimumVariance {
			continue
		}
		// Normalize separately to avoid the product ff*mm overflowing.
		rho := (fm / math.Sqrt(ff)) / math.Sqrt(mm)
		if !finite(rho) {
			return 0, nil, ErrNeighborhoodCorrelation
		}
		total -= rho * rho
		eligible++
		if !derivatives {
			continue
		}
		scale := -2 * (fm / ff) / mm
		for _, s := range samples {
			if err := ctx.Err(); err != nil {
				return 0, nil, err
			}
			transform.JacobianWithRespectToParameters(s.fixedPoint, &jacobian)
			if jacobian.Columns != parameters || len(jacobian.Values) != 3*parameters {
				return 0, nil, ErrNeighborhoodCorrelation
			}
			factor := scale * ((s.fixed - meanF) - (fm/mm)*(s.moving-meanM))
			for p := 0; p < parameters; p++ {
				g := s.gradient.X*jacobian.At(0, p) + s.gradient.Y*jacobian.At(1, p) + s.gradient.Z*jacobian.At(2, p)
				if !finite(g) {
					return 0, nil, ErrNeighborhoodCorrelation
				}
				derivative[p] += factor * g
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	if eligible == 0 {
		return 0, nil, ErrTooFewSamples
	}
	for p := range derivative {
		derivative[p] /= float64(eligible)
		if !finite(derivative[p]) {
			return 0, nil, ErrNeighborhoodCorrelation
		}
	}
	return total / float64(eligible), derivative, nil
}

func (c *NeighborhoodCorrelation) pair(point render.Vec3, transform Transform, gradients bool) (sample, bool, error) {
	e := c.evaluator
	fixed, ok := e.fixed.Interpolate(point)
	if !ok {
		return sample{}, false, nil
	}
	fixedMask, err := neighborhoodMaskAt(e.domain.FixedMask, point)
	if err != nil || !fixedMask {
		return sample{}, false, err
	}
	mapped := transform.TransformPoint(point)
	if !finiteVec(mapped) {
		return sample{}, false, ErrNeighborhoodCorrelation
	}
	movingMask, err := neighborhoodMaskAt(e.domain.MovingMask, mapped)
	if err != nil || !movingMask {
		return sample{}, false, err
	}
	index := e.moving.ContinuousIndex(mapped)
	if !e.moving.inside(index) {
		return sample{}, false, nil
	}
	moving := e.moving.linearAt(index)
	if !finite(fixed) || !finite(moving) {
		return sample{}, false, fmt.Errorf("%w: nonfinite pair", ErrNeighborhoodCorrelation)
	}
	s := sample{fixedPoint: point, fixed: fixed, moving: moving}
	if gradients {
		s.gradient = e.moving.gradientAt(index)
		if !finiteVec(s.gradient) {
			return sample{}, false, ErrNeighborhoodCorrelation
		}
	}
	return s, true, nil
}

func neighborhoodMaskAt(mask *Image, point render.Vec3) (bool, error) {
	if mask == nil {
		return true, nil
	}
	index := mask.ContinuousIndex(point)
	if !mask.inside(index) {
		return false, nil
	}
	x, y, z := int(math.Floor(index.X+.5)), int(math.Floor(index.Y+.5)), int(math.Floor(index.Z+.5))
	value := float64(mask.At(x, y, z))
	if !finite(value) {
		return false, ErrNeighborhoodCorrelation
	}
	return value != 0, nil
}
