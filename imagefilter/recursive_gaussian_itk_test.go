package imagefilter

// Scenarios ported from ITK (Apache-2.0, commit 35b196e1) recursive Gaussian
// tests: itkHessianRecursiveGaussianFilterScaleSpaceTest.cxx,
// itkRecursiveGaussianImageFilterTest.cxx and
// itkRecursiveGaussianScaleSpaceTest1.cxx. The synthetic inputs and the
// assertions are re-expressed here; no ITK code is copied.

import (
	"context"
	"math"
	"testing"
)

// TestITKHessianScaleSpace: a Gaussian ridge exp(−x²/2s²) filtered with the
// scale-normalized Hessian at σ = s has Hxx = −1/(2√2) ≈ −0.35355 at its
// centre for every s in the continuous limit; ITK's recursive filter gives
// −0.3546 (its test accepts [−0.3547, −0.3546]), the ~0.3% being the
// recursive approximation. The second half keeps s = σ = 5 mm and varies the
// spacing from 0.2 to 1 mm (ITK accepts [−0.355, −0.354]).
func TestITKHessianScaleSpace(t *testing.T) {
	run := func(objectSize, spacing float64) float64 {
		dims := [3]int{401, 21, 21}
		// ITK's origin is (−20, −1.25, −1.25) at spacing 0.1 and keeps
		// those values when the spacing changes: the centre of the ridge
		// is at index 20/spacing along x.
		src := NewVolume(dims, IsotropicSigma(spacing))
		center := int(math.Round(20 / spacing))
		for z := 0; z < dims[2]; z++ {
			for y := 0; y < dims[1]; y++ {
				for x := 0; x < dims[0]; x++ {
					p := -20 + float64(x)*spacing
					src.Data[src.Index(x, y, z)] = float32(math.Exp(-p * p / (2 * objectSize * objectSize)))
				}
			}
		}
		h, err := HessianRecursiveGaussian(context.Background(), src,
			RecursiveGaussianOptions{SigmaMM: IsotropicSigma(objectSize), NormalizeAcrossScale: true})
		if err != nil {
			t.Fatal(err)
		}
		// ITK maps the physical centre (0, 0, 0) to the nearest index; with
		// the y/z origin at −1.25 that is index 12 or 13 of a constant
		// profile, so any y/z index gives the same value.
		return float64(h.XX.At(center, 10, 10))
	}
	for _, s := range []float64{1, 2, 3, 5} {
		hxx := run(s, 0.1)
		t.Logf("σ = s = %g mm, spacing 0.1 mm: Hxx = %.5f (continuous %.5f)", s, hxx, -1/(2*math.Sqrt2))
		if hxx > -0.3546 || hxx < -0.3547 {
			t.Errorf("σ = %g: centre Hxx = %.6f, want in [−0.3547, −0.3546]", s, hxx)
		}
	}
	for _, scale := range []float64{1, 2, 3, 5} {
		spacing := scale / 5
		// Only 401 samples: at spacing 0.2 the line spans ±40 mm, at 1 mm
		// ITK's image starts at −20 and extends to +380.
		hxx := run(5, spacing)
		t.Logf("σ = s = 5 mm, spacing %g mm: Hxx = %.5f", spacing, hxx)
		if hxx > -0.354 || hxx < -0.355 {
			t.Errorf("spacing %g: centre Hxx = %.6f, want in [−0.355, −0.354]", spacing, hxx)
		}
	}
}

