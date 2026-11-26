package imagefilter

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"runtime"
	"sync/atomic"
	"testing"
)

// fillVolume sets every voxel from f of its patient position (mm) relative
// to the volume centre.
func fillVolume(dims [3]int, spacing [3]float64, f func(x, y, z float64) float64) *Volume {
	v := NewVolume(dims, spacing)
	for k := 0; k < dims[2]; k++ {
		for j := 0; j < dims[1]; j++ {
			for i := 0; i < dims[0]; i++ {
				x := (float64(i) - float64(dims[0]-1)/2) * spacing[0]
				y := (float64(j) - float64(dims[1]-1)/2) * spacing[1]
				z := (float64(k) - float64(dims[2]-1)/2) * spacing[2]
				v.Data[v.Index(i, j, k)] = float32(f(x, y, z))
			}
		}
	}
	return v
}

func centre(v *Volume) int { return v.Index(v.Dims[0]/2, v.Dims[1]/2, v.Dims[2]/2) }

// gaussianTube is a tube along z with a Gaussian cross-section of standard
// deviation s (mm) and peak 100.
func gaussianTube(s float64) func(x, y, z float64) float64 {
	return func(x, y, _ float64) float64 { return 100 * math.Exp(-(x*x+y*y)/(2*s*s)) }
}

// solidCylinder is a uniform cylinder along z of radius r (mm), 100 inside
// and 0 outside, with the boundary voxels weighted by a 4×4 supersampling so
// the sampled radius is right.
func solidCylinder(r, spacing float64) func(x, y, z float64) float64 {
	return func(x, y, _ float64) float64 {
		inside := 0
		for a := 0; a < 4; a++ {
			for b := 0; b < 4; b++ {
				dx := x + (float64(a)-1.5)/4*spacing
				dy := y + (float64(b)-1.5)/4*spacing
				if dx*dx+dy*dy <= r*r {
					inside++
				}
			}
		}
		return 100 * float64(inside) / 16
	}
}

func denseSigmas(minMM, maxMM float64) []float64 {
	s, err := VesselnessSigmas(minMM, maxMM, 25, false)
	if err != nil {
		panic(err)
	}
	return s
}

func TestVesselnessSigmas(t *testing.T) {
	got, err := VesselnessSigmas(1, 8, 4, false)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []float64{1, 2, 4, 8} {
		if math.Abs(got[i]-want) > 1e-12 {
			t.Fatalf("logarithmic sigmas %v", got)
		}
	}
	got, _ = VesselnessSigmas(1, 4, 4, true)
	for i, want := range []float64{1, 2, 3, 4} {
		if math.Abs(got[i]-want) > 1e-12 {
			t.Fatalf("equispaced sigmas %v", got)
		}
	}
	// ITK: a single step is the minimum.
	if got, _ := VesselnessSigmas(0.7, 3, 1, false); len(got) != 1 || got[0] != 0.7 {
		t.Fatalf("single step %v", got)
	}
	for _, bad := range [][3]float64{{0, 1, 2}, {2, 1, 2}, {1, math.Inf(1), 2}, {1, 2, 0}} {
		if _, err := VesselnessSigmas(bad[0], bad[1], int(bad[2]), false); !errors.Is(err, ErrInvalidVolume) {
			t.Fatalf("VesselnessSigmas%v error %v", bad, err)
		}
	}
}

