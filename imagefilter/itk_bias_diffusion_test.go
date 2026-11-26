package imagefilter

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"
)

// ITK's N4 MaskLabelMatchesBinaryMask compares equivalent support. Exercise
// that property in incomplete shrink blocks and verify how the fitted field
// corrects the entire input grid.
func TestITKN4MaskSupportAndPartialBlocks(t *testing.T) {
	for _, tc := range []struct {
		name       string
		depth      int
		shrink     int
		shrunken   [3]int
		wantBlocks int
	}{
		{"plane_full", 1, 1, [3]int{9, 7, 1}, 4},
		{"plane_partial", 1, 4, [3]int{3, 2, 1}, 3},
		{"volume_full", 5, 1, [3]int{9, 7, 5}, 6},
		{"volume_partial", 5, 4, [3]int{3, 2, 2}, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := NewVolume([3]int{9, 7, tc.depth}, [3]float64{0.75, 1.5, 2.5})
			for i := range input.Data {
				input.Data[i] = 75
			}
			locations := [][3]int{{0, 0, 0}, {3, 3, 0}, {4, 0, 0}, {8, 6, 0}}
			if tc.depth > 1 {
				locations = append(locations, [3]int{0, 0, 3}, [3]int{8, 6, 4})
			}
			positiveMask := make([]bool, len(input.Data))
			for j, p := range locations {
				i := input.Index(p[0], p[1], p[2])
				positiveMask[i] = true
				input.Data[i] = float32(90 + 7*j)
			}
			base, err := N4BiasFieldCorrection(context.Background(), input, N4Options{
				Mask: positiveMask, ShrinkFactor: tc.shrink, Iterations: []int{3, 2},
			})
			if err != nil {
				t.Fatal(err)
			}
			if base.ShrunkenDims != tc.shrunken || base.Samples != tc.wantBlocks {
				t.Fatalf("shrunken grid %v with %d occupied cells, want %v with %d", base.ShrunkenDims, base.Samples, tc.shrunken, tc.wantBlocks)
			}

			changed := input.Clone()
			changedMask := slices.Clone(positiveMask)
			negative := changed.Index(1, 5, 0)
			zero := changed.Index(2, 5, 0)
			outlier := changed.Index(8, 1, tc.depth-1)
			changed.Data[negative], changed.Data[zero], changed.Data[outlier] = -32, 0, 1e5
			changedMask[negative], changedMask[zero] = true, true
			got, err := N4BiasFieldCorrection(context.Background(), changed, N4Options{
				Mask: changedMask, ShrinkFactor: tc.shrink, Iterations: []int{3, 2},
			})
			if err != nil {
				t.Fatal(err)
			}
			if got.Samples != base.Samples || !slices.Equal(got.LogBiasField.Data, base.LogBiasField.Data) {
				t.Fatal("masked intensity or selected nonpositive voxel changed the fit")
			}
			if got.Corrected.Dims != changed.Dims || got.Corrected.SpacingMM != changed.SpacingMM {
				t.Fatal("correction did not preserve the full grid")
			}
			if got.Corrected.Data[outlier] == changed.Data[outlier] || got.Corrected.Data[negative] >= 0 || got.Corrected.Data[zero] != 0 {
				t.Fatal("expected full-grid correction with the input sign preserved")
			}
			for i, value := range changed.Data {
				want := float64(value) * math.Exp(-float64(got.LogBiasField.Data[i]))
				if delta := math.Abs(float64(got.Corrected.Data[i]) - want); !(delta <= 2e-7*math.Abs(want)) {
					t.Fatalf("voxel %d: corrected %g, want %g from the reported field", i, got.Corrected.Data[i], want)
				}
			}
		})
	}
}

