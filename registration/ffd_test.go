package registration

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

// ffdScene is a synthetic multimodal pair with a known deformation: the
// moving image is a field of Gaussian blobs g in a second "modality"
// (200 + 800·e^(−2g), a non-linear decreasing intensity map), and the
// fixed image samples g at T(x) = x + D(x), so the true fixed-to-moving
// mapping is T. D is a smooth sinusoidal field of the given amplitude in
// millimetres, outside the B-spline space the registration searches.
type ffdScene struct {
	origin, extent render.Vec3
	amplitude      float64
	blobs          []ffdBlob
}

type ffdBlob struct {
	centre     render.Vec3
	sigma, max float64
}

func newFFDScene(size [3]int, spacing float64, amplitude float64) ffdScene {
	extent := render.Vec3{X: float64(size[0]-1) * spacing, Y: float64(size[1]-1) * spacing, Z: float64(size[2]-1) * spacing}
	scene := ffdScene{origin: render.Vec3{X: -extent.X / 2, Y: -extent.Y / 2, Z: 10}, extent: extent, amplitude: amplitude}
	random := rand.New(rand.NewSource(42))
	volume := extent.X * extent.Y * math.Max(extent.Z, spacing)
	count := int(volume / 600)
	for i := 0; i < count; i++ {
		scene.blobs = append(scene.blobs, ffdBlob{
			centre: scene.origin.Add(render.Vec3{X: random.Float64() * extent.X, Y: random.Float64() * extent.Y, Z: random.Float64() * extent.Z}),
			sigma:  2.5 + 3*random.Float64(),
			max:    0.3 + 0.7*random.Float64(),
		})
	}
	return scene
}

func (s ffdScene) g(p render.Vec3) float64 {
	var v float64
	for _, b := range s.blobs {
		d := p.Sub(b.centre)
		v += b.max * math.Exp(-d.Dot(d)/(2*b.sigma*b.sigma))
	}
	return v
}

// displacement is D(x).
func (s ffdScene) displacement(p render.Vec3) render.Vec3 {
	u := p.Sub(s.origin)
	ux, uy, uz := u.X/s.extent.X, u.Y/s.extent.Y, 0.5
	if s.extent.Z > 0 {
		uz = u.Z / s.extent.Z
	}
	a := s.amplitude
	return render.Vec3{
		X: a * math.Sin(math.Pi*ux) * math.Cos(math.Pi*(uy-0.5)),
		Y: -a * math.Sin(math.Pi*uy) * math.Cos(1.3*math.Pi*(ux-0.4)),
		Z: 0.5 * a * math.Sin(math.Pi*uz) * math.Sin(math.Pi*ux),
	}
}

func ffdTestImages(t testing.TB, size [3]int, spacing float64, amplitude float64) (*Image, *Image) {
	t.Helper()
	fixed, moving, _ := ffdTestScene(t, size, spacing, amplitude)
	return fixed, moving
}

func ffdTestScene(t testing.TB, size [3]int, spacing float64, amplitude float64) (*Image, *Image, ffdScene) {
	t.Helper()
	scene := newFFDScene(size, spacing, amplitude)
	n := size[0] * size[1] * size[2]
	fixedData, movingData := make([]float32, n), make([]float32, n)
	sp := render.Vec3{X: spacing, Y: spacing, Z: spacing}
	fixed, err := NewImage(size, fixedData, scene.origin, sp, Identity3())
	if err != nil {
		t.Fatal(err)
	}
	moving, err := NewImage(size, movingData, scene.origin, sp, Identity3())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		p := fixed.Point(render.Vec3{X: float64(i % size[0]), Y: float64(i / size[0] % size[1]), Z: float64(i / (size[0] * size[1]))})
		fixedData[i] = float32(scene.g(p.Add(scene.displacement(p))))
		movingData[i] = float32(200 + 800*math.Exp(-2*scene.g(p)))
	}
	return fixed, moving, scene
}

// mappingError is the RMS and largest distance between t and the true
// mapping over the fixed samples at least margin samples inside the image.
func mappingError(fixed *Image, scene ffdScene, t Transform, margin int) (float64, float64) {
	size := fixed.Size()
	var sum, largest float64
	var count int
	for z := 0; z < size[2]; z++ {
		if size[2] > 1 && (z < margin || z >= size[2]-margin) {
			continue
		}
		for y := margin; y < size[1]-margin; y++ {
			for x := margin; x < size[0]-margin; x++ {
				p := fixed.Point(render.Vec3{X: float64(x), Y: float64(y), Z: float64(z)})
				d := t.TransformPoint(p).Sub(p.Add(scene.displacement(p))).Length()
				sum += d * d
				largest = math.Max(largest, d)
				count++
			}
		}
	}
	return math.Sqrt(sum / float64(count)), largest
}