// TestVesselnessScaleSelectionTubes is the acceptance test of #1099:
// synthetic tubes of known radius respond at the right scale. With the
// σ²-normalized Hessian, the cross-section curvature at the centre of a
// Gaussian tube of deviation s is −A·σ²s²/(s²+σ²)², largest at σ = s where
// it is −A/4; for a solid cylinder of radius R it is −A·u·exp(−u),
// u = R²/2σ² (from the heat equation, the centre value being
// A(1 − exp(−R²/2σ²))), largest at σ = R/√2 where it is −A/e. Sato's
// measure at the centre is that curvature; Frangi's scaled objectness is
// proportional to it.
func TestVesselnessScaleSelectionTubes(t *testing.T) {
	const spacing = 0.5
	dims := [3]int{57, 57, 9}
	type tubeCase struct {
		name      string
		f         func(x, y, z float64) float64
		wantSigma float64
		wantPeak  float64 // Sato response at the centre, at the best scale
	}
	var cases []tubeCase
	for _, s := range []float64{1, 1.5, 2.5} {
		cases = append(cases, tubeCase{"gaussian", gaussianTube(s), s, 100.0 / 4})
	}
	for _, r := range []float64{2, 3, 4} {
		cases = append(cases, tubeCase{"cylinder", solidCylinder(r, spacing), r / math.Sqrt2, 100 / math.E})
	}
	sigmas := denseSigmas(0.5, 5)
	step := sigmas[1] / sigmas[0]
	for _, tc := range cases {
		src := fillVolume(dims, IsotropicSigma(spacing), tc.f)
		for _, measure := range []VesselnessMeasure{VesselnessSato, VesselnessFrangi} {
			opts := VesselnessOptions{Measure: measure, SigmasMM: sigmas, ScalesOutput: true}
			if measure == VesselnessFrangi {
				// ITK's defaults: γ fixed, objectness scaled by |λ3|. (With
				// the automatic γ every scale saturates at the centre of an
				// isolated tube, so the scale is not selective.)
				opts.Gamma, opts.ScaleByLargestEigenvalue = 5, true
			}
			res, err := Vesselness(context.Background(), src, opts)
			if err != nil {
				t.Fatal(err)
			}
			c := centre(src)
			got := float64(res.ScaleMM.Data[c])
			if got < tc.wantSigma/step || got > tc.wantSigma*step {
				t.Errorf("%s %v tube, %v: best σ = %.3g mm, want %.3g mm (±1 step)", tc.name, tc.wantSigma, measure, got, tc.wantSigma)
			}
			if measure == VesselnessSato {
				peak := float64(res.Response.Data[c])
				if math.Abs(peak-tc.wantPeak) > 0.03*tc.wantPeak {
					t.Errorf("%s %v tube: Sato peak %.4g, want %.4g", tc.name, tc.wantSigma, peak, tc.wantPeak)
				}
			}
		}
	}
}

// TestVesselnessShapes checks that tubes respond and plates and blobs of
// the same profile do not, and that the Frangi structure option picks each
// shape.
func TestVesselnessShapes(t *testing.T) {
	const s = 1.5
	dims := [3]int{41, 41, 41}
	g := func(d2 float64) float64 { return 100 * math.Exp(-d2/(2*s*s)) }
	shapes := map[string]*Volume{
		"tube":  fillVolume(dims, IsotropicSigma(0.5), func(x, y, _ float64) float64 { return g(x*x + y*y) }),
		"plate": fillVolume(dims, IsotropicSigma(0.5), func(x, _, _ float64) float64 { return g(x * x) }),
		"blob":  fillVolume(dims, IsotropicSigma(0.5), func(x, y, z float64) float64 { return g(x*x + y*y + z*z) }),
	}
	respond := func(shape string, opts VesselnessOptions) float64 {
		t.Helper()
		opts.SigmasMM = []float64{s}
		res, err := Vesselness(context.Background(), shapes[shape], opts)
		if err != nil {
			t.Fatal(err)
		}
		return float64(res.Response.Data[centre(shapes[shape])])
	}
	for _, structure := range []struct {
		s    HessianStructure
		want string
	}{{StructureTube, "tube"}, {StructureBlob, "blob"}, {StructurePlate, "plate"}} {
		opts := VesselnessOptions{Structure: structure.s, Gamma: -1}
		for shape := range shapes {
			v := respond(shape, opts)
			// Without γ the ideal shape gives 1 − e⁻² (Ra = 1 with α = 0.5),
			// or 1 for a plate, which has no Ra term; a wrong shape gives
			// at most e⁻² times as much (Rb = 1 with β = 0.5), or 0.
			own := 1 - math.Exp(-2)
			if structure.s == StructurePlate {
				own = 1
			}
			if shape == structure.want && math.Abs(v-own) > 0.02 {
				t.Errorf("Frangi %v on its own shape: %.4g, want %.4g", structure.want, v, own)
			}
			if shape != structure.want && v > math.Exp(-2)*own+0.01 {
				t.Errorf("Frangi %v on a %s: %.4g, want ≤ %.4g", structure.want, shape, v, math.Exp(-2)*own)
			}
		}
	}
	sato := VesselnessOptions{Measure: VesselnessSato}
	tube, plate, blob := respond("tube", sato), respond("plate", sato), respond("blob", sato)
	if !(tube > 20) || plate > 0.01*tube {
		t.Errorf("Sato tube %.4g, plate %.4g", tube, plate)
	}
	// A Gaussian blob's three eigenvalues are equal, −A·σ²s³/(s²+σ²)^{5/2}
	// = −A/2^{5/2} at σ = s against the tube's −A/4, and the third one is
	// weighted by exp(−½(1/α1)²) = e⁻².
	if want := math.Exp(-2) * tube * 4 / math.Pow(2, 2.5); math.Abs(blob-want) > 0.05*want {
		t.Errorf("Sato blob %.4g, want %.4g (tube %.4g)", blob, want, tube)
	}
}

