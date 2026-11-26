package imagefilter_test

import (
	"errors"
	"math"
	"math/cmplx"
	"testing"

	"github.com/ThalesMMS/dicom-go/imagefilter"
)

func TestRecursiveGaussianPlanDerivativeMoments(t *testing.T) {
	// Closed-form moments of the infinite two-sided impulse response prove
	// constant preservation and derivative gain independently of a volume scan.
	for _, sigma := range []float64{.5, .75, 1, 4, 8, 32} {
		for order := 0; order <= 2; order++ {
			scale := math.Pow(sigma, float64(order))
			k, err := imagefilter.PlanRecursiveGaussian(sigma, order, scale)
			if err != nil {
				t.Fatal(err)
			}
			var moments [3]float64
			for _, p := range k.Poles {
				z, c, d := p.Pole, p.Causal, p.Anticausal
				if cmplx.Abs(z) >= 1 {
					t.Fatalf("unstable pole at sigma %v order %d: %v", sigma, order, z)
				}
				u := 1 - z
				moments[0] += real(c/u + d*z/u)
				moments[1] -= real((c - d) * z / (u * u))
				moments[2] += real((c + d) * z * (1 + z) / (u * u * u))
				if cmplx.Abs(p.CausalBoundary-c/u) > 1e-12 || cmplx.Abs(p.AnticausalBoundary-d*z/u) > 1e-12 {
					t.Fatal("replicated endpoint boundary does not match steady-state gain")
				}
			}
			want := scale
			if order == 2 {
				want *= 2
			}
			if math.Abs(moments[order]-want) > 1e-8*max(1, want) || order > 0 && math.Abs(moments[0]) > 1e-8 {
				t.Fatalf("sigma %v order %d: moments %v, gain want %v", sigma, order, moments, want)
			}
		}
	}
}

func TestRecursiveGaussianPlanRejectsInvalidMetadata(t *testing.T) {
	for _, parameters := range []struct {
		sigma, scale float64
		order        int
	}{
		{0, 1, 0}, {-1, 1, 0}, {math.NaN(), 1, 0}, {math.Inf(1), 1, 0},
		{1, math.Inf(1), 0}, {1, math.NaN(), 0}, {1, 1, -1}, {1, 1, 3},
	} {
		k, err := imagefilter.PlanRecursiveGaussian(parameters.sigma, parameters.order, parameters.scale)
		if !errors.Is(err, imagefilter.ErrInvalidVolume) || k != (imagefilter.RecursiveGaussianKernel{}) {
			t.Fatalf("invalid parameters published coefficients: %v %v", k, err)
		}
	}
}