// ITK's N4 driver sets per-level caps and its weighted-mask GTest verifies
// convergence. Check the public report against the iterations Go publishes.
func TestITKN4ConvergenceAndLevelSchedule(t *testing.T) {
	input, _, _ := n4Phantom([3]int{23, 21, 9}, n4SurfaceCoil, 0)
	for _, tc := range []struct {
		name      string
		threshold float64
		schedule  []int
		want      []int
	}{
		{"converges_each_level", 1, []int{5, 4, 3}, []int{1, 1, 1}},
		{"respects_iteration_caps", 1e-15, []int{2, 4, 1}, []int{2, 4, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var progress []N4Progress
			got, err := N4BiasFieldCorrection(context.Background(), input, N4Options{
				ShrinkFactor: 2, Iterations: tc.schedule, ConvergenceThreshold: tc.threshold,
				Progress: func(p N4Progress) { progress = append(progress, p) },
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Levels) != len(tc.want) {
				t.Fatalf("completed %d levels, want %d", len(got.Levels), len(tc.want))
			}
			var seen int
			for level, count := range tc.want {
				if got.Levels[level].Iterations != count || len(progress)-seen < count {
					t.Fatalf("level %d: want %d published iterations, got %+v with %d progress events remaining", level, count, got.Levels[level], len(progress)-seen)
				}
				for i, p := range progress[seen : seen+count] {
					if p.Level != level || p.Levels != len(tc.want) || p.Iteration != i+1 || !(p.Convergence >= 0 && p.Convergence <= math.MaxFloat64) {
						t.Fatalf("unexpected progress at level %d iteration %d: %+v", level, i+1, p)
					}
					if i+1 < count && p.Convergence <= tc.threshold {
						t.Fatalf("level %d continued after reaching threshold at iteration %d", level, i+1)
					}
				}
				seen += count
				if final := progress[seen-1].Convergence; final != got.Levels[level].Convergence || count < tc.schedule[level] && final > tc.threshold {
					t.Fatalf("level %d reported convergence %g, last event %g, threshold %g", level, got.Levels[level].Convergence, final, tc.threshold)
				}
			}
			if seen != len(progress) {
				t.Fatalf("received %d extra progress events", len(progress)-seen)
			}
		})
	}
}

// The ITK gradient test exercises one explicit step. A quadratic profile
// gives an analytic Perona-Malik flux at both the interior and zero-flux ends.
func TestITKGradientDiffusionOneStep(t *testing.T) {
	for _, tc := range []struct {
		name        string
		axis        int
		spacing     float64
		conductance float64
	}{
		{"x_weak_conductance", 0, 0.5, 0.5},
		{"y_unit_conductance", 1, 1, 1},
		{"z_strong_conductance", 2, 2.5, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dims, spacing := [3]int{1, 1, 1}, [3]float64{}
			dims[tc.axis], spacing[tc.axis] = 5, tc.spacing
			in := NewVolume(dims, spacing)
			copy(in.Data, []float32{0, 1, 4, 9, 16})
			before := slices.Clone(in.Data)
			got, err := GradientAnisotropicDiffusion(context.Background(), in, AnisotropicDiffusionOptions{
				Iterations: 1, Conductance: tc.conductance, TimeStep: tc.spacing * tc.spacing / 4,
			})
			if err != nil {
				t.Fatal(err)
			}
			// Centred differences are 2/h, 4/h and 6/h. Face differences
			// are 1, 3, 5 and 7. The factors of h cancel with the time step.
			k := 2 * tc.conductance * tc.conductance * (4.0 + 16 + 36) / 3
			flux := func(d float64) float64 { return d * math.Exp(-d*d/k) }
			wantDelta := [...]float64{flux(1), flux(3) - flux(1), flux(5) - flux(3), flux(7) - flux(5), -flux(7)}
			for i, delta := range wantDelta {
				want := float32(float64(before[i]) + delta/4)
				if delta := math.Abs(float64(got.Data[i] - want)); !(delta <= 1e-6) {
					t.Fatalf("voxel %d: updated value %g, analytic value %g", i, got.Data[i], want)
				}
			}
			if !slices.Equal(in.Data, before) || got.Dims != dims || got.SpacingMM != spacing {
				t.Fatal("diffusion changed the input or grid geometry")
			}
		})
	}
}