func TestVesselnessDarkAndOblique(t *testing.T) {
	const s = 1.5
	dims := [3]int{41, 41, 41}
	sp := IsotropicSigma(0.5)
	bright := fillVolume(dims, sp, gaussianTube(s))
	dark := fillVolume(dims, sp, func(x, y, z float64) float64 { return 1000 - gaussianTube(s)(x, y, z) })
	// The same tube along (1, 1, 1)/√3.
	oblique := fillVolume(dims, sp, func(x, y, z float64) float64 {
		t := (x + y + z) / 3
		dx, dy, dz := x-t, y-t, z-t
		return 100 * math.Exp(-(dx*dx+dy*dy+dz*dz)/(2*s*s))
	})
	for _, measure := range []VesselnessMeasure{VesselnessFrangi, VesselnessSato} {
		opts := VesselnessOptions{Measure: measure, SigmasMM: []float64{s}, Gamma: 5, ScaleByLargestEigenvalue: true}
		run := func(v *Volume, dark bool) float64 {
			o := opts
			o.DarkObjects = dark
			res, err := Vesselness(context.Background(), v, o)
			if err != nil {
				t.Fatal(err)
			}
			return float64(res.Response.Data[centre(v)])
		}
		b, d, o := run(bright, false), run(dark, true), run(oblique, false)
		if math.Abs(b-d) > 1e-4*b {
			t.Errorf("%v: dark tube %.6g, bright %.6g", measure, d, b)
		}
		if math.Abs(o-b) > 0.03*b {
			t.Errorf("%v: oblique tube %.5g, axis-aligned %.5g", measure, o, b)
		}
		if v := run(dark, false); v != 0 {
			t.Errorf("%v: dark tube as bright object responds %.4g", measure, v)
		}
	}
}

// TestVesselnessAnisotropicSpacing: σ is in millimetres, so a tube across
// thick slices answers at the same scale and with nearly the same value as
// with isotropic voxels.
func TestVesselnessAnisotropicSpacing(t *testing.T) {
	const s = 2.0
	tube := func(x, _, z float64) float64 { return 100 * math.Exp(-(x*x+z*z)/(2*s*s)) } // along y
	iso := fillVolume([3]int{41, 9, 41}, IsotropicSigma(0.5), tube)
	aniso := fillVolume([3]int{41, 9, 15}, [3]float64{0.5, 0.5, 1.5}, tube)
	opts := VesselnessOptions{Measure: VesselnessSato, SigmasMM: denseSigmas(1, 4), ScalesOutput: true}
	a, err := Vesselness(context.Background(), iso, opts)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Vesselness(context.Background(), aniso, opts)
	if err != nil {
		t.Fatal(err)
	}
	va, vb := a.Response.Data[centre(iso)], b.Response.Data[centre(aniso)]
	if math.Abs(float64(va-vb)) > 0.03*float64(va) {
		t.Fatalf("anisotropic response %.4g, isotropic %.4g", vb, va)
	}
	sa, sb := a.ScaleMM.Data[centre(iso)], b.ScaleMM.Data[centre(aniso)]
	if math.Abs(float64(sa)-s) > 0.1*s || math.Abs(float64(sb)-s) > 0.1*s {
		t.Fatalf("best scales %.3g (isotropic) and %.3g (anisotropic), want %v", sa, sb, s)
	}
}