func testFFD(fixed, moving *Image) FreeFormDeformation {
	return FreeFormDeformation{
		Fixed: fixed, Moving: moving,
		MeshSize: [3]int{3, 3, 2},
		Levels: []FFDLevel{
			{Level: Level{Shrink: 2, SmoothingSigmaMM: 2}},
			{Level: Level{Shrink: 1, SmoothingSigmaMM: 1}, RefineMesh: true},
		},
		Bins: 32,
	}
}

// TestFreeFormDeformationRecoversAKnownDeformation: a multimodal pair
// related by a 3 mm sinusoidal deformation (2 mm samples) is registered to
// well under a sample: the recovered mapping is within 0.5 mm RMS of the
// truth inside the image, down from about 2 mm without registration.
func TestFreeFormDeformationRecoversAKnownDeformation(t *testing.T) {
	if testing.Short() {
		t.Skip("registration run")
	}
	fixed, moving, scene := ffdTestScene(t, [3]int{40, 36, 20}, 2, 3)
	identity, _ := NewBSplineForImage(fixed, [3]int{1, 1, 1})
	beforeRMS, beforeMax := mappingError(fixed, scene, identity, 3)
	ffd := testFFD(fixed, moving)
	ffd.Sampling, ffd.Seed = SampleRandom, 7
	ffd.Levels[0].SamplingPercentage, ffd.Levels[1].SamplingPercentage = 0.5, 0.3
	result, err := ffd.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	afterRMS, afterMax := mappingError(fixed, scene, result.Transform, 3)
	for _, level := range result.Levels {
		t.Logf("level %+v: %d points, %d iterations, %d evaluations, value %.6f, %s", level.Level, level.Points, level.Iterations, level.Evaluations, level.Value, level.Stop)
	}
	t.Logf("mapping error: RMS %.3f mm, max %.3f mm before; RMS %.3f mm, max %.3f mm after", beforeRMS, beforeMax, afterRMS, afterMax)
	if beforeRMS < 1.5 {
		t.Fatalf("the synthetic deformation is too small (%.3f mm RMS)", beforeRMS)
	}
	if afterRMS > 0.5 || afterMax > 1.5 {
		t.Fatalf("recovered mapping error RMS %.3f mm, max %.3f mm", afterRMS, afterMax)
	}
	if result.BSpline.MeshSize() != [3]int{6, 6, 4} {
		t.Fatalf("final mesh %v", result.BSpline.MeshSize())
	}
	if result.Value >= result.Levels[0].Value {
		t.Logf("note: the last level's value %v is not below the first's %v (different smoothing)", result.Value, result.Levels[0].Value)
	}
}

// TestFreeFormDeformationIsDeterministic: a seeded, sampled run gives the
// same parameters twice and with any number of workers.
func TestFreeFormDeformationIsDeterministic(t *testing.T) {
	fixed, moving := ffdTestImages(t, [3]int{24, 22, 8}, 2.5, 2)
	run := func(workers int) []float64 {
		ffd := testFFD(fixed, moving)
		ffd.Sampling, ffd.Seed, ffd.Workers = SampleRandom, 3, workers
		ffd.Levels[0].SamplingPercentage, ffd.Levels[1].SamplingPercentage = 0.5, 0.5
		ffd.Optimizer.MaximumIterations = 8
		result, err := ffd.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return result.BSpline.Parameters()
	}
	first := run(1)
	for _, workers := range []int{1, 4} {
		again := run(workers)
		for i := range first {
			if first[i] != again[i] {
				t.Fatalf("workers %d: parameter %d is %v, then %v", workers, i, first[i], again[i])
			}
		}
	}
}

func TestFreeFormDeformationIsCancellable(t *testing.T) {
	fixed, moving := ffdTestImages(t, [3]int{24, 22, 8}, 2.5, 2)
	ctx, cancel := context.WithCancel(context.Background())
	ffd := testFFD(fixed, moving)
	evaluations := 0
	ffd.Progress = func(p Progress) {
		evaluations++
		if evaluations == 3 {
			cancel()
		}
	}
	if _, err := ffd.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled run: %v", err)
	}
	if evaluations != 3 {
		t.Fatalf("%d evaluations after cancellation", evaluations)
	}
}

