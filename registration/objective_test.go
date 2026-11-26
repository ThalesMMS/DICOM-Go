package registration

import (
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/optimize"
	"github.com/ThalesMMS/dicom-go/render"
)

// TestRegistrationRecoversARigidMotion registers two 3D volumes related by
// a known rigid motion with each metric and regular-step gradient descent,
// using physical-shift scales and learning-rate estimation, and checks the
// recovered transform maps the fixed image's points where the motion does.
func TestRegistrationRecoversARigidMotion(t *testing.T) {
	const n = 40
	truth := NewEuler3D()
	centre := render.Vec3{X: 39, Y: 39, Z: 39}
	_ = truth.SetCenter(centre)
	if err := truth.SetParameters([]float64{0.06, -0.04, 0.08, 2.5, -1.8, 1.2}); err != nil {
		t.Fatal(err)
	}
	// Three blobs of different sizes, not in a line (two would leave the
	// rotation about their axis undetermined), in mm around the centre.
	blobs := func(p render.Vec3) float64 {
		a := p.Sub(render.Vec3{X: 30, Y: 36, Z: 40})
		b := p.Sub(render.Vec3{X: 48, Y: 44, Z: 36})
		c := p.Sub(render.Vec3{X: 38, Y: 30, Z: 48})
		return 800*math.Exp(-a.Dot(a)/60) + 500*math.Exp(-b.Dot(b)/120) + 650*math.Exp(-c.Dot(c)/90)
	}
	fixedData := make([]float32, n*n*n)
	movingData := make([]float32, n*n*n)
	spacing := render.Vec3{X: 2, Y: 2, Z: 2}
	for i := range fixedData {
		index := render.Vec3{X: float64(i % n), Y: float64(i / n % n), Z: float64(i / (n * n))}
		p := render.Vec3{X: index.X * spacing.X, Y: index.Y * spacing.Y, Z: index.Z * spacing.Z}
		fixedData[i] = float32(blobs(p))
		// moving(truth(x)) = fixed(x): moving at q holds fixed at truth⁻¹(q).
		inverse, _ := truth.Inverse()
		movingData[i] = float32(blobs(inverse.TransformPoint(p)))
	}
	fixed, _ := NewImage([3]int{n, n, n}, fixedData, render.Vec3{}, spacing, Identity3())
	moving, _ := NewImage([3]int{n, n, n}, movingData, render.Vec3{}, spacing, Identity3())

	mattes, _ := NewMattesMutualInformation(fixed, moving, MetricDomain{}, 0)
	ms, _ := NewMeanSquares(fixed, moving, MetricDomain{})
	nc, _ := NewCorrelation(fixed, moving, MetricDomain{})
	for name, metric := range map[string]Metric{"mean squares": ms, "correlation": nc, "mattes": mattes} {
		transform := NewEuler3D()
		_ = transform.SetCenter(centre)
		estimator := PhysicalShiftScales{Transform: transform.Clone(), Points: CornerPoints(fixed)}
		scales, err := estimator.Scales()
		if err != nil {
			t.Fatal(err)
		}
		o := optimize.RegularStepGradientDescent{
			RelaxationFactor: 0.5, MinimumStepLength: 1e-4, GradientMagnitudeTolerance: 1e-9, MaximumIterations: 300,
			Scales: scales, StepScale: estimator, MaximumStepSize: MaximumStepSize(fixed),
		}
		result, err := o.Minimize(Objective{Metric: metric, Transform: transform}, transform.Parameters())
		if err != nil {
			t.Fatal(err)
		}
		_ = transform.SetParameters(result.X)
		var worst float64
		for _, corner := range CornerPoints(fixed) {
			worst = math.Max(worst, transform.TransformPoint(corner).Sub(truth.TransformPoint(corner)).Length())
		}
		t.Logf("%s: %v after %d iterations (%s); worst corner error %.3f mm", name, result.X, result.Iterations, result.Stop, worst)
		if worst > 0.25 {
			t.Fatalf("%s: corners misplaced by %.3f mm", name, worst)
		}
	}
}
