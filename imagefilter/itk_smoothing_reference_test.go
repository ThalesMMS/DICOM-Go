package imagefilter_test

import (
	"context"
	"math"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/imagefilter"
)

// gaussianImpulseWithReplicatedEnds computes a finite-sum approximation to
// the sampled Gaussian of one impulse on a constant-extended line. Twelve
// standard deviations leave a negligible omitted Gaussian tail.
func gaussianImpulseWithReplicatedEnds(length, impulse int, sigmaSamples float64) []float64 {
	result := make([]float64, length)
	if length == 1 || sigmaSamples == 0 {
		result[impulse] = 1
		return result
	}
	radius := int(math.Ceil(12 * sigmaSamples))
	kernel := make([]float64, 2*radius+1)
	var weightSum float64
	for i := range kernel {
		x := float64(i-radius) / sigmaSamples
		kernel[i] = math.Exp(-x * x / 2)
		weightSum += kernel[i]
	}
	for x := range result {
		for i, weight := range kernel {
			source := min(length-1, max(0, x+i-radius))
			if source == impulse {
				result[x] += weight / weightSum
			}
		}
	}
	return result
}

// The 2D/3D and non-unit sampling comparisons in ITK Smoothing use full
// output images. Boundary impulses exercise the sum of all off-grid
// contributions when the image is extended by its edge samples.
func TestITKReferenceGaussianBoundaryImpulse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dims    [3]int
		impulse [3]int
		spacing [3]float64
		sigma   [3]float64
	}{
		{"2D origin corner", [3]int{17, 22, 1}, [3]int{0, 0, 0}, [3]float64{.8, 1.5, 2.5}, [3]float64{1.6, 4.5, 3.75}},
		{"2D last row", [3]int{17, 22, 1}, [3]int{5, 21, 0}, [3]float64{.8, 1.5, 2.5}, [3]float64{1.6, 4.5, 3.75}},
		{"3D mixed corner", [3]int{17, 22, 15}, [3]int{0, 21, 14}, [3]float64{.8, 1.5, 2.5}, [3]float64{1.6, 4.5, 3.75}},
		{"3D face and unsmoothed row axis", [3]int{17, 22, 15}, [3]int{16, 8, 5}, [3]float64{.8, 1.5, 2.5}, [3]float64{1.6, 0, 3.75}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const baseline, contrast = -40, 120
			input := imagefilter.NewVolume(tc.dims, tc.spacing)
			for i := range input.Data {
				input.Data[i] = baseline
			}
			input.Data[input.Index(tc.impulse[0], tc.impulse[1], tc.impulse[2])] += contrast
			original := slices.Clone(input.Data)
			var axis [3][]float64
			for a := range axis {
				axis[a] = gaussianImpulseWithReplicatedEnds(tc.dims[a], tc.impulse[a], tc.sigma[a]/tc.spacing[a])
			}
			opts := imagefilter.RecursiveGaussianOptions{SigmaMM: tc.sigma, Workers: 1}
			got, err := imagefilter.SmoothRecursiveGaussian(context.Background(), input, opts)
			if err != nil {
				t.Fatal(err)
			}
			if got.Dims != tc.dims || got.SpacingMM != tc.spacing {
				t.Fatalf("output grid = %v at %v mm, want %v at %v mm", got.Dims, got.SpacingMM, tc.dims, tc.spacing)
			}
			opts.NormalizeAcrossScale = true
			normalized, err := imagefilter.SmoothRecursiveGaussian(context.Background(), input, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got.Data, normalized.Data) {
				t.Fatal("smoothing changed with scale normalization")
			}
			for z, wz := range axis[2] {
				for y, wy := range axis[1] {
					for x, wx := range axis[0] {
						want := baseline + contrast*wx*wy*wz
						// Allow the recursive approximation error across
						// three passes, scaled by the impulse contrast.
						if v := float64(got.At(x, y, z)); !(math.Abs(v-want) <= .0015*contrast) {
							t.Fatalf("output at (%d,%d,%d) = %.7g, sampled Gaussian %.7g", x, y, z, v, want)
						}
					}
				}
			}
			if !slices.Equal(input.Data, original) {
				t.Fatal("filter modified its input")
			}
			got.Data[0] = 123
			if !slices.Equal(input.Data, original) {
				t.Fatal("output aliases input")
			}
		})
	}
}
