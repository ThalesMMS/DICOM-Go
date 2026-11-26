package imagefilter

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"slices"
	"testing"
	"time"
)

// noisyStep is a 2-D phantom: 0 left of column 32, 100 from it on, plus
// Gaussian noise of standard deviation sigma.
func noisyStep(size int, spacing, sigma float64, seed int64) *Volume {
	v := NewVolume([3]int{size, size, 1}, [3]float64{spacing, spacing, 0})
	rng := rand.New(rand.NewSource(seed))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			base := 0.0
			if x >= size/2 {
				base = 100
			}
			v.Data[v.Index(x, y, 0)] = float32(base + sigma*rng.NormFloat64())
		}
	}
	return v
}

// flatNoise is the standard deviation within the two flat halves, away
// from the step and the border.
func flatNoise(v *Volume) float64 {
	size := v.Dims[0]
	var sum, sum2 float64
	n := 0
	for y := 4; y < size-4; y++ {
		for _, band := range [2][2]int{{4, size/2 - 8}, {size/2 + 8, size - 4}} {
			for x := band[0]; x < band[1]; x++ {
				value := float64(v.At(x, y, 0))
				if x >= size/2 {
					value -= 100
				}
				sum += value
				sum2 += value * value
				n++
			}
		}
	}
	mean := sum / float64(n)
	return math.Sqrt(sum2/float64(n) - mean*mean)
}

// edgeWidth is the 10–90% rise, in samples, of the row-averaged profile.
func edgeWidth(v *Volume) float64 {
	size := v.Dims[0]
	profile := make([]float64, size)
	for x := 0; x < size; x++ {
		for y := 0; y < size; y++ {
			profile[x] += float64(v.At(x, y, 0)) / float64(size)
		}
	}
	cross := func(level float64) float64 {
		for x := 1; x < size; x++ {
			if profile[x-1] < level && profile[x] >= level {
				return float64(x-1) + (level-profile[x-1])/(profile[x]-profile[x-1])
			}
		}
		return math.NaN()
	}
	return cross(90) - cross(10)
}

