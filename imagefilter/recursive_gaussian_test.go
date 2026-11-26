package imagefilter

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"
)

// sampledVolume fills a volume with f evaluated at each voxel's position in
// millimetres, relative to the voxel at index center.
func sampledVolume(dims [3]int, spacing [3]float64, center [3]float64, f func(x, y, z float64) float64) *Volume {
	v := NewVolume(dims, spacing)
	for z := 0; z < dims[2]; z++ {
		for y := 0; y < dims[1]; y++ {
			for x := 0; x < dims[0]; x++ {
				v.Data[v.Index(x, y, z)] = float32(f(
					(float64(x)-center[0])*spacing[0],
					(float64(y)-center[1])*spacing[1],
					(float64(z)-center[2])*spacing[2]))
			}
		}
	}
	return v
}

// maxInteriorError is the largest |got − want(x, y, z)| over the voxels at
// least margin[axis] samples away from every border.
func maxInteriorError(got *Volume, margin [3]int, center [3]float64, want func(x, y, z float64) float64) float64 {
	worst := 0.0
	d, s := got.Dims, got.SpacingMM
	for z := margin[2]; z < d[2]-margin[2]; z++ {
		for y := margin[1]; y < d[1]-margin[1]; y++ {
			for x := margin[0]; x < d[0]-margin[0]; x++ {
				w := want((float64(x)-center[0])*s[0], (float64(y)-center[1])*s[1], (float64(z)-center[2])*s[2])
				worst = max(worst, math.Abs(float64(got.At(x, y, z))-w))
			}
		}
	}
	return worst
}

