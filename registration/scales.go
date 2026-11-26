package registration

import (
	"errors"
	"math"

	"github.com/ThalesMMS/dicom-go/render"
)

// PhysicalShiftScales estimates optimizer scales for a transform from the
// physical displacement its parameters cause at sample points, after ITK's
// RegistrationParameterScalesFromPhysicalShift: a rotation in radians and a
// translation in millimetres then move the image comparably. It implements
// optimize.StepScaleEstimator for learning-rate estimation.
type PhysicalShiftScales struct {
	// Transform is probed around its current parameters; it is cloned,
	// never changed.
	Transform Transform
	// Points are the physical sample points: CornerPoints of the fixed
	// image for a matrix transform, as in ITK.
	Points []render.Vec3
	// SmallParameterVariation is the probing change of a parameter
	// (0: 0.01).
	SmallParameterVariation float64
}

// ErrNoSamplePoints reports a scale estimator without points.
var ErrNoSamplePoints = errors.New("registration: no sample points for parameter scales")

func (e PhysicalShiftScales) variation() float64 {
	if e.SmallParameterVariation > 0 {
		return e.SmallParameterVariation
	}
	return 0.01
}

// maxShift is the largest displacement of a sample point when the
// parameters change by delta.
func (e PhysicalShiftScales) maxShift(delta []float64) (float64, error) {
	if len(e.Points) == 0 {
		return 0, ErrNoSamplePoints
	}
	moved := e.Transform.Clone()
	if err := moved.UpdateParameters(delta, 1); err != nil {
		return 0, err
	}
	var out float64
	for _, point := range e.Points {
		out = math.Max(out, moved.TransformPoint(point).Sub(e.Transform.TransformPoint(point)).Length())
	}
	return out, nil
}

// Scales returns one scale per parameter: the squared largest shift per
// unit change of that parameter, (shift/variation)². A parameter that moves
// no point takes the smallest non-zero scale; if none moves any, every
// scale is 1.
func (e PhysicalShiftScales) Scales() ([]float64, error) {
	n := e.Transform.NumberOfParameters()
	v := e.variation()
	shifts := make([]float64, n)
	smallest := math.Inf(1)
	for i := range shifts {
		delta := make([]float64, n)
		delta[i] = v
		shift, err := e.maxShift(delta)
		if err != nil {
			return nil, err
		}
		shifts[i] = shift
		if shift > epsilon {
			smallest = math.Min(smallest, shift)
		}
	}
	scales := make([]float64, n)
	for i, shift := range shifts {
		switch {
		case math.IsInf(smallest, 1):
			scales[i] = 1
		case shift <= epsilon:
			scales[i] = smallest * smallest / (v * v)
		default:
			scales[i] = shift * shift / (v * v)
		}
	}
	return scales, nil
}

// StepScale is the largest displacement a parameter step causes, measured
// on the step reduced to the probing variation and scaled back, so that it
// stays in the transform's linear range.
func (e PhysicalShiftScales) StepScale(step []float64) (float64, error) {
	var largest float64
	for _, s := range step {
		largest = math.Max(largest, math.Abs(s))
	}
	if largest <= epsilon {
		return 0, nil
	}
	factor := e.variation() / largest
	reduced := make([]float64, len(step))
	for i, s := range step {
		reduced[i] = s * factor
	}
	shift, err := e.maxShift(reduced)
	if err != nil {
		return 0, err
	}
	return shift / factor, nil
}

// CornerPoints are the physical positions of the corner samples of an
// image, the sample points ITK uses for the scales of a linear transform.
func CornerPoints(im *Image) []render.Vec3 {
	var out []render.Vec3
	seen := map[[3]int]bool{}
	for corner := 0; corner < 8; corner++ {
		var index [3]int
		for axis := 0; axis < 3; axis++ {
			if corner>>axis&1 == 1 {
				index[axis] = im.size[axis] - 1
			}
		}
		if seen[index] {
			continue
		}
		seen[index] = true
		out = append(out, im.Point(render.Vec3{X: float64(index[0]), Y: float64(index[1]), Z: float64(index[2])}))
	}
	return out
}

// MaximumStepSize is the smallest sample spacing of an image, ITK's default
// largest physical step for learning-rate estimation.
func MaximumStepSize(im *Image) float64 {
	return math.Min(im.spacing.X, math.Min(im.spacing.Y, im.spacing.Z))
}
