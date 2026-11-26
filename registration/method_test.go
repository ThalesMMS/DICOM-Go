package registration

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

// phantom is a smooth, asymmetric test object in millimetres: three blobs
// of different sizes and contrasts, not in a line.
func phantom(p render.Vec3) float64 {
	a := p.Sub(render.Vec3{X: 40, Y: 48, Z: 52})
	b := p.Sub(render.Vec3{X: 62, Y: 56, Z: 44})
	c := p.Sub(render.Vec3{X: 50, Y: 38, Z: 60})
	return 800*math.Exp(-a.Dot(a)/90) + 500*math.Exp(-b.Dot(b)/200) + 650*math.Exp(-c.Dot(c)/140)
}

// imagePair samples the phantom as a fixed image and, through the inverse
// of truth, as a moving image: moving(truth(x)) = fixed(x).
func imagePair(t *testing.T, truth Transform, invert func(render.Vec3) render.Vec3) (*Image, *Image) {
	t.Helper()
	const n = 48
	spacing := render.Vec3{X: 2, Y: 2, Z: 2}
	fixedData := make([]float32, n*n*n)
	movingData := make([]float32, n*n*n)
	for i := range fixedData {
		p := render.Vec3{X: float64(i%n) * spacing.X, Y: float64(i/n%n) * spacing.Y, Z: float64(i/(n*n)) * spacing.Z}
		fixedData[i] = float32(phantom(p))
		movingData[i] = float32(phantom(invert(p)))
	}
	fixed, err := NewImage([3]int{n, n, n}, fixedData, render.Vec3{}, spacing, Identity3())
	if err != nil {
		t.Fatal(err)
	}
	moving, err := NewImage([3]int{n, n, n}, movingData, render.Vec3{}, spacing, Identity3())
	if err != nil {
		t.Fatal(err)
	}
	return fixed, moving
}

// errors between a recovered and a true matrix transform: the rotation
// angle of R_est·R_trueᵀ in degrees and the largest point error over the
// fixed image's corners and centre, in millimetres.
func transformErrors(estimate, truth MatrixTransform, fixed *Image) (degrees, millimetres float64) {
	product := estimate.Matrix().Mul(truth.Matrix().Transpose())
	cosine := (product[0][0] + product[1][1] + product[2][2] - 1) / 2
	degrees = math.Acos(math.Max(-1, math.Min(1, cosine))) * 180 / math.Pi
	points := append(CornerPoints(fixed), fixed.Point(render.Vec3{X: 23.5, Y: 23.5, Z: 23.5}))
	for _, p := range points {
		millimetres = math.Max(millimetres, estimate.TransformPoint(p).Sub(truth.TransformPoint(p)).Length())
	}
	return degrees, millimetres
}

