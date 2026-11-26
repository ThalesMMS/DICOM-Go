package imagefilter

import (
	"context"
	"fmt"
	"math"
	"testing"
)

// Extend the physical-spacing coverage of itkGradientAnisotropicDiffusionImageFilterTest2
// to the point where a two-slice volume has no interior samples along Z.
func TestReferenceAcceptanceDiffusionOnTwoSliceVolumes(t *testing.T) {
	for _, algorithm := range []struct {
		name         string
		apply        func(context.Context, *Volume, AnisotropicDiffusionOptions) (*Volume, error)
		centreFactor float64
	}{
		{"gradient", GradientAnisotropicDiffusion, 2},
		{"curvature", CurvatureAnisotropicDiffusion, 2 * math.Sqrt2},
	} {
		t.Run(algorithm.name, func(t *testing.T) {
			// A flat extension in Z has no normal flux. Adding identical
			// slices therefore preserves the in-plane conductance and update.
			const step = 0.05
			profile := [...]float32{4, 1, 0, 1, 4}
			// The three interior X derivatives are -2, 0 and 2. Their
			// mean square is 8/3, giving K = 16/3 for unit conductance.
			// The valley centre receives equal contributions across two
			// faces of gradient magnitude 1. MCDE also scales by the
			// upwind gradient magnitude sqrt(2).
			wantCentre := step * algorithm.centreFactor * math.Exp(-3.0/16)
			for _, depth := range []int{1, 2, 3} {
				t.Run(fmt.Sprintf("replicated-depth-%d", depth), func(t *testing.T) {
					input := NewVolume([3]int{5, 5, depth}, [3]float64{1, 1, 2})
					for z := range depth {
						for y := range 5 {
							for x, value := range profile {
								input.Data[input.Index(x, y, z)] = value
							}
						}
					}
					got, err := algorithm.apply(context.Background(), input, AnisotropicDiffusionOptions{
						Iterations: 1, TimeStep: step, Conductance: 1,
					})
					if err != nil {
						t.Fatal(err)
					}
					for z := range depth {
						value := got.Data[got.Index(2, 2, z)]
						if !(math.Abs(float64(value)-wantCentre) <= 1e-6) {
							t.Errorf("centre voxel in slice %d = %g, want %g after one physical diffusion step", z, value, wantCentre)
						}
					}
				})
			}

			t.Run("diffusion-between-two-distinct-slices", func(t *testing.T) {
				input := NewVolume([3]int{5, 5, 2}, [3]float64{1, 1, 2})
				for i := 25; i < len(input.Data); i++ {
					input.Data[i] = 10
				}
				got, err := algorithm.apply(context.Background(), input, AnisotropicDiffusionOptions{
					Iterations: 1, TimeStep: step, Conductance: 1,
				})
				if err != nil {
					t.Fatal(err)
				}
				low := got.Data[got.Index(2, 2, 0)]
				high := got.Data[got.Index(2, 2, 1)]
				// The only interior face carries flux from the high slice to
				// the low slice. Opposite normals and equal spacings make
				// their updates symmetric within a single explicit step.
				if !(0 < low && low < high && high < 10 && math.Abs(float64(low)+float64(high)-10) <= 1e-6) {
					t.Errorf("slice centres after one diffusion step = %g and %g, want symmetric smoothing between 0 and 10", low, high)
				}
			})
		})
	}
}
