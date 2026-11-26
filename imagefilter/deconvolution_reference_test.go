package imagefilter

import (
	"context"
	"math"
	"slices"
	"testing"
)

// The ITK Richardson-Lucy suite restores a known blur for one iteration
// with both Gaussian and irregular kernels. Here the finite forward matrix
// A is small enough to obtain exact values without another imaging runtime.
// Starting from any positive constant, the first update at pixel j is
// Σᵢ Aᵢⱼ*yᵢ/(row sum of Aᵢ) divided by the column sum of Aⱼ.
func TestITKReferenceRichardsonLucyOneIteration(t *testing.T) {
	ctx := context.Background()
	dims := [3]int{3, 3, 1}
	truth := psfTestVolume(dims, []float32{8, 0, 0, 0, 0, 0, 0, 0, 16})
	gaussian := psfTestVolume(dims, []float32{1, 2, 1, 2, 4, 2, 1, 2, 1})
	asymmetric := psfTestVolume(dims, []float32{1, 0, 0, 0, 4, 2, 0, 1, 0})
	for _, tc := range []struct {
		name     string
		kernel   *Volume
		mass     float64
		boundary PSFBoundary
		blurred  [9]float32
		oneStep  [9]float32
	}{
		{
			name: "gaussian-zero", kernel: gaussian, mass: 16, boundary: PSFZeroBoundary,
			blurred: [9]float32{2, 1, 0, 1, 1.5, 2, 0, 2, 4},
			oneStep: [9]float32{
				379.0 / 162, 175.0 / 108, 19.0 / 18,
				175.0 / 108, 49.0 / 24, 287.0 / 108,
				19.0 / 18, 287.0 / 108, 731.0 / 162,
			},
		},
		{
			name: "gaussian-circular", kernel: gaussian, mass: 16, boundary: PSFCircularBoundary,
			blurred: [9]float32{3, 2, 3, 2, 1.5, 2.5, 3, 2.5, 4.5},
			oneStep: [9]float32{
				43.0 / 16, 2.5, 45.0 / 16,
				2.5, 75.0 / 32, 85.0 / 32,
				45.0 / 16, 85.0 / 32, 97.0 / 32,
			},
		},
		{
			name: "asymmetric-zero", kernel: asymmetric, mass: 8, boundary: PSFZeroBoundary,
			blurred: [9]float32{4, 2, 0, 1, 2, 0, 0, 0, 8},
			oneStep: [9]float32{
				3308.0 / 735, 78.0 / 49, 0,
				4.0 / 3, 9.0 / 5, 40.0 / 21,
				0, 412.0 / 147, 54.0 / 7,
			},
		},
		{
			name: "asymmetric-circular", kernel: asymmetric, mass: 8, boundary: PSFCircularBoundary,
			blurred: [9]float32{4, 2, 2, 1, 2, 0, 4, 0, 9},
			oneStep: [9]float32{
				15.0 / 4, 9.0 / 4, 2,
				7.0 / 4, 1.5, 13.0 / 8,
				2.5, 21.0 / 8, 6,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := PSFDeconvolutionOptions{
				Iterations: 1, Boundary: tc.boundary, Normalization: PSFNormalizeMass,
				SourceReferences: []string{"test:two-emitters"},
			}
			blurred, err := ConvolvePSFContext(ctx, truth, tc.kernel, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(blurred.Data, tc.blurred[:]) {
				t.Fatalf("blurred samples %v, want %v", blurred.Data, tc.blurred)
			}
			observed := psfTestVolume(dims, tc.blurred[:])
			result, err := RichardsonLucy(ctx, observed, tc.kernel, opts)
			if err != nil {
				t.Fatal(err)
			}
			if result.Restored == nil {
				t.Fatal("missing restored image")
			}
			if result.Restored.Dims != observed.Dims || result.Restored.SpacingMM != observed.SpacingMM {
				t.Fatalf("restored dims %v and spacing %v, want %v and %v",
					result.Restored.Dims, result.Restored.SpacingMM, observed.Dims, observed.SpacingMM)
			}
			if got := len(result.Restored.Data); got != len(tc.oneStep) {
				t.Fatalf("restored sample count %d, want %d", got, len(tc.oneStep))
			}
			for i, want := range tc.oneStep {
				if got := result.Restored.Data[i]; !(math.Abs(float64(got-want)) <= 1e-6) {
					t.Fatalf("restored sample %d = %g, want %g", i, got, want)
				}
			}
			if got := result.Metadata; got.OriginalPSFMass != tc.mass || got.Boundary != tc.boundary ||
				got.IterationsCompleted != 1 || got.StoppingReason != "iteration-limit" {
				t.Fatalf("restoration metadata %+v", got)
			}
			if !(result.FinalPoissonDeviance < result.InitialPoissonDeviance) {
				t.Fatalf("one iteration changed Poisson deviance from %g to %g",
					result.InitialPoissonDeviance, result.FinalPoissonDeviance)
			}
		})
	}
}