func TestRecursiveGaussianConstant(t *testing.T) {
	src := sampledVolume([3]int{17, 13, 9}, [3]float64{0.6, 0.9, 2.5}, [3]float64{}, func(x, y, z float64) float64 { return 123.25 })
	opts := RecursiveGaussianOptions{SigmaMM: [3]float64{1.5, 3, 7}}
	ctx := context.Background()
	smooth, err := SmoothRecursiveGaussian(ctx, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range smooth.Data {
		if math.Abs(float64(v)-123.25) > 2e-4 {
			t.Fatalf("smoothed constant[%d] = %v", i, v)
		}
	}
	hessian, err := HessianRecursiveGaussian(ctx, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	grad, err := GradientRecursiveGaussian(ctx, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	lap, err := LaplacianRecursiveGaussian(ctx, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]*Volume{
		"gx": grad[0], "gy": grad[1], "gz": grad[2], "log": lap,
		"xx": hessian.XX, "xy": hessian.XY, "xz": hessian.XZ, "yy": hessian.YY, "yz": hessian.YZ, "zz": hessian.ZZ,
	} {
		for i, x := range v.Data {
			if math.Abs(float64(x)) > 1e-4 {
				t.Fatalf("%s of a constant [%d] = %v", name, i, x)
			}
		}
	}
}

// TestRecursiveGaussianPolynomials checks the normalization on polynomials,
// away from the borders where the replicated edge bends them: smoothing and
// the first derivatives are exact on a linear ramp, the first derivatives
// on a quadratic, and the Hessian on a quadratic is its constant second
// derivatives.
func TestRecursiveGaussianPolynomials(t *testing.T) {
	dims := [3]int{96, 72, 48}
	spacing := [3]float64{0.5, 0.8, 1.6}
	center := [3]float64{47.5, 35.5, 23.5}
	sigma := [3]float64{1.5, 2, 3} // 3, 2.5 and 1.9 samples
	opts := RecursiveGaussianOptions{SigmaMM: sigma}
	margin := [3]int{30, 25, 19}
	ctx := context.Background()

	ramp := func(x, y, z float64) float64 { return 2.5*x - 1.25*y + 0.75*z + 40 }
	src := sampledVolume(dims, spacing, center, ramp)
	smooth, err := SmoothRecursiveGaussian(ctx, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	if e := maxInteriorError(smooth, margin, center, ramp); e > 2e-4 {
		t.Errorf("smoothed ramp error %.3g", e)
	}
	grad, err := GradientRecursiveGaussian(ctx, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	for axis, slope := range []float64{2.5, -1.25, 0.75} {
		if e := maxInteriorError(grad[axis], margin, center, func(x, y, z float64) float64 { return slope }); e > 1e-4 {
			t.Errorf("ramp derivative along axis %d: error %.3g (slope %v)", axis, e, slope)
		}
	}
	// The replicated border halves the derivative of a ramp at the edge.
	if edge := float64(grad[0].At(0, 36, 24)) / 2.5; math.Abs(edge-0.5) > 0.05 {
		t.Errorf("ramp derivative at the x edge is %.3f of the slope, want about 0.5", edge)
	}

	quad := func(x, y, z float64) float64 { return 0.5*x*x + 0.25*x*y - 0.3*y*y + 0.1*y*z + 0.2*z*z - 0.4*x*z + x }
	src = sampledVolume(dims, spacing, center, quad)
	grad, err = GradientRecursiveGaussian(ctx, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	wantGrad := []func(x, y, z float64) float64{
		func(x, y, z float64) float64 { return x + 0.25*y - 0.4*z + 1 },
		func(x, y, z float64) float64 { return 0.25*x - 0.6*y + 0.1*z },
		func(x, y, z float64) float64 { return 0.1*y + 0.4*z - 0.4*x },
	}
	for axis, want := range wantGrad {
		if e := maxInteriorError(grad[axis], margin, center, want); e > 2e-4 {
			t.Errorf("quadratic derivative along axis %d: error %.3g", axis, e)
		}
	}
	h, err := HessianRecursiveGaussian(ctx, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	lap, err := LaplacianRecursiveGaussian(ctx, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		v    *Volume
		want float64
	}{
		{"xx", h.XX, 1}, {"xy", h.XY, 0.25}, {"xz", h.XZ, -0.4}, {"yy", h.YY, -0.6}, {"yz", h.YZ, 0.1}, {"zz", h.ZZ, 0.4},
		{"laplacian", lap, 0.8},
	} {
		if e := maxInteriorError(c.v, margin, center, func(x, y, z float64) float64 { return c.want }); e > 2e-4 {
			t.Errorf("H%s of the quadratic: error %.3g (want %v)", c.name, e, c.want)
		}
	}
}

// TestRecursiveGaussianAnisotropicBlob filters an isotropic Gaussian blob
// sampled on an anisotropic grid with a different σ per axis and compares
// every output with the closed form: the blob of width s convolved with a
// Gaussian of σ_a along axis a is a Gaussian of width S_a = √(s²+σ_a²).
func TestRecursiveGaussianAnisotropicBlob(t *testing.T) {
	dims := [3]int{81, 61, 41}
	spacing := [3]float64{0.4, 0.6, 1.2}
	center := [3]float64{40, 30, 20}
	const s, amp = 3.0, 100.0
	sigma := [3]float64{1.2, 2, 2.5}
	src := sampledVolume(dims, spacing, center, func(x, y, z float64) float64 {
		return amp * math.Exp(-(x*x+y*y+z*z)/(2*s*s))
	})
	var S2 [3]float64
	peak := amp
	for a := range S2 {
		S2[a] = s*s + sigma[a]*sigma[a]
		peak *= s / math.Sqrt(S2[a])
	}
	smooth := func(x, y, z float64) float64 {
		return peak * math.Exp(-(x*x/S2[0]+y*y/S2[1]+z*z/S2[2])/2)
	}
	d1 := func(a int) func(x, y, z float64) float64 {
		return func(x, y, z float64) float64 { return -[3]float64{x, y, z}[a] / S2[a] * smooth(x, y, z) }
	}
	d2 := func(a, b int) func(x, y, z float64) float64 {
		return func(x, y, z float64) float64 {
			p := [3]float64{x, y, z}
			v := p[a] / S2[a] * p[b] / S2[b]
			if a == b {
				v -= 1 / S2[a]
			}
			return v * smooth(x, y, z)
		}
	}
	opts := RecursiveGaussianOptions{SigmaMM: sigma}
	ctx := context.Background()
	// Errors are relative to the peak of each analytic output: about 0.03%
	// for smoothing, 0.4% for first and 0.4–0.8% for second derivatives (see
	// TestRecursiveGaussianImpulseResponse for the 1-D kernels).
	check := func(name string, got *Volume, want func(x, y, z float64) float64, tol float64) {
		t.Helper()
		scale := 0.0
		maxInteriorError(got, [3]int{}, center, func(x, y, z float64) float64 {
			scale = max(scale, math.Abs(want(x, y, z)))
			return 0
		})
		e := maxInteriorError(got, [3]int{}, center, want) / scale
		t.Logf("%-9s max error %.2e of peak %.4g", name, e, scale)
		if e > tol {
			t.Errorf("%s: relative error %.3g > %v", name, e, tol)
		}
	}
	out, err := SmoothRecursiveGaussian(ctx, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	check("smooth", out, smooth, 1e-3)
	grad, err := GradientRecursiveGaussian(ctx, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	for a, v := range grad {
		check([]string{"dx", "dy", "dz"}[a], v, d1(a), 6e-3)
	}
	mag, err := GradientMagnitudeRecursiveGaussian(ctx, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	check("|grad|", mag, func(x, y, z float64) float64 {
		return math.Sqrt(math.Pow(d1(0)(x, y, z), 2) + math.Pow(d1(1)(x, y, z), 2) + math.Pow(d1(2)(x, y, z), 2))
	}, 6e-3)
	h, err := HessianRecursiveGaussian(ctx, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		v    *Volume
		a, b int
	}{{"dxx", h.XX, 0, 0}, {"dxy", h.XY, 0, 1}, {"dxz", h.XZ, 0, 2}, {"dyy", h.YY, 1, 1}, {"dyz", h.YZ, 1, 2}, {"dzz", h.ZZ, 2, 2}} {
		check(c.name, c.v, d2(c.a, c.b), 1e-2)
	}
	lap, err := LaplacianRecursiveGaussian(ctx, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	check("laplacian", lap, func(x, y, z float64) float64 { return d2(0, 0)(x, y, z) + d2(1, 1)(x, y, z) + d2(2, 2)(x, y, z) }, 1e-2)

	// NormalizeAcrossScale multiplies each derivative by σ^order per axis.
	opts.NormalizeAcrossScale = true
	hn, err := HessianRecursiveGaussian(ctx, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		raw, norm *Volume
		factor    float64
	}{{h.XX, hn.XX, sigma[0] * sigma[0]}, {h.XY, hn.XY, sigma[0] * sigma[1]}, {h.ZZ, hn.ZZ, sigma[2] * sigma[2]}} {
		for i := range c.raw.Data {
			if d := math.Abs(float64(c.norm.Data[i]) - c.factor*float64(c.raw.Data[i])); d > 1e-6+1e-5*math.Abs(float64(c.norm.Data[i])) {
				t.Fatalf("normalized Hessian [%d] = %v, want %v", i, c.norm.Data[i], c.factor*float64(c.raw.Data[i]))
			}
		}
	}
}

// impulseResponse filters a unit impulse at the middle of a line of n
// samples of unit spacing.
func impulseResponse(t testing.TB, n int, sigma float64, order int) []float64 {
	src := NewVolume([3]int{n, 1, 1}, [3]float64{1, 1, 1})
	src.Data[n/2] = 1
	out, err := RecursiveGaussian(context.Background(), src, [3]int{order, 0, 0}, RecursiveGaussianOptions{SigmaMM: [3]float64{sigma, 0, 0}})
	if err != nil {
		t.Fatal(err)
	}
	r := make([]float64, n)
	for i, v := range out.Data {
		r[i] = float64(v)
	}
	return r
}

// TestRecursiveGaussianImpulseResponse compares the impulse responses with
// the sampled Gaussian and its derivatives (the sampled kernels normalized
// like the recursive ones: unit sum, unit ramp response, response 2 to n²).
// The error is relative to the kernel's peak magnitude. It is the error of
// the fits themselves and hardly depends on σ: 0.03–0.05% for order 0
// (Deriche), 0.3–0.4% for order 1 and 1.0–1.5% for order 2 (Farnebäck–
// Westin, as in ITK), from σ = 1 to 32 samples.
//
// The effective scale is checked through the kernel moments, which is what
// matters on smooth data: the variance of the order-0 kernel, Σk²g/Σg, and
// the variances implied by the derivative kernels (Σk³g₁/3 and Σk⁴g₂/12
// with the normalizations above), against σ². Deriche's Gaussian has 0.996σ²;
// the Farnebäck–Westin kernels 0.938σ² for every order, which is why they are
// not used for smoothing (ITK's SmoothingRecursiveGaussianImageFilter uses
// them and smooths like a Gaussian of 0.969σ).
func TestRecursiveGaussianImpulseResponse(t *testing.T) {
	tolerance := [3]float64{6e-4, 5e-3, 1.8e-2}
	varianceRange := [3][2]float64{{0.993, 1}, {0.93, 0.945}, {0.93, 0.945}}
	for order := 0; order <= 2; order++ {
		for _, sigma := range []float64{1, 2, 4, 8, 16, 32} {
			n := int(24*sigma) | 1
			got := impulseResponse(t, n, sigma, order)
			mid := n / 2
			// The response at i to the impulse at mid is the kernel weight
			// of the sample at offset k = mid−i from the output.
			want := make([]float64, n)
			for i := range want {
				k := float64(mid - i)
				g := math.Exp(-k * k / (2 * sigma * sigma))
				want[i] = [3]float64{g, k * g, (k*k - sigma*sigma) * g}[order]
			}
			var norm float64
			for i, w := range want {
				k := float64(mid - i)
				// Response to 1, to the ramp k and to k²/2.
				norm += [3]float64{w, k * w, k * k * w / 2}[order]
			}
			peak := 0.0
			for i := range want {
				want[i] /= norm
				peak = max(peak, math.Abs(want[i]))
			}
			worst := 0.0
			var m [5]float64
			for i := range got {
				worst = max(worst, math.Abs(got[i]-want[i]))
				k, p := float64(mid-i), 1.0
				for j := range m {
					m[j] += p * got[i]
					p *= k
				}
			}
			variance := [3]float64{m[2] / m[0], m[3] / 3, m[4] / 12}[order] / (sigma * sigma)
			t.Logf("order %d σ=%-4g samples: max |IIR − sampled| = %.2e of peak %.4g (%.3f%%), variance %.4fσ²", order, sigma, worst, peak, 100*worst/peak, variance)
			if worst/peak > tolerance[order] {
				t.Errorf("order %d σ=%v: relative impulse error %.3g > %v", order, sigma, worst/peak, tolerance[order])
			}
			if r := varianceRange[order]; variance < r[0] || variance > r[1] {
				t.Errorf("order %d σ=%v: effective variance %.4fσ² outside %v", order, sigma, variance, r)
			}
		}
	}
}

func TestRecursiveGaussianTwoDimensional(t *testing.T) {
	ctx := context.Background()
	img := sampledVolume([3]int{64, 48, 1}, [3]float64{0.7, 0.7, 0}, [3]float64{32, 24, 0}, func(x, y, _ float64) float64 {
		return 50 * math.Exp(-(x*x+y*y)/(2*16))
	})
	opts := RecursiveGaussianOptions{SigmaMM: IsotropicSigma(2)}
	h, err := HessianRecursiveGaussian(ctx, img, opts)
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]*Volume{"xz": h.XZ, "yz": h.YZ, "zz": h.ZZ} {
		for _, x := range v.Data {
			if x != 0 {
				t.Fatalf("H%s of a 2-D image = %v, want 0", name, x)
			}
		}
	}
	lap, err := LaplacianRecursiveGaussian(ctx, img, opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := range lap.Data {
		if want := h.XX.Data[i] + h.YY.Data[i]; math.Abs(float64(lap.Data[i]-want)) > 1e-6 {
			t.Fatalf("2-D Laplacian [%d] = %v, want Hxx+Hyy = %v", i, lap.Data[i], want)
		}
	}
	// The same image as one slice of a thicker volume, with a z σ of 0,
	// gives the same in-plane result.
	stack := NewVolume([3]int{64, 48, 3}, [3]float64{0.7, 0.7, 5})
	for z := 0; z < 3; z++ {
		copy(stack.Data[z*64*48:], img.Data)
	}
	smooth2D, err := SmoothRecursiveGaussian(ctx, img, opts)
	if err != nil {
		t.Fatal(err)
	}
	smooth3D, err := SmoothRecursiveGaussian(ctx, stack, RecursiveGaussianOptions{SigmaMM: [3]float64{2, 2, 0}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(smooth2D.Data, smooth3D.Data[64*48:2*64*48]) {
		t.Fatal("2-D smoothing differs from the in-plane smoothing of a volume")
	}
}

func TestRecursiveGaussianWorkersAndCancellation(t *testing.T) {
	src := sampledVolume([3]int{40, 33, 27}, [3]float64{0.8, 0.8, 1.5}, [3]float64{20, 16, 13}, func(x, y, z float64) float64 {
		return math.Sin(x/3) * math.Cos(y/4) * (z + 5)
	})
	ctx := context.Background()
	one, err := HessianRecursiveGaussian(ctx, src, RecursiveGaussianOptions{SigmaMM: IsotropicSigma(2), Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	many, err := HessianRecursiveGaussian(ctx, src, RecursiveGaussianOptions{SigmaMM: IsotropicSigma(2), Workers: 7})
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]*Volume{{one.XX, many.XX}, {one.XY, many.XY}, {one.XZ, many.XZ}, {one.YY, many.YY}, {one.YZ, many.YZ}, {one.ZZ, many.ZZ}} {
		if !slices.Equal(pair[0].Data, pair[1].Data) {
			t.Fatal("result depends on the worker count")
		}
	}
	// A nil context means context.Background().
	var noContext context.Context
	if _, err := GradientMagnitudeRecursiveGaussian(noContext, src, RecursiveGaussianOptions{SigmaMM: IsotropicSigma(2)}); err != nil {
		t.Fatalf("nil context: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := GradientMagnitudeRecursiveGaussian(cancelled, src, RecursiveGaussianOptions{SigmaMM: IsotropicSigma(2)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled gradient magnitude error = %v", err)
	}
	if _, err := SmoothRecursiveGaussian(cancelled, src, RecursiveGaussianOptions{SigmaMM: IsotropicSigma(2)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled smoothing error = %v", err)
	}
}

func TestRecursiveGaussianRejectsInvalidInput(t *testing.T) {
	ctx := context.Background()
	good := NewVolume([3]int{4, 4, 4}, [3]float64{1, 1, 1})
	nan := good.Clone()
	nan.Data[5] = float32(math.NaN())
	short := &Volume{Dims: [3]int{4, 4, 4}, SpacingMM: [3]float64{1, 1, 1}, Data: make([]float32, 63)}
	flat := &Volume{Dims: [3]int{4, 4, 4}, SpacingMM: [3]float64{1, 0, 1}, Data: make([]float32, 64)}
	for name, c := range map[string]struct {
		src    *Volume
		orders [3]int
		sigma  [3]float64
	}{
		"nil volume":      {nil, [3]int{}, IsotropicSigma(1)},
		"NaN sample":      {nan, [3]int{}, IsotropicSigma(1)},
		"short data":      {short, [3]int{}, IsotropicSigma(1)},
		"zero spacing":    {flat, [3]int{}, IsotropicSigma(1)},
		"negative sigma":  {good, [3]int{}, [3]float64{1, -1, 1}},
		"NaN sigma":       {good, [3]int{}, [3]float64{math.NaN(), 1, 1}},
		"order 3":         {good, [3]int{3, 0, 0}, IsotropicSigma(1)},
		"negative order":  {good, [3]int{0, -1, 0}, IsotropicSigma(1)},
		"zero-σ gradient": {good, [3]int{0, 0, 1}, [3]float64{1, 1, 0}},
	} {
		if _, err := RecursiveGaussian(ctx, c.src, c.orders, RecursiveGaussianOptions{SigmaMM: c.sigma}); !errors.Is(err, ErrInvalidVolume) {
			t.Errorf("%s: error = %v, want ErrInvalidVolume", name, err)
		}
	}
	// A zero σ along an axis leaves it unsmoothed.
	src := sampledVolume([3]int{9, 9, 9}, [3]float64{1, 1, 1}, [3]float64{}, func(x, y, z float64) float64 { return x*x + 3*z })
	out, err := SmoothRecursiveGaussian(ctx, src, RecursiveGaussianOptions{SigmaMM: [3]float64{}})
	if err != nil || !slices.Equal(out.Data, src.Data) || &out.Data[0] == &src.Data[0] {
		t.Fatalf("zero-σ smoothing: err %v, copy %v", err, err == nil && &out.Data[0] != &src.Data[0])
	}
}