// A symmetric valley and its intensity reflection exercise both signs of
// the MCDE speed and the corresponding upwind derivative at the extremum.
func TestITKCurvatureDiffusionOneStep(t *testing.T) {
	for _, tc := range []struct {
		name    string
		axis    int
		spacing float64
		sign    float64
	}{
		{"valley_along_x", 0, 0.5, 1},
		{"ridge_along_y", 1, 1.25, -1},
		{"valley_along_z", 2, 2.5, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dims, spacing := [3]int{1, 1, 1}, [3]float64{}
			dims[tc.axis], spacing[tc.axis] = 5, tc.spacing
			in := NewVolume(dims, spacing)
			for i, value := range [...]float64{4, 1, 0, 1, 4} {
				in.Data[i] = float32(8 + tc.sign*value)
			}
			got, err := CurvatureAnisotropicDiffusion(context.Background(), in, AnisotropicDiffusionOptions{
				Iterations: 1, Conductance: 1, TimeStep: tc.spacing * tc.spacing / 8,
			})
			if err != nil {
				t.Fatal(err)
			}
			// Centred gradients are -2/h, 0 and 2/h. Each nonzero
			// face contributes a unit normal times its conductance.
			// The small normal regularizer is below the test precision.
			k := 16.0 / 3
			n1, n3 := math.Exp(-1/k), math.Exp(-9/k)
			boundary := -3 * n3 / 8
			shoulder := (n3 - n1) / 8
			extremum := math.Sqrt2 * n1 / 4
			for i, delta := range [...]float64{boundary, shoulder, extremum, shoulder, boundary} {
				want := float32(float64(in.Data[i]) + tc.sign*delta)
				if deviation := math.Abs(float64(got.Data[i] - want)); !(deviation <= 1e-6) {
					t.Fatalf("voxel %d: updated value %g, analytic value %g", i, got.Data[i], want)
				}
			}
		})
	}
}

// Like itkGradientAnisotropicDiffusionImageFilterTest2, compare the same
// image after scaling spacing and time. The Go time unit is mm².
func TestITKDiffusionPhysicalScaleAndStability(t *testing.T) {
	for _, algorithm := range []struct {
		name         string
		apply        func(context.Context, *Volume, AnisotropicDiffusionOptions) (*Volume, error)
		stabilityDiv float64
	}{
		{"gradient", GradientAnisotropicDiffusion, 2},
		{"curvature", CurvatureAnisotropicDiffusion, 4},
	} {
		for _, geometry := range []struct {
			name    string
			dims    [3]int
			spacing [3]float64
			invH2   float64
		}{
			{"plane", [3]int{7, 5, 1}, [3]float64{0.5, 1, 0}, 5},
			{"volume", [3]int{7, 5, 3}, [3]float64{0.5, 1, 2}, 5.25},
		} {
			t.Run(algorithm.name+"/"+geometry.name, func(t *testing.T) {
				in := NewVolume(geometry.dims, geometry.spacing)
				for z := 0; z < in.Dims[2]; z++ {
					for y := 0; y < in.Dims[1]; y++ {
						for x := 0; x < in.Dims[0]; x++ {
							in.Data[in.Index(x, y, z)] = float32((x-3)*(x-3) + y*y - 4*z + x*y - y*z)
						}
					}
				}
				ctx := context.Background()
				stable := 1 / (algorithm.stabilityDiv * geometry.invH2)
				opts := AnisotropicDiffusionOptions{Iterations: 4, TimeStep: stable, Conductance: 1.5}
				base, err := algorithm.apply(ctx, in, opts)
				if err != nil {
					t.Fatalf("time step at stability bound: %v", err)
				}
				if slices.Equal(in.Data, base.Data) {
					t.Fatal("the test profile must respond to diffusion")
				}
				scaled := in.Clone()
				for axis := range scaled.SpacingMM {
					scaled.SpacingMM[axis] *= 2
				}
				opts.TimeStep *= 4
				got, err := algorithm.apply(ctx, scaled, opts)
				if err != nil {
					t.Fatalf("rescaled time step at stability bound: %v", err)
				}
				for i, want := range base.Data {
					if delta := math.Abs(float64(got.Data[i] - want)); !(delta <= 2e-5) {
						t.Fatalf("voxel %d: spacing and time scaling changed %g to %g", i, want, got.Data[i])
					}
				}
				opts.TimeStep *= 1.001
				if got, err := algorithm.apply(ctx, scaled, opts); got != nil || !errors.Is(err, ErrInvalidVolume) {
					t.Fatalf("above the scaled stability bound: result present %t, error %v", got != nil, err)
				}
			})
		}
	}
}