// TestMethodRecoversKnownRigidMotions registers volumes related by a known
// rigid motion of several degrees and millimetres, from a centre-of-mass
// initialization, with each metric: the rotation must be recovered within
// 0.5° and every corner within 0.5 mm.
func TestMethodRecoversKnownRigidMotions(t *testing.T) {
	truth := NewEuler3D()
	_ = truth.SetCenter(render.Vec3{X: 47, Y: 47, Z: 47})
	degree := math.Pi / 180
	if err := truth.SetParameters([]float64{5 * degree, -3 * degree, 7 * degree, 8, -6, 5}); err != nil {
		t.Fatal(err)
	}
	inverse, _ := truth.Inverse()
	fixed, moving := imagePair(t, truth, inverse.TransformPoint)
	for name, metric := range map[string]func(*Image, *Image, MetricDomain) (Metric, error){
		"mattes":       nil,
		"mean squares": func(f, m *Image, d MetricDomain) (Metric, error) { return NewMeanSquares(f, m, d) },
		"correlation":  func(f, m *Image, d MetricDomain) (Metric, error) { return NewCorrelation(f, m, d) },
	} {
		initial := NewEuler3D()
		if err := CenterTransform(initial, fixed, moving, CenterMoments); err != nil {
			t.Fatal(err)
		}
		method := Method{Fixed: fixed, Moving: moving, Transform: initial, NewMetric: metric, Sampling: SampleRegular, Seed: 7,
			Levels: []Level{{Shrink: 2, SmoothingSigmaMM: 2, SamplingPercentage: 0.5}, {Shrink: 1, SmoothingSigmaMM: 1, SamplingPercentage: 0.5}, {Shrink: 1, SamplingPercentage: 0.5}}}
		result, err := method.Run(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		degrees, millimetres := transformErrors(result.Transform.(MatrixTransform), truth, fixed)
		for _, level := range result.Levels {
			t.Logf("%s: level %+v: %d points, %d iterations, %s, value %.6g", name, level.Level, level.Points, level.Iterations, level.Stop, level.Value)
		}
		t.Logf("%s: rotation error %.3f°, point error %.3f mm", name, degrees, millimetres)
		if degrees > 0.5 || millimetres > 0.5 {
			t.Fatalf("%s: rotation error %.3f°, point error %.3f mm", name, degrees, millimetres)
		}
	}
}

// TestMethodRecoversAnAffineWithRandomSampling: an anisotropic scaling and
// shear with a translation, registered with mean squares on 30% random
// samples from a geometric-centre initialization.
func TestMethodRecoversAnAffineWithRandomSampling(t *testing.T) {
	truth := NewAffine()
	_ = truth.SetCenter(render.Vec3{X: 47, Y: 47, Z: 47})
	if err := truth.SetParameters([]float64{1.04, 0.03, 0, -0.02, 0.97, 0.02, 0.01, 0, 1.03, 3, -2.5, 4}); err != nil {
		t.Fatal(err)
	}
	inverse, err := truth.Inverse()
	if err != nil {
		t.Fatal(err)
	}
	fixed, moving := imagePair(t, truth, inverse.TransformPoint)
	initial := NewAffine()
	if err := CenterTransform(initial, fixed, moving, CenterGeometry); err != nil {
		t.Fatal(err)
	}
	method := Method{
		Fixed: fixed, Moving: moving, Transform: initial, Sampling: SampleRandom, Seed: 3,
		NewMetric: func(f, m *Image, d MetricDomain) (Metric, error) { return NewMeanSquares(f, m, d) },
		Levels:    []Level{{Shrink: 2, SmoothingSigmaMM: 2, SamplingPercentage: 0.3}, {Shrink: 1, SmoothingSigmaMM: 1, SamplingPercentage: 0.3}, {Shrink: 1, SamplingPercentage: 0.3}},
	}
	method.Optimizer.MaximumIterations = 400
	result, err := method.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, millimetres := transformErrors(result.Transform.(MatrixTransform), truth, fixed)
	t.Logf("affine: point error %.3f mm after %d/%d/%d iterations", millimetres, result.Levels[0].Iterations, result.Levels[1].Iterations, result.Levels[2].Iterations)
	if millimetres > 0.5 {
		t.Fatalf("affine: point error %.3f mm", millimetres)
	}
}

// TestMethodSamplingIsValidatedRepeatableAndCancellable: ITK's
// itkImageRegistrationSamplingTest percentages (0.1 accepted; −0.1, 0 and
// 1.1 refused), identical results for the same seed, and a cancelled
// context stopping the run.
func TestMethodSamplingIsValidatedRepeatableAndCancellable(t *testing.T) {
	if err := ValidateSamplingPercentage(0.1); err != nil {
		t.Fatal(err)
	}
	for _, p := range []float64{-0.1, 0, 1.1} {
		if err := ValidateSamplingPercentage(p); !errors.Is(err, ErrSamplingPercentage) {
			t.Fatalf("percentage %v: %v", p, err)
		}
	}
	truth := NewEuler3D()
	_ = truth.SetParameters([]float64{0, 0, 0.05, 2, 0, 0})
	inverse, _ := truth.Inverse()
	fixed, moving := imagePair(t, truth, inverse.TransformPoint)
	method := Method{Fixed: fixed, Moving: moving, Transform: NewEuler3D(), Sampling: SampleRandom, Seed: 11,
		Levels: []Level{{Shrink: 2, SmoothingSigmaMM: 1, SamplingPercentage: 0.2}}}
	method.Optimizer.MaximumIterations = 20
	first, err := method.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := method.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range first.Transform.Parameters() {
		if second.Transform.Parameters()[i] != p {
			t.Fatal("the same seed gave different results")
		}
	}
	bad := method
	bad.Levels = []Level{{Shrink: 1, SamplingPercentage: 1.1}}
	if _, err := bad.Run(context.Background()); !errors.Is(err, ErrSamplingPercentage) {
		t.Fatalf("level percentage 1.1: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	method.Progress = func(p Progress) {
		calls++
		if calls == 3 {
			cancel()
		}
	}
	if _, err := method.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled run: %v", err)
	}
	if calls != 3 {
		t.Fatalf("%d evaluations after cancellation", calls)
	}
}

// TestCenterTransform aligns geometric centres and centres of mass.
func TestCenterTransform(t *testing.T) {
	truth := NewEuler3D()
	_ = truth.SetParameters([]float64{0, 0, 0, 6, -4, 2})
	inverse, _ := truth.Inverse()
	fixed, moving := imagePair(t, truth, inverse.TransformPoint)
	transform := NewEuler3D()
	if err := CenterTransform(transform, fixed, moving, CenterMoments); err != nil {
		t.Fatal(err)
	}
	// The object moved by (6, −4, 2); its centre of mass moves with it
	// (up to what leaves the field of view).
	if got := transform.Translation(); got.Sub(render.Vec3{X: 6, Y: -4, Z: 2}).Length() > 0.2 {
		t.Fatalf("moments translation %v", got)
	}
	if err := CenterTransform(transform, fixed, moving, CenterGeometry); err != nil {
		t.Fatal(err)
	}
	if transform.Translation().Length() != 0 || transform.Center() != fixed.Point(render.Vec3{X: 23.5, Y: 23.5, Z: 23.5}) {
		t.Fatalf("geometry: centre %v translation %v", transform.Center(), transform.Translation())
	}
	empty, _ := NewImage([3]int{2, 2, 2}, make([]float32, 8), render.Vec3{}, render.Vec3{X: 1, Y: 1, Z: 1}, Identity3())
	if err := CenterTransform(transform, empty, moving, CenterMoments); !errors.Is(err, ErrNoMass) {
		t.Fatalf("empty image: %v", err)
	}
}