// TestVesselness2D: Frangi's two-dimensional objectness on a line in an
// image (for example a DSA frame) picks the line's scale and ignores a disk.
// A line of Gaussian profile s in a 2-D image has a one-dimensional
// cross-section, whose normalized curvature σ²·s/(s²+σ²)^{3/2} peaks at
// σ = √2·s (a tube's two-dimensional cross-section peaks at σ = s).
func TestVesselness2D(t *testing.T) {
	const s = 2.0
	img := fillVolume([3]int{81, 81, 1}, [3]float64{0.5, 0.5, 0}, func(x, y, _ float64) float64 {
		line := 100 * math.Exp(-(x-8)*(x-8)/(2*s*s))
		disk := 100 * math.Exp(-((x+10)*(x+10)+y*y)/(2*s*s))
		return line + disk
	})
	res, err := Vesselness(context.Background(), img, VesselnessOptions{
		SigmasMM: denseSigmas(0.75, 6), ScalesOutput: true, Gamma: 5, ScaleByLargestEigenvalue: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	line := img.Index(40+16, 40, 0)
	disk := img.Index(40-20, 40, 0)
	if got, want := float64(res.ScaleMM.Data[line]), math.Sqrt2*s; math.Abs(got-want) > 0.1*want {
		t.Fatalf("line best σ %.3g, want %.3g", got, want)
	}
	// A disk has Rb = 1, which β = 0.5 weighs by e⁻².
	if res.Response.Data[disk] > 0.15*res.Response.Data[line] {
		t.Fatalf("disk %.4g vs line %.4g", res.Response.Data[disk], res.Response.Data[line])
	}
}

// TestITKHessian3DToVesselnessScenario re-expresses
// itkHessian3DToVesselnessMeasureImageFilterTest.cxx (ITK 35b196e1,
// Apache-2.0): an 8×8×8 zero image with a line of 100 along y at x = z = 3,
// σ = 0.5, α1 = 0.5, α2 = 2. ITK prints the output without assertions; here
// the line responds uniformly, symmetrically about it and more than any
// other voxel, and voxels two samples away respond much less.
func TestITKHessian3DToVesselnessScenario(t *testing.T) {
	src := NewVolume([3]int{8, 8, 8}, IsotropicSigma(1))
	for y := 0; y < 8; y++ {
		src.Data[src.Index(3, y, 3)] = 100
	}
	res, err := Vesselness(context.Background(), src, VesselnessOptions{
		Measure: VesselnessSato, SigmasMM: []float64{0.5}, Alpha1: 0.5, Alpha2: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := res.Response
	onLine := out.At(3, 0, 3)
	if !(onLine > 0) {
		t.Fatalf("line response %v", onLine)
	}
	for y := 0; y < 8; y++ {
		if math.Abs(float64(out.At(3, y, 3)-onLine)) > 1e-4*float64(onLine) {
			t.Fatalf("line response varies along y: %v vs %v", out.At(3, y, 3), onLine)
		}
		for z := 0; z < 8; z++ {
			for x := 0; x < 8; x++ {
				if (x != 3 || z != 3) && out.At(x, y, z) >= onLine {
					t.Fatalf("voxel (%d,%d,%d) responds %v ≥ line %v", x, y, z, out.At(x, y, z), onLine)
				}
			}
		}
	}
	if a, b := out.At(2, 4, 3), out.At(4, 4, 3); math.Abs(float64(a-b)) > 1e-4*float64(onLine) {
		t.Fatalf("asymmetric about the line: %v, %v", a, b)
	}
	if far := out.At(5, 4, 3); far > 0.2*onLine {
		t.Fatalf("two samples away %v, line %v", far, onLine)
	}
}

// phantom is a noisy synthetic angiography volume: tubes of several radii
// and directions, a plate and a blob.
func phantom(dims [3]int, spacing [3]float64, seed int64) *Volume {
	rng := rand.New(rand.NewSource(seed))
	v := fillVolume(dims, spacing, func(x, y, z float64) float64 {
		val := 0.0
		val += 300 * math.Exp(-(x*x+y*y)/(2*1.0*1.0))                 // along z
		val += 250 * math.Exp(-((y-4)*(y-4)+(z-3)*(z-3))/(2*2.0*2.0)) // along x
		d := (x + z) / 2
		val += 200 * math.Exp(-((x-d)*(x-d)+(z-d)*(z-d)+(y+5)*(y+5))/(2*1.5*1.5)) // oblique
		val += 400 * math.Exp(-(x-8)*(x-8)/(2*1.2*1.2))                           // plate
		val += 500 * math.Exp(-((x+7)*(x+7)+(y+7)*(y+7)+(z+4)*(z+4))/(2*2.5*2.5)) // blob
		return val
	})
	for i := range v.Data {
		v.Data[i] += float32(rng.NormFloat64() * 10)
	}
	return v
}

// TestVesselnessSlabsMatchWholeVolume: processing in thin slabs with the
// 6σ margin gives the whole-volume result, for fixed and automatic γ and for
// Sato, and the worker count does not change the result.
func TestVesselnessSlabsMatchWholeVolume(t *testing.T) {
	src := phantom([3]int{40, 36, 48}, [3]float64{0.6, 0.6, 0.8}, 1)
	for _, opts := range []VesselnessOptions{
		{SigmasMM: []float64{0.8, 1.6, 3}, ScalesOutput: true},
		{SigmasMM: []float64{0.8, 1.6, 3}, Gamma: 20, ScaleByLargestEigenvalue: true, ScalesOutput: true},
		{Measure: VesselnessSato, SigmasMM: []float64{0.8, 1.6, 3}, ScalesOutput: true},
	} {
		opts.Workers = 3
		whole, err := Vesselness(context.Background(), src, opts)
		if err != nil {
			t.Fatal(err)
		}
		if whole.SlabSlices != src.Dims[2] {
			t.Fatalf("whole volume split into %d-slice slabs", whole.SlabSlices)
		}
		sliced := opts
		sliced.MaxWorkingBytes = 5 * vesselnessSlabBuffers * int64(src.Dims[0]*src.Dims[1]) * 4
		sliced.Workers = 1
		slabs, err := Vesselness(context.Background(), src, sliced)
		if err != nil {
			t.Fatal(err)
		}
		if slabs.SlabSlices != 5 {
			t.Fatalf("slab of %d slices under a bound of 5 slices", slabs.SlabSlices)
		}
		peak := float32(0)
		for _, v := range whole.Response.Data {
			peak = max(peak, v)
		}
		for i := range whole.Response.Data {
			if d := math.Abs(float64(whole.Response.Data[i] - slabs.Response.Data[i])); d > 2e-3*float64(peak) {
				t.Fatalf("%v: voxel %d: slabs %v, whole %v (peak %v)", opts.Measure, i, slabs.Response.Data[i], whole.Response.Data[i], peak)
			}
		}
		for i, g := range whole.Gammas {
			if math.Abs(g-slabs.Gammas[i]) > 1e-3*g {
				t.Fatalf("γ per scale: slabs %v, whole %v", slabs.Gammas, whole.Gammas)
			}
		}
		// Same slabs, other worker count: identical.
		sliced.Workers = 7
		again, err := Vesselness(context.Background(), src, sliced)
		if err != nil {
			t.Fatal(err)
		}
		for i := range again.Response.Data {
			if again.Response.Data[i] != slabs.Response.Data[i] || again.ScaleMM.Data[i] != slabs.ScaleMM.Data[i] {
				t.Fatalf("worker count changed voxel %d", i)
			}
		}
	}
}

// TestVesselnessMemoryBound measures the heap Vesselness allocates in total
// (not only live): its outputs plus the reported working set, which stays
// within MaxWorkingBytes and the line scratch, whatever the number of scales
// and slabs (here up to 16 slabs, each computed twice for the automatic γ).
// The slack left for per-pass bookkeeping is below one volume-sized buffer,
// so any buffer allocated per slab or per scale fails the test.
func TestVesselnessMemoryBound(t *testing.T) {
	src := phantom([3]int{96, 96, 64}, IsotropicSigma(0.7), 2)
	n := int64(len(src.Data))
	sliceBytes := int64(vesselnessSlabBuffers * 96 * 96 * 4)
	const slack = 1 << 20
	if slack >= n*4 {
		t.Fatal("slack must stay below one volume buffer")
	}
	for _, bound := range []int64{0, 16 * sliceBytes, 4 * sliceBytes} {
		opts := VesselnessOptions{SigmasMM: []float64{0.7, 1.4, 2.8}, ScalesOutput: true, MaxWorkingBytes: bound, Workers: 4}
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		res, err := Vesselness(context.Background(), src, opts)
		runtime.ReadMemStats(&after)
		if err != nil {
			t.Fatal(err)
		}
		allocated := int64(after.TotalAlloc - before.TotalAlloc)
		outputs := 2 * n * 4
		limit := bound
		if limit == 0 {
			limit = DefaultVesselnessWorkingBytes
		}
		scratch := int64(opts.Workers) * int64(96*laneWidth+4*laneWidth) * 8
		if res.WorkingBytes > limit+scratch {
			t.Fatalf("bound %d: working set %d bytes", bound, res.WorkingBytes)
		}
		if allocated > outputs+res.WorkingBytes+slack {
			t.Fatalf("bound %d: allocated %d bytes, outputs %d + working %d", bound, allocated, outputs, res.WorkingBytes)
		}
		t.Logf("bound %d: %d-slice slabs, working %d bytes, allocated %d bytes (outputs %d)",
			bound, res.SlabSlices, res.WorkingBytes, allocated, outputs)
	}
}

// countdownContext reports cancellation after a number of Err calls, to
// check that long runs poll the context.
type countdownContext struct {
	context.Context
	left atomic.Int64
}

func (c *countdownContext) Err() error {
	if c.left.Add(-1) < 0 {
		return context.Canceled
	}
	return nil
}

func TestVesselnessCancellation(t *testing.T) {
	src := phantom([3]int{32, 32, 32}, IsotropicSigma(1), 3)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Vesselness(ctx, src, VesselnessOptions{SigmasMM: []float64{1}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context: %v", err)
	}
	for _, calls := range []int64{3, 20, 200} {
		c := &countdownContext{Context: context.Background()}
		c.left.Store(calls)
		res, err := Vesselness(c, src, VesselnessOptions{SigmasMM: []float64{1, 2}, MaxWorkingBytes: 1})
		if !errors.Is(err, context.Canceled) || res != nil {
			t.Fatalf("cancel after %d polls: %v, %v", calls, res, err)
		}
	}
}

func TestVesselnessInvalid(t *testing.T) {
	vol := NewVolume([3]int{8, 8, 8}, IsotropicSigma(1))
	img := NewVolume([3]int{8, 8, 1}, [3]float64{1, 1, 0})
	for name, tc := range map[string]struct {
		src  *Volume
		opts VesselnessOptions
	}{
		"no scale":      {vol, VesselnessOptions{}},
		"bad sigma":     {vol, VesselnessOptions{SigmasMM: []float64{1, -1}}},
		"sato 2-D":      {img, VesselnessOptions{Measure: VesselnessSato, SigmasMM: []float64{1}}},
		"plate 2-D":     {img, VesselnessOptions{Structure: StructurePlate, SigmasMM: []float64{1}}},
		"bad structure": {vol, VesselnessOptions{Structure: 9, SigmasMM: []float64{1}}},
		"bad measure":   {vol, VesselnessOptions{Measure: 9, SigmasMM: []float64{1}}},
		"bad alpha":     {vol, VesselnessOptions{Alpha: -1, SigmasMM: []float64{1}}},
		"bad gamma":     {vol, VesselnessOptions{Gamma: math.NaN(), SigmasMM: []float64{1}}},
		"bad alpha2":    {vol, VesselnessOptions{Measure: VesselnessSato, Alpha2: math.Inf(1), SigmasMM: []float64{1}}},
		"bad bound":     {vol, VesselnessOptions{MaxWorkingBytes: -1, SigmasMM: []float64{1}}},
		"bad volume":    {&Volume{Dims: [3]int{2, 2, 2}, SpacingMM: IsotropicSigma(1)}, VesselnessOptions{SigmasMM: []float64{1}}},
	} {
		if _, err := Vesselness(context.Background(), tc.src, tc.opts); !errors.Is(err, ErrInvalidVolume) {
			t.Errorf("%s: error %v", name, err)
		}
	}
	// A flat volume has no structure: zero response, γ 0.
	res, err := Vesselness(context.Background(), vol, VesselnessOptions{SigmasMM: []float64{1}, ScalesOutput: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := range res.Response.Data {
		if res.Response.Data[i] != 0 || res.ScaleMM.Data[i] != 0 {
			t.Fatal("flat volume responds")
		}
	}
	if res.Gammas[0] != 0 {
		t.Fatalf("flat volume γ %v", res.Gammas)
	}
}

func BenchmarkVesselness(b *testing.B) {
	src := phantom([3]int{128, 128, 128}, IsotropicSigma(0.7), 4)
	for _, tc := range []struct {
		name string
		opts VesselnessOptions
	}{
		{"frangi-auto-gamma", VesselnessOptions{SigmasMM: []float64{1, 2, 4}}},
		{"frangi-auto-gamma-slabs", VesselnessOptions{SigmasMM: []float64{1, 2, 4}, MaxWorkingBytes: 16 << 20}},
		{"sato", VesselnessOptions{Measure: VesselnessSato, SigmasMM: []float64{1, 2, 4}}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, err := Vesselness(context.Background(), src, tc.opts); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