// TestITKRecursiveGaussianNormalization ports the 1-D checks of ITK's
// itkRecursiveGaussianImageFilterTest: an impulse of 1000 in the middle of
// 21 samples keeps its total (within 1e-3) after smoothing with σ = 2, the
// scale normalization leaves smoothing unchanged, and by the mean value
// theorem the first derivative at the middle − 2 lies between the finite
// differences of the smoothed signal around it (σ = 3), and likewise the
// second derivative between those of the first derivative.
func TestITKRecursiveGaussianNormalization(t *testing.T) {
	ctx := context.Background()
	src := NewVolume([3]int{21, 1, 1}, [3]float64{1, 1, 1})
	src.Data[10] = 1000
	filter := func(order int, sigma float64, normalize bool) []float32 {
		out, err := RecursiveGaussian(ctx, src, [3]int{order, 0, 0},
			RecursiveGaussianOptions{SigmaMM: [3]float64{sigma, 1, 1}, NormalizeAcrossScale: normalize})
		if err != nil {
			t.Fatal(err)
		}
		return out.Data
	}
	a, b := filter(0, 2, true), filter(0, 2, false)
	if math.Abs(float64(a[10]-b[10])) > 1e-4 {
		t.Errorf("scale normalization changed smoothing: %v vs %v", a[10], b[10])
	}
	total := 0.0
	for _, v := range a {
		total += float64(v)
	}
	if e := math.Abs(total-1000) / 1000; e > 1e-3 {
		t.Errorf("impulse total after smoothing %v (relative error %.3g)", total, e)
	}

	s0, s1, s2 := filter(0, 3, false), filter(1, 3, false), filter(2, 3, false)
	mid := 10
	lo, hi := (s0[mid]-s0[mid-2])/2, (s0[mid-2]-s0[mid-4])/2
	if d := s1[mid-2]; d < lo || d > hi {
		t.Errorf("first derivative %v outside [%v, %v]", d, lo, hi)
	}
	lo, hi = (s1[mid]-s1[mid-2])/2, (s1[mid-2]-s1[mid-4])/2
	if d := s2[mid-2]; d < lo || d > hi {
		t.Errorf("second derivative %v outside [%v, %v]", d, lo, hi)
	}
}

// TestITKRecursiveGaussianScaleSpace ports itkRecursiveGaussianScaleSpaceTest1:
// for a sine of angular frequency ω, the scale-normalized derivative of order
// k peaks over σ at σ = √k/ω with the value k^{k/2}·e^{−k/2}. The maximum over
// the image at σ = √k/ω must not be exceeded at 0.95σ or 1.05σ, and it must
// be within 0.01 of the analytic value (ITK only prints that last check).
// Spacing invariance is checked at 0.01 and 100 mm.
func TestITKRecursiveGaussianScaleSpace(t *testing.T) {
	const size = 1024
	check := func(cyclesPerImage float64, order int, spacing float64) {
		t.Helper()
		omega := cyclesPerImage * 2 * math.Pi / (size * spacing)
		sigmaMax := math.Sqrt(float64(order)) / omega
		expected := math.Pow(float64(order), float64(order)/2) * math.Exp(-float64(order)/2)
		tol := math.Pow(1e-6, 1/float64(order))
		src := NewVolume([3]int{size, 1, 1}, [3]float64{spacing, 1, 1})
		for i := range src.Data {
			src.Data[i] = float32(math.Sin(float64(i) * spacing * omega))
		}
		peak := func(sigma float64) float64 {
			out, err := RecursiveGaussian(context.Background(), src, [3]int{order, 0, 0},
				RecursiveGaussianOptions{SigmaMM: [3]float64{sigma, 1, 1}, NormalizeAcrossScale: true})
			if err != nil {
				t.Fatal(err)
			}
			m := math.Inf(-1)
			for _, v := range out.Data {
				m = max(m, float64(v))
			}
			return m
		}
		best := peak(sigmaMax)
		for _, f := range []float64{0.95, 1.05} {
			if other := peak(f * sigmaMax); other > best && other-best > tol {
				t.Errorf("f=%g order %d spacing %g: max %.6f at %.2fσ exceeds %.6f at σ", cyclesPerImage, order, spacing, other, f, best)
			}
		}
		if math.Abs(best-expected) > 0.01 {
			t.Errorf("f=%g order %d spacing %g: max %.5f, expected %.5f", cyclesPerImage, order, spacing, best, expected)
		}
	}
	for _, order := range []int{1, 2} {
		for _, f := range []float64{1.5, 2.5, 5, 10, 25} {
			check(f, order, 1)
		}
	}
	check(5, 2, 0.01)
	check(5, 2, 100)
}
