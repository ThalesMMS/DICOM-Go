package registration

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

// BenchmarkSyN64 is one complete SyN run on a 64³ phantom pair (two levels,
// ten iterations each, radius two), with and without membrane
// regularization; it includes both final inversions and certificates.
func BenchmarkSyN64(b *testing.B) {
	f, m := synPhantomPair(b, 64, 3)
	for _, regularization := range []float64{.1, 0} {
		name := "membrane"
		if regularization == 0 {
			name = "unregularized"
		}
		b.Run(name, func(b *testing.B) {
			o := SyNOptions{
				Levels:         []SyNLevel{{Shrink: 2, Iterations: 10}, {Shrink: 1, Iterations: 10}},
				Correlation:    NeighborhoodCorrelationOptions{Radius: [3]int{2, 2, 2}},
				LearningRateMM: .3, UpdateSigmaMM: .8, Regularization: regularization, MinimumImprovement: 1e-9,
				MaxWork: 1e11,
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r, err := SyN(context.Background(), f, m, o)
				if err != nil || r.FixedToMoving == nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkMaskedFFTCorrelation is one complete masked-FFT NCC estimate:
// 64³ volumes pad to 128³ (2^21 voxels), 512² images to 1024² (2^20).
func BenchmarkMaskedFFTCorrelation(b *testing.B) {
	for _, size := range [][3]int{{64, 64, 64}, {512, 512, 1}} {
		b.Run(fmt.Sprintf("%dx%dx%d", size[0], size[1], size[2]), func(b *testing.B) {
			f, m, fm, mm := maskedFFTCostFixture(b, size)
			o := MaskedFFTOptions{MinOverlap: 12, MinOverlapFraction: .2}
			want, err := EstimateMaskedFFTTranslation(context.Background(), f, m, fm, mm, o)
			if err != nil {
				b.Fatal(want, err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r, err := EstimateMaskedFFTTranslation(context.Background(), f, m, fm, mm, o)
				if err != nil || r != want {
					b.Fatal(r, err)
				}
			}
		})
	}
}

// BenchmarkMattesBSpline64Derivative evaluates Mattes MI and its derivative
// for a 24³-mesh B-spline (local support, 59,049 parameters) on 64³ images.
func BenchmarkMattesBSpline64Derivative(b *testing.B) {
	const n = 64
	value := func(x, y, z float64) float32 {
		return float32(100 + 60*math.Sin(.21*x)*math.Cos(.17*y) + 40*math.Sin(.13*z+.05*x))
	}
	f := neighborhoodTestImage(b, [3]int{n, n, n}, render.Vec3{X: 1, Y: 1, Z: 1}, func(x, y, z int) float32 { return value(float64(x), float64(y), float64(z)) })
	m := neighborhoodTestImage(b, [3]int{n, n, n}, render.Vec3{X: 1, Y: 1, Z: 1}, func(x, y, z int) float32 {
		return value(float64(x)-.4, float64(y)+.3, float64(z)-.2)
	})
	transform, err := NewBSplineForImage(f, [3]int{24, 24, 24})
	if err != nil {
		b.Fatal(err)
	}
	parameters := make([]float64, transform.NumberOfParameters())
	for i := range parameters {
		parameters[i] = .05 * math.Sin(float64(i)*.37)
	}
	if err := transform.SetParameters(parameters); err != nil {
		b.Fatal(err)
	}
	metric, err := NewMattesMutualInformation(f, m, MetricDomain{}, 32)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := metric.ValueAndDerivative(transform); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDemonsDiffeomorphic runs ten diffeomorphic demons iterations on
// a 64³ phantom pair.
func BenchmarkDemonsDiffeomorphic(b *testing.B) {
	f, m := synPhantomPair(b, 64, 3)
	o := DemonsOptions{Iterations: 10, Diffeomorphic: true, UpdateSigma: 1}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Demons(context.Background(), f, m, o); err != nil {
			b.Fatal(err)
		}
	}
}