func TestEdgePreservingFiltersKeepTheStepWhileRemovingNoise(t *testing.T) {
	src := noisyStep(96, 0.7, 10, 1103)
	noise, width := flatNoise(src), edgeWidth(src)
	gaussian, err := SmoothRecursiveGaussian(context.Background(), src, RecursiveGaussianOptions{SigmaMM: [3]float64{1.4, 1.4, 0}})
	if err != nil {
		t.Fatal(err)
	}
	diffusion, err := GradientAnisotropicDiffusion(context.Background(), src, AnisotropicDiffusionOptions{Iterations: 20})
	if err != nil {
		t.Fatal(err)
	}
	bilateral, err := Bilateral(context.Background(), src, BilateralOptions{SigmaMM: [3]float64{1.4, 1.4, 0}, RangeSigma: 30})
	if err != nil {
		t.Fatal(err)
	}
	curvature, err := CurvatureAnisotropicDiffusion(context.Background(), src, AnisotropicDiffusionOptions{Iterations: 40, Conductance: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("input: noise %.2f, edge %.2f px", noise, width)
	t.Logf("gaussian: noise %.2f, edge %.2f px", flatNoise(gaussian), edgeWidth(gaussian))
	t.Logf("diffusion: noise %.2f, edge %.2f px", flatNoise(diffusion), edgeWidth(diffusion))
	t.Logf("bilateral: noise %.2f, edge %.2f px", flatNoise(bilateral), edgeWidth(bilateral))
	t.Logf("curvature: noise %.2f, edge %.2f px", flatNoise(curvature), edgeWidth(curvature))
	for name, out := range map[string]*Volume{"diffusion": diffusion, "bilateral": bilateral, "curvature": curvature} {
		if got := flatNoise(out); got > noise/2.5 {
			t.Errorf("%s leaves noise %.2f of %.2f", name, got, noise)
		}
		if got := edgeWidth(out); got > width+0.5 || got >= edgeWidth(gaussian)-0.5 {
			t.Errorf("%s widens the edge to %.2f px (input %.2f, Gaussian %.2f)", name, got, width, edgeWidth(gaussian))
		}
	}
}

func TestAnisotropicDiffusionConservesMeanAndRespectsSpacing(t *testing.T) {
	src := noisyStep(40, 1, 5, 7)
	src.Dims = [3]int{40, 20, 2}
	src.SpacingMM = [3]float64{0.5, 1, 3}
	out, err := GradientAnisotropicDiffusion(context.Background(), src, AnisotropicDiffusionOptions{Iterations: 8})
	if err != nil {
		t.Fatal(err)
	}
	// Zero-flux borders: the explicit scheme moves intensity, never creates it.
	var before, after float64
	for i := range src.Data {
		before += float64(src.Data[i])
		after += float64(out.Data[i])
	}
	if math.Abs(before-after) > 1e-3*math.Abs(before) {
		t.Fatalf("mean changed from %.4f to %.4f", before, after)
	}
	if _, err := GradientAnisotropicDiffusion(context.Background(), src, AnisotropicDiffusionOptions{TimeStep: 1}); !errors.Is(err, ErrInvalidVolume) {
		t.Fatalf("unstable step: %v", err)
	}
	flat := NewVolume([3]int{8, 8, 1}, [3]float64{1, 1, 0})
	for i := range flat.Data {
		flat.Data[i] = 42
	}
	same, err := GradientAnisotropicDiffusion(context.Background(), flat, AnisotropicDiffusionOptions{})
	if err != nil || same.Data[27] != 42 {
		t.Fatalf("constant image: %v %v", same.Data[27], err)
	}
}

func TestAnisotropicDiffusionUsesBoundaryGradientWhenInteriorIsZero(t *testing.T) {
	input := NewVolume([3]int{4, 3, 3}, [3]float64{1, 1, 1})
	constant := input.Clone()
	for i := range input.Data {
		input.Data[i] = float32(i % 2)
		constant.Data[i] = 42
	}
	before := slices.Clone(input.Data)
	opts := AnisotropicDiffusionOptions{Iterations: 1, TimeStep: 0.05, Conductance: 3, Workers: 1}

	// Every X-line is [0, 1, 0, 1]. Its two centered interior derivatives
	// vanish. Extending the edges gives an absolute derivative of 1/2 at
	// each end, so the whole-volume mean square is 1/8 and K is 9/4.
	step := opts.TimeStep * math.Exp(-1/(9.0/4))
	for _, tc := range []struct {
		name     string
		diffuse  func(context.Context, *Volume, AnisotropicDiffusionOptions) (*Volume, error)
		end      float64
		interior float64
	}{
		{"gradient", GradientAnisotropicDiffusion, step, 2 * step},
		{"curvature", CurvatureAnisotropicDiffusion, step / math.Sqrt(1+1e-10), 2 * math.Sqrt2 * step / math.Sqrt(1+1e-10)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.diffuse(context.Background(), input, opts)
			if err != nil {
				t.Fatal(err)
			}
			want := [...]float32{float32(tc.end), float32(1 - tc.interior), float32(tc.interior), float32(1 - tc.end)}
			for i, value := range got.Data {
				if math.Abs(float64(value-want[i%4])) > 1e-7 {
					t.Fatalf("sample %d got %g, want %g", i, value, want[i%4])
				}
			}
			if !slices.Equal(before, input.Data) || got.Dims != input.Dims || got.SpacingMM != input.SpacingMM {
				t.Fatal("diffusion changed the input or grid geometry")
			}
			same, err := tc.diffuse(context.Background(), constant, opts)
			if err != nil || same == nil || !slices.Equal(same.Data, constant.Data) {
				t.Fatalf("constant volume changed: %v", err)
			}
		})
	}
}

// A linear ramp has straight level sets, so the MCDE leaves it unchanged
// away from the replicated borders; a noisy ramp is smoothed.
func TestCurvatureDiffusionKeepsRampsAndValidates(t *testing.T) {
	ramp := NewVolume([3]int{48, 24, 1}, [3]float64{0.6, 0.9, 0})
	rng := rand.New(rand.NewSource(11))
	noisy := ramp.Clone()
	for y := 0; y < 24; y++ {
		for x := 0; x < 48; x++ {
			value := float32(3*x + 2*y)
			ramp.Data[ramp.Index(x, y, 0)] = value
			noisy.Data[noisy.Index(x, y, 0)] = value + float32(4*rng.NormFloat64())
		}
	}
	// The replicated border bends the ramp's level sets and the change moves
	// inward one sample per iteration.
	out, err := CurvatureAnisotropicDiffusion(context.Background(), ramp, AnisotropicDiffusionOptions{Iterations: 4})
	if err != nil {
		t.Fatal(err)
	}
	for y := 5; y < 19; y++ {
		for x := 5; x < 43; x++ {
			if d := math.Abs(float64(out.At(x, y, 0) - ramp.At(x, y, 0))); d > 1e-3 {
				t.Fatalf("ramp changed by %.4f at (%d,%d)", d, x, y)
			}
		}
	}
	smooth, err := CurvatureAnisotropicDiffusion(context.Background(), noisy, AnisotropicDiffusionOptions{Iterations: 20, Conductance: 2})
	if err != nil {
		t.Fatal(err)
	}
	residual := func(v *Volume) float64 {
		sum := 0.0
		n := 0
		for y := 5; y < 19; y++ {
			for x := 5; x < 43; x++ {
				d := float64(v.At(x, y, 0) - ramp.At(x, y, 0))
				sum += d * d
				n++
			}
		}
		return math.Sqrt(sum / float64(n))
	}
	if before, after := residual(noisy), residual(smooth); after > before/1.5 {
		t.Fatalf("noisy ramp residual %.2f -> %.2f", before, after)
	}
	if _, err := CurvatureAnisotropicDiffusion(context.Background(), ramp, AnisotropicDiffusionOptions{TimeStep: 1}); !errors.Is(err, ErrInvalidVolume) {
		t.Fatalf("unstable step: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CurvatureAnisotropicDiffusion(ctx, ramp, AnisotropicDiffusionOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestUnsharpMaskSharpensEdgesAndKeepsRamps(t *testing.T) {
	ramp := NewVolume([3]int{64, 8, 1}, [3]float64{0.5, 0.5, 0})
	for y := 0; y < 8; y++ {
		for x := 0; x < 64; x++ {
			ramp.Data[ramp.Index(x, y, 0)] = float32(2 * x)
		}
	}
	out, err := UnsharpMask(context.Background(), ramp, UnsharpMaskOptions{SigmaMM: [3]float64{1, 1, 0}, Amount: 1})
	if err != nil {
		t.Fatal(err)
	}
	for x := 12; x < 52; x++ {
		if d := math.Abs(float64(out.At(x, 4, 0) - ramp.At(x, 4, 0))); d > 0.05 {
			t.Fatalf("ramp changed by %.3f at x=%d", d, x)
		}
	}
	step := noisyStep(64, 0.5, 0, 1)
	sharp, err := UnsharpMask(context.Background(), step, UnsharpMaskOptions{SigmaMM: [3]float64{1, 1, 0}, Amount: 1})
	if err != nil {
		t.Fatal(err)
	}
	if over, under := sharp.At(33, 30, 0), sharp.At(30, 30, 0); over <= 100 || under >= 0 {
		t.Fatalf("no overshoot at the step: %.2f / %.2f", over, under)
	}
	if edgeWidth(sharp) > edgeWidth(step)+1e-9 {
		t.Fatalf("unsharp widened the edge: %.2f vs %.2f", edgeWidth(sharp), edgeWidth(step))
	}
	gated, err := UnsharpMask(context.Background(), noisyStep(64, 0.5, 2, 3), UnsharpMaskOptions{SigmaMM: [3]float64{1, 1, 0}, Amount: 1, Threshold: 20})
	if err != nil {
		t.Fatal(err)
	}
	if noise := flatNoise(gated); noise > 2.2 {
		t.Fatalf("thresholded unsharp amplified flat noise to %.2f", noise)
	}
}

func TestEdgePreservingFiltersValidateAndCancel(t *testing.T) {
	src := noisyStep(16, 1, 1, 1)
	if _, err := Bilateral(context.Background(), src, BilateralOptions{SigmaMM: [3]float64{1, 1, 0}}); !errors.Is(err, ErrInvalidVolume) {
		t.Fatalf("zero range sigma: %v", err)
	}
	if _, err := Bilateral(context.Background(), src, BilateralOptions{SigmaMM: [3]float64{100, 100, 0}, RangeSigma: 1, MaxKernelSamples: 100}); !errors.Is(err, ErrInvalidVolume) {
		t.Fatalf("kernel bound: %v", err)
	}
	if _, err := UnsharpMask(context.Background(), src, UnsharpMaskOptions{SigmaMM: [3]float64{1, 1, 0}, Threshold: -1}); !errors.Is(err, ErrInvalidVolume) {
		t.Fatalf("negative threshold: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := GradientAnisotropicDiffusion(ctx, src, AnisotropicDiffusionOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("diffusion cancel: %v", err)
	}
	if _, err := Bilateral(ctx, src, BilateralOptions{SigmaMM: [3]float64{1, 1, 0}, RangeSigma: 5}); !errors.Is(err, context.Canceled) {
		t.Fatalf("bilateral cancel: %v", err)
	}
}

// TestEdgePreservingSliceTime reports the time per displayed 512² slice.
func TestEdgePreservingSliceTime(t *testing.T) {
	if testing.Short() {
		t.Skip("timing")
	}
	src := noisyStep(512, 0.7, 10, 5)
	for name, run := range map[string]func() error{
		"diffusion 5 iterations": func() error {
			_, err := GradientAnisotropicDiffusion(context.Background(), src, AnisotropicDiffusionOptions{})
			return err
		},
		"curvature diffusion 5 iterations": func() error {
			_, err := CurvatureAnisotropicDiffusion(context.Background(), src, AnisotropicDiffusionOptions{})
			return err
		},
		"bilateral 1 mm": func() error {
			_, err := Bilateral(context.Background(), src, BilateralOptions{SigmaMM: [3]float64{1, 1, 0}, RangeSigma: 30})
			return err
		},
		"unsharp 1 mm": func() error {
			_, err := UnsharpMask(context.Background(), src, UnsharpMaskOptions{SigmaMM: [3]float64{1, 1, 0}})
			return err
		},
	} {
		start := time.Now()
		if err := run(); err != nil {
			t.Fatal(err)
		}
		elapsed := time.Since(start)
		t.Logf("%s: %v per 512² slice", name, elapsed)
		if elapsed > 2*time.Second {
			t.Errorf("%s took %v per slice", name, elapsed)
		}
	}
}