// TestFreeFormDeformationAfterAnInitialTransform: with a known translation
// as the initial transform, the deformation is found on top of it, the
// result maps through both, and its displacement field resamples the
// moving image onto the fixed grid and writes a Deformable Spatial
// Registration grid.
func TestFreeFormDeformationAfterAnInitialTransform(t *testing.T) {
	if testing.Short() {
		t.Skip("registration run")
	}
	fixed, moving, scene := ffdTestScene(t, [3]int{32, 30, 12}, 2, 2.5)
	// Shift the moving image by (4, −3, 0) mm: moving'(y) = moving(y − s).
	shift := render.Vec3{X: 4, Y: -3}
	shifted, err := NewImage(moving.Size(), make([]float32, len(moving.Data())), moving.Origin().Add(shift), moving.Spacing(), moving.Direction())
	if err != nil {
		t.Fatal(err)
	}
	copy(shifted.Data(), moving.Data())
	initial := NewAffine()
	if err := initial.SetTranslation(shift); err != nil {
		t.Fatal(err)
	}
	ffd := testFFD(fixed, shifted)
	ffd.Initial = initial
	ffd.Regularization = 0.2
	result, err := ffd.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result.Transform.(*Composite); !ok {
		t.Fatalf("result transform %T", result.Transform)
	}
	// The deformation alone should match the scene's.
	rms, largest := mappingError(fixed, scene, result.BSpline, 3)
	t.Logf("deformation error RMS %.3f mm, max %.3f mm", rms, largest)
	if rms > 0.5 || largest > 1.2 {
		t.Fatalf("deformation error RMS %.3f mm, max %.3f mm", rms, largest)
	}
	field, err := result.DisplacementField(context.Background(), fixed.Grid())
	if err != nil {
		t.Fatal(err)
	}
	mapping, err := FieldMapping(field)
	if err != nil {
		t.Fatal(err)
	}
	resampled, err := Resample(context.Background(), shifted, fixed.Grid(), mapping, ResampleOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The resampled moving image is the fixed one in the other modality.
	var worst float64
	size := fixed.Size()
	for z := 2; z < size[2]-2; z++ {
		for y := 3; y < size[1]-3; y++ {
			for x := 3; x < size[0]-3; x++ {
				i := (z*size[1]+y)*size[0] + x
				want := 200 + 800*math.Exp(-2*float64(fixed.Data()[i]))
				worst = math.Max(worst, math.Abs(float64(resampled.Image.Data()[i])-want))
			}
		}
	}
	t.Logf("largest intensity difference after resampling: %.1f (range 200–1000)", worst)
	if worst > 120 {
		t.Fatalf("resampled moving image differs by up to %.1f", worst)
	}
	grid, err := field.Grid()
	if err != nil {
		t.Fatal(err)
	}
	if grid == nil {
		t.Fatal("no spatial registration grid")
	}
}

func TestFreeFormDeformationRejectsInvalidOptions(t *testing.T) {
	fixed, moving := ffdTestImages(t, [3]int{10, 10, 4}, 2, 1)
	for name, ffd := range map[string]FreeFormDeformation{
		"no moving":      {Fixed: fixed},
		"negative mesh":  {Fixed: fixed, Moving: moving, MeshSize: [3]int{-1, 2, 2}},
		"no level":       {Fixed: fixed, Moving: moving, Levels: []FFDLevel{}},
		"zero shrink":    {Fixed: fixed, Moving: moving, Levels: []FFDLevel{{}}},
		"regularization": {Fixed: fixed, Moving: moving, Regularization: -1},
	} {
		if _, err := ffd.Run(context.Background()); err == nil {
			t.Fatalf("%s: no error", name)
		}
	}
}

// BenchmarkMattesBSplineDerivative compares the two-pass local-support
// derivative with the dense bins × bins × parameters one on a 32×32×16
// image and a 4×4×2 mesh (735 parameters).
func BenchmarkMattesBSplineDerivative(b *testing.B) {
	fixed, moving := ffdTestImages(b, [3]int{32, 32, 16}, 2, 2)
	metric, err := NewMattesMutualInformation(fixed, moving, MetricDomain{}, 32)
	if err != nil {
		b.Fatal(err)
	}
	bspline, err := NewBSplineForImage(fixed, [3]int{4, 4, 2})
	if err != nil {
		b.Fatal(err)
	}
	for _, c := range []struct {
		name      string
		transform Transform
	}{{"local", bspline}, {"dense", denseOnly{bspline}}} {
		b.Run(c.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, _, err := metric.ValueAndDerivative(c.transform); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(bspline.NumberOfParameters()), "parameters")
		})
	}
}

func BenchmarkFreeFormDeformation(b *testing.B) {
	fixed, moving := ffdTestImages(b, [3]int{40, 36, 20}, 2, 3)
	ffd := testFFD(fixed, moving)
	ffd.Sampling, ffd.Seed = SampleRandom, 7
	ffd.Levels[0].SamplingPercentage, ffd.Levels[1].SamplingPercentage = 0.5, 0.3
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ffd.Run(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}
