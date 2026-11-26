package registration

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/optimize"
	"github.com/ThalesMMS/dicom-go/render"
)

var mattesReplayParameters = [][6]float64{
	{},
	{0.005, -0.007, 0.003, 0.13, -0.09, 0.06},
	{-0.004, 0.002, 0.008, 0.26, -0.18, 0.12},
	{0.003, -0.002, -0.004, 0.39, -0.27, 0.18},
}

func mattesBenchmarkFixture(t testing.TB) (fixed, moving, fixedMask, movingMask *Image, points []render.Vec3) {
	t.Helper()
	size := [3]int{32, 32, 16}
	fixedData, movingData := make([]float32, 32*32*16), make([]float32, 32*32*16)
	fixedMaskData, movingMaskData := make([]float32, len(fixedData)), make([]float32, len(fixedData))
	phantom := func(x, y, z float64) float32 {
		// The synthetic CT fixture of the six-workload ITK comparison.
		first := (x-13)*(x-13)/25 + (y-17)*(y-17)/49 + (z-6)*(z-6)/9
		second := (x-23)*(x-23)/16 + (y-10)*(y-10)/25 + (z-11)*(z-11)/4
		return float32(math.Round(100+900*math.Exp(-first)+500*math.Exp(-second)+15*math.Sin(x*.31+y*.17+z*.47)) - 1024)
	}
	for z := 0; z < size[2]; z++ {
		for y := 0; y < size[1]; y++ {
			for x := 0; x < size[0]; x++ {
				index := (z*size[1]+y)*size[0] + x
				p := render.Vec3{X: float64(x), Y: float64(y), Z: float64(z)}
				fixedData[index] = phantom(p.X, p.Y, p.Z)
				movingData[index] = phantom(p.X-.4, p.Y+.3, p.Z-.2)
				if (x+y+z)%6 != 0 {
					points = append(points, p)
				}
				if x >= 2 && x < 30 && y >= 2 && y < 30 && z >= 2 && z < 14 {
					fixedMaskData[index] = 1
				}
				if x >= 4 && x < 28 && z >= 2 && z < 14 {
					movingMaskData[index] = 1
				}
			}
		}
	}
	image := func(data []float32) *Image {
		result, err := NewImage(size, data, render.Vec3{}, render.Vec3{X: 1, Y: 1, Z: 1}, Identity3())
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	return image(fixedData), image(movingData), image(fixedMaskData), image(movingMaskData), points
}

func mattesBenchmarkTransform(t testing.TB) *Euler3D {
	t.Helper()
	transform := NewEuler3D()
	if err := transform.SetCenter(render.Vec3{X: 15.5, Y: 15.5, Z: 7.5}); err != nil {
		t.Fatal(err)
	}
	return transform
}

func BenchmarkMattesReplay(b *testing.B) {
	fixed, moving, fixedMask, movingMask, points := mattesBenchmarkFixture(b)
	for _, workers := range []int{1, 4} {
		b.Run(fmt.Sprintf("workers_%d", workers), func(b *testing.B) {
			domain := MetricDomain{FixedMask: fixedMask, MovingMask: movingMask, Points: points, Workers: workers}
			metric, err := NewMattesMutualInformation(fixed, moving, domain, 32)
			if err != nil {
				b.Fatal(err)
			}
			var transforms []*Euler3D
			for _, parameters := range mattesReplayParameters {
				transform := mattesBenchmarkTransform(b)
				if err := transform.SetParameters(parameters[:]); err != nil {
					b.Fatal(err)
				}
				transforms = append(transforms, transform)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				for _, transform := range transforms {
					if _, _, err := metric.ValueAndDerivative(transform); err != nil {
						b.Fatal(err)
					}
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(len(transforms)), "evals/op")
		})
	}
}

func BenchmarkMattesRigidRegistration(b *testing.B) {
	fixed, moving, _, _, _ := mattesBenchmarkFixture(b)
	for _, profile := range []string{"historical", "multiresolution"} {
		for _, workers := range []int{1, 4} {
			b.Run(fmt.Sprintf("%s/workers_%d", profile, workers), func(b *testing.B) {
				method := Method{
					Fixed: fixed, Moving: moving, Transform: mattesBenchmarkTransform(b), Workers: workers,
					Levels: []Level{{Shrink: 1}},
					NewMetric: func(f, m *Image, d MetricDomain) (Metric, error) {
						return NewMattesMutualInformation(f, m, d, 32)
					},
					Optimizer: optimize.RegularStepGradientDescent{
						LearningRate: .25, MinimumStepLength: 1e-4, RelaxationFactor: .5,
						GradientMagnitudeTolerance: 1e-12, MaximumIterations: 40,
						Scales: []float64{1000, 1000, 1000, 1, 1, 1},
					},
				}
				if profile == "multiresolution" {
					method.Levels = []Level{{Shrink: 2, SmoothingSigmaMM: 4}, {Shrink: 1, SmoothingSigmaMM: 2}, {Shrink: 1}}
					method.Sampling, method.Seed = SampleRegular, 1087
					method.Optimizer = optimize.RegularStepGradientDescent{}
				}
				var result MethodResult
				b.ReportAllocs()
				b.ResetTimer()
				for iteration := 0; iteration < b.N; iteration++ {
					var err error
					result, err = method.Run(context.Background())
					if err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				evaluations, iterations := 0, 0
				for _, level := range result.Levels {
					evaluations += level.Evaluations
					iterations += level.Iterations
				}
				maxError := 0.0
				for _, z := range []float64{3, 7.5, 12} {
					for _, x := range []float64{5, 15.5, 26} {
						p := render.Vec3{X: x, Y: 15.5, Z: z}
						want := p.Add(render.Vec3{X: .4, Y: -.3, Z: .2})
						maxError = max(maxError, result.Transform.TransformPoint(p).Sub(want).Length())
					}
				}
				b.Logf("levels=%+v", result.Levels)
				b.ReportMetric(float64(evaluations), "evals/op")
				b.ReportMetric(float64(iterations), "iters/op")
				b.ReportMetric(maxError, "landmark-mm")
				if maxError > 0.15 {
					b.Fatalf("landmark error %g mm exceeds 0.15 mm", maxError)
				}
			})
		}
	}
}
