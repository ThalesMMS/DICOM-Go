package registration

import (
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

// itkGaussians are the images of ITK's
// itkMattesMutualInformationImageToImageMetricv4Test: 100×100 samples of
// spacing (3, 2) at origin (0.001, −0.002), holding
// uint8(200·exp(−|d|²/s²)) with d the index offset from the centre (50, 50)
// and s = 50; the moving Gaussian is displaced by (5, 5) index units.
func itkGaussians(t testing.TB) (fixed, moving *Image) {
	t.Helper()
	const n = 100
	make2D := func(displacement float64) *Image {
		data := make([]float32, n*n)
		for y := 0; y < n; y++ {
			for x := 0; x < n; x++ {
				dx, dy := float64(x)-n/2+displacement, float64(y)-n/2+displacement
				data[y*n+x] = float32(uint8(200 * math.Exp(-(dx*dx+dy*dy)/(n/2*n/2))))
			}
		}
		image, err := NewImage([3]int{n, n, 1}, data, render.Vec3{X: 0.001, Y: -0.002}, render.Vec3{X: 3, Y: 2, Z: 1}, Identity3())
		if err != nil {
			t.Fatal(err)
		}
		return image
	}
	return make2D(0), make2D(5)
}

// checkDerivative compares each analytic partial of the in-plane affine
// parameters with ITK's fourth-order central difference (step 1e-5) and
// returns the worst relative error.
func checkDerivative(t *testing.T, m Metric, transform Transform, parameters []int) float64 {
	t.Helper()
	value, derivative, err := m.ValueAndDerivative(transform)
	if err != nil {
		t.Fatal(err)
	}
	if only, err := m.Value(transform); err != nil || math.Abs(only-value) > 1e-12*math.Max(1, math.Abs(value)) {
		t.Fatalf("Value %v differs from ValueAndDerivative %v (%v)", only, value, err)
	}
	const delta = 1e-5
	at := func(p int, step float64) float64 {
		shifted := transform.Clone()
		values := shifted.Parameters()
		values[p] += step
		if err := shifted.SetParameters(values); err != nil {
			t.Fatal(err)
		}
		v, err := m.Value(shifted)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	worst := 0.0
	for _, p := range parameters {
		numeric := (-at(p, 2*delta)/12 + at(p, -2*delta)/12 + 2*at(p, delta)/3 - 2*at(p, -delta)/3) / delta
		ratio := derivative[p] / numeric
		t.Logf("parameter %d: analytic %.9g numeric %.9g ratio %.6f", p, derivative[p], numeric, ratio)
		worst = math.Max(worst, math.Abs(ratio-1))
	}
	return worst
}

// affine2DParameters are the in-plane parameters of the 3D affine: the
// 2×2 matrix and the x, y translations.
var affine2DParameters = []int{0, 1, 3, 4, 9, 10}

func TestMeanSquaresDerivativeMatchesFiniteDifferences(t *testing.T) {
	fixed, moving := itkGaussians(t)
	metric, err := NewMeanSquares(fixed, moving, MetricDomain{})
	if err != nil {
		t.Fatal(err)
	}
	if worst := checkDerivative(t, metric, NewAffine(), affine2DParameters); worst > 0.014 {
		t.Fatalf("worst derivative ratio error %.4f", worst)
	}
	// Identical images: zero at the identity.
	same, _ := NewMeanSquares(fixed, fixed, MetricDomain{})
	if v, err := same.Value(NewAffine()); err != nil || v != 0 {
		t.Fatalf("identical images %v, %v", v, err)
	}
}

func TestCorrelationDerivativeMatchesFiniteDifferences(t *testing.T) {
	fixed, moving := itkGaussians(t)
	metric, err := NewCorrelation(fixed, moving, MetricDomain{})
	if err != nil {
		t.Fatal(err)
	}
	if worst := checkDerivative(t, metric, NewAffine(), affine2DParameters); worst > 0.014 {
		t.Fatalf("worst derivative ratio error %.4f", worst)
	}
	// A linear intensity change is a perfect match.
	scaled := make([]float32, len(fixed.Data()))
	for i, v := range fixed.Data() {
		scaled[i] = 3*v + 7
	}
	linear, _ := NewImage(fixed.Size(), scaled, fixed.Origin(), fixed.Spacing(), fixed.Direction())
	same, _ := NewCorrelation(fixed, linear, MetricDomain{})
	if v, err := same.Value(NewAffine()); err != nil || math.Abs(v+1) > 1e-9 {
		t.Fatalf("linearly related images %v, %v", v, err)
	}
}

// TestMattesMatchesITK: the values around the identity match the known
// values of ITK's itkMattesMutualInformationImageToImageMetricv4Test (its
// last perturbed parameter, the y translation), and
// the derivative matches finite differences, densely and on every fifth
// fixed sample.
func TestMattesMatchesITK(t *testing.T) {
	fixed, moving := itkGaussians(t)
	metric, err := NewMattesMutualInformation(fixed, moving, MetricDomain{}, 50)
	if err != nil {
		t.Fatal(err)
	}
	known := map[float64]float64{1e-5: -1.297994750907097, -1e-5: -1.297995377157083, 2e-5: -1.297994448222851, -2e-5: -1.297995700722833}
	for step, want := range known {
		transform := NewAffine()
		parameters := transform.Parameters()
		parameters[10] += step
		if err := transform.SetParameters(parameters); err != nil {
			t.Fatal(err)
		}
		got, err := metric.Value(transform)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("translation %+g: %.15f, ITK %.15f", step, got, want)
		if math.Abs(got-want) > 1e-12 {
			t.Errorf("translation %+g: value %.15f, ITK %.15f", step, got, want)
		}
	}
	if worst := checkDerivative(t, metric, NewAffine(), affine2DParameters); worst > 0.014 {
		t.Fatalf("dense: worst derivative ratio error %.4f", worst)
	}
	var points []render.Vec3
	for i := 0; i < len(fixed.Data()); i += 5 {
		points = append(points, fixed.Point(render.Vec3{X: float64(i % 100), Y: float64(i / 100)}))
	}
	sampled, err := NewMattesMutualInformation(fixed, moving, MetricDomain{Points: points}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if worst := checkDerivative(t, sampled, NewAffine(), affine2DParameters); worst > 0.075 {
		t.Fatalf("sampled: worst derivative ratio error %.4f", worst)
	}
}

// TestMetricsHonourMasksAndWorkers: masks restrict the samples (a fixed
// mask of one half halves the count's effect), results do not depend on
// the number of workers, and a domain without overlap is refused.
func TestMetricsHonourMasksAndWorkers(t *testing.T) {
	fixed, moving := itkGaussians(t)
	half := make([]float32, 100*100)
	for i := range half {
		if i%100 < 50 {
			half[i] = 1
		}
	}
	mask, _ := NewImage(fixed.Size(), half, fixed.Origin(), fixed.Spacing(), fixed.Direction())
	transform := NewAffine()
	_ = transform.SetTranslation(render.Vec3{X: 1.3, Y: -0.7})
	for name, build := range map[string]func(MetricDomain) (Metric, error){
		"mean squares": func(d MetricDomain) (Metric, error) { return NewMeanSquares(fixed, moving, d) },
		"correlation":  func(d MetricDomain) (Metric, error) { return NewCorrelation(fixed, moving, d) },
		"mattes": func(d MetricDomain) (Metric, error) {
			return NewMattesMutualInformation(fixed, moving, d, 32)
		},
	} {
		one, _ := build(MetricDomain{Workers: 1})
		many, _ := build(MetricDomain{Workers: 7})
		v1, d1, err1 := one.ValueAndDerivative(transform)
		v7, d7, err7 := many.ValueAndDerivative(transform)
		if err1 != nil || err7 != nil || v1 != v7 {
			t.Fatalf("%s: workers change the value: %v %v (%v %v)", name, v1, v7, err1, err7)
		}
		for p := range d1 {
			if d1[p] != d7[p] {
				t.Fatalf("%s: workers change derivative %d", name, p)
			}
		}
		masked, _ := build(MetricDomain{FixedMask: mask, MovingMask: mask})
		vm, err := masked.Value(transform)
		if err != nil || vm == v1 {
			t.Fatalf("%s: masks ignored: %v %v", name, vm, err)
		}
	}
	// A moving image far away leaves no overlap.
	far, _ := NewImage(moving.Size(), moving.Data(), render.Vec3{X: 1e5}, moving.Spacing(), moving.Direction())
	ms, _ := NewMeanSquares(fixed, far, MetricDomain{})
	if _, err := ms.Value(NewAffine()); !errors.Is(err, ErrTooFewSamples) {
		t.Fatalf("no overlap: %v", err)
	}
	constant := make([]float32, 100*100)
	flat, _ := NewImage(fixed.Size(), constant, fixed.Origin(), fixed.Spacing(), fixed.Direction())
	if _, err := NewMattesMutualInformation(fixed, flat, MetricDomain{}, 0); !errors.Is(err, ErrImage) {
		t.Fatalf("constant moving image: %v", err)
	}
}

func TestMattesMaskPhysicalIndexOverflow(t *testing.T) {
	voxels := []float32{1, 2}
	size := [3]int{2, 1, 1}
	spacing := render.Vec3{X: 1, Y: 1, Z: 1}
	fixed, err := NewImage(size, voxels, render.Vec3{X: math.MaxFloat64}, spacing, Identity3())
	if err != nil {
		t.Fatal(err)
	}
	mask, err := NewImage(size, voxels, render.Vec3{X: -math.MaxFloat64}, spacing, Identity3())
	if err != nil {
		t.Fatal(err)
	}
	metric, err := NewMattesMutualInformation(fixed, fixed, MetricDomain{FixedMask: mask, Workers: 1}, 32)
	if metric != nil || !errors.Is(err, ErrImage) {
		t.Fatalf("expected no samples inside the mask, got metric %v, err %v", metric, err)
	}
}

func TestInsideMaskNearestSampleBoundaries(t *testing.T) {
	mask, err := NewImage([3]int{2, 1, 1}, []float32{1, 0}, render.Vec3{}, render.Vec3{X: 1, Y: 1, Z: 1}, Identity3())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		x    float64
		want bool
	}{
		{math.Nextafter(-0.5, math.Inf(-1)), false},
		{-0.5, true},
		{0.25, true},
		{0.5, false},
		{1.5, false},
		{math.NaN(), false},
	} {
		if got := mask.insideMask(render.Vec3{X: tc.x}); got != tc.want {
			t.Errorf("x=%v: inside mask %t, want %t", tc.x, got, tc.want)
		}
	}
}

// TestMetricsIn3DWithARigidTransform: on an anisotropic, oblique 3D volume
// the derivatives with respect to the Euler angles and translation match
// finite differences for every metric.
func TestMetricsIn3DWithARigidTransform(t *testing.T) {
	const n = 24
	direction := Matrix3{{math.Cos(0.3), -math.Sin(0.3), 0}, {math.Sin(0.3), math.Cos(0.3), 0}, {0, 0, 1}}
	blob := func(shift float64) *Image {
		data := make([]float32, n*n*n)
		for z := 0; z < n; z++ {
			for y := 0; y < n; y++ {
				for x := 0; x < n; x++ {
					dx, dy, dz := float64(x)-12+shift, float64(y)-11, (float64(z)-12)*1.5
					data[(z*n+y)*n+x] = float32(1000 * math.Exp(-(dx*dx+dy*dy*1.3+dz*dz)/40))
				}
			}
		}
		image, err := NewImage([3]int{n, n, n}, data, render.Vec3{X: -10, Y: 4, Z: 2}, render.Vec3{X: 1.2, Y: 0.9, Z: 2}, direction)
		if err != nil {
			t.Fatal(err)
		}
		return image
	}
	fixed, moving := blob(0), blob(1.5)
	transform := NewEuler3D()
	_ = transform.SetCenter(fixed.Point(render.Vec3{X: 12, Y: 12, Z: 12}))
	parameters := transform.Parameters()
	copy(parameters, []float64{0.02, -0.03, 0.05, 0.4, -0.2, 0.3})
	if err := transform.SetParameters(parameters); err != nil {
		t.Fatal(err)
	}
	// Points near the border would cross the moving buffer under the
	// perturbations, which makes any of these metrics jump: compare the
	// interior only.
	interior := make([]float32, n*n*n)
	for z := 4; z < n-4; z++ {
		for y := 4; y < n-4; y++ {
			for x := 4; x < n-4; x++ {
				interior[(z*n+y)*n+x] = 1
			}
		}
	}
	mask, _ := NewImage(fixed.Size(), interior, fixed.Origin(), fixed.Spacing(), fixed.Direction())
	domain := MetricDomain{FixedMask: mask}
	mattes, _ := NewMattesMutualInformation(fixed, moving, domain, 0)
	ms, _ := NewMeanSquares(fixed, moving, domain)
	nc, _ := NewCorrelation(fixed, moving, domain)
	for name, m := range map[string]Metric{"mattes": mattes, "mean squares": ms, "correlation": nc} {
		if worst := checkDerivative(t, m, transform, []int{0, 1, 2, 3, 4, 5}); worst > 0.002 {
			t.Fatalf("%s: worst derivative ratio error %.4f", name, worst)
		}
	}
}

// BenchmarkMetrics128 evaluates each metric and its derivative for a
// rigid transform on 128³ volumes.
func BenchmarkMetrics128(b *testing.B) {
	const n = 128
	volume := func(shift float64) *Image {
		data := make([]float32, n*n*n)
		for i := range data {
			x, y, z := float64(i%n)-64+shift, float64(i/n%n)-64, float64(i/(n*n))-64
			data[i] = float32(1000 * math.Exp(-(x*x+y*y+z*z)/800))
		}
		image, _ := NewImage([3]int{n, n, n}, data, render.Vec3{}, render.Vec3{X: 1, Y: 1, Z: 1}, Identity3())
		return image
	}
	fixed, moving := volume(0), volume(3)
	transform := NewEuler3D()
	_ = transform.SetCenter(render.Vec3{X: 64, Y: 64, Z: 64})
	parameters := transform.Parameters()
	copy(parameters, []float64{0.02, -0.01, 0.03, 1, 0.5, -0.5})
	_ = transform.SetParameters(parameters)
	mattes, _ := NewMattesMutualInformation(fixed, moving, MetricDomain{}, 0)
	ms, _ := NewMeanSquares(fixed, moving, MetricDomain{})
	nc, _ := NewCorrelation(fixed, moving, MetricDomain{})
	for _, bench := range []struct {
		name string
		m    Metric
	}{{"MeanSquares", ms}, {"Correlation", nc}, {"Mattes", mattes}} {
		b.Run(bench.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, _, err := bench.m.ValueAndDerivative(transform); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
