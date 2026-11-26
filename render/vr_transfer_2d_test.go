package render

import (
	"bytes"
	"context"
	"errors"
	"image"
	"math"
	"testing"
)

// TestVRTransfer2DConstantTableReproducesThe1DFrame (#1232): a table
// constant along the gradient axis, sampled from a 1D transfer function as
// DVR classifies with it, renders the 1D frame. With 1024 value columns the
// shaded phantom frames agree within a tenth of an 8-bit level on average
// (99th percentile 1); the editor's 256 columns stay within half a level.
func TestVRTransfer2DConstantTableReproducesThe1DFrame(t *testing.T) {
	vol, err := BuildVolume(ctPhantomStack(64, 64, 48))
	if err != nil {
		t.Fatal(err)
	}
	minimum, maximum, ok := vol.HURange()
	if !ok {
		t.Fatal("no HU range")
	}
	cam := NewVRCamera(vol.BoundingRadiusMM())
	cam.FitVolume(vol)
	cam.Rotate(25, -10)
	bonesSkin := DefaultVRPreset()
	bonesSkin.GradientOpacityScale = 0
	clinical := VRPreset{Name: "muscles-bones", TF: NewMusclesBonesVRTransferFunction(VROpacityMappingLogarithmicInverse), Mode: VRModeDVR}
	for _, c := range []struct {
		name    string
		preset  VRPreset
		window  WindowLevel
		columns int
		mean    float64
		p99     float64
	}{
		{"legacy HU, 1024 columns", bonesSkin, WindowLevel{Center: 40, Width: 400}, 1024, 0.1, 1},
		{"normalized, 1024 columns", clinical, WindowLevel{Center: 300, Width: 1200}, 1024, 0.1, 1},
		{"legacy HU, editor size", bonesSkin, WindowLevel{Center: 40, Width: 400}, DefaultVRTransfer2DWidth, 0.5, 4},
	} {
		for _, shading := range []bool{false, true} {
			quality := VRQuality{Width: 64, Height: 64, MaxSteps: DefaultVRMaxSteps}
			want := RenderVR(vol, cam, c.preset, c.window, shading, nil, quality).(*image.NRGBA)
			table, err := VRTransfer2DFromTransferFunction(c.preset.TF, c.window, c.columns, 4, minimum, maximum, 500)
			if err != nil {
				t.Fatal(err)
			}
			preset := c.preset
			preset.Transfer2D = table
			got := RenderVR(vol, cam, preset, c.window, shading, nil, quality).(*image.NRGBA)
			mean, p99 := vrImageError(got, want)
			if mean > c.mean || p99 > c.p99 {
				t.Errorf("%s shading %v: 2D vs 1D mean %.3f p99 %.0f, want <= %.2f/%.0f", c.name, shading, mean, p99, c.mean, c.p99)
			}
		}
	}
}

// uniformCubeStack is n³ voxels of air (0) holding a centred cube of value
// 1000 with half-side half voxels: a homogeneous interior with a sharp edge.
func uniformCubeStack(n, half int) *Stack {
	center := n / 2
	return stack16(n, false, func(int) Rescale { return Rescale{Slope: 1} }, func(x, y, z int) int {
		if abs(x-center) < half && abs(y-center) < half && abs(z-center) < half {
			return 1000
		}
		return 0
	})
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// TestVRTransfer2DSeparatesEdgesFromInteriors (#1232): the cube's interior
// and its edge have the same value, so no 1D function can colour them
// apart; a 2D table can. Interior-only red renders a red centre, edge-only
// green a green rim with the interior seen through it.
func TestVRTransfer2DSeparatesEdgesFromInteriors(t *testing.T) {
	vol, err := BuildVolume(uniformCubeStack(32, 10))
	if err != nil {
		t.Fatal(err)
	}
	cam := NewVRCamera(vol.BoundingRadiusMM())
	cam.FitVolume(vol)
	cam.Projection = VRProjectionParallel
	window := WindowLevel{Center: 500, Width: 1000}
	interior := VRTransfer2DRegion{IntensityMin: 900, IntensityMax: 1100, GradientMin: 0, GradientMax: 20, R: 1, Opacity: 0.2}
	edge := VRTransfer2DRegion{IntensityMin: 100, IntensityMax: 1000, GradientMin: 100, GradientMax: 600, G: 1, Opacity: 0.3}
	render := func(regions ...VRTransfer2DRegion) *image.NRGBA {
		table, err := BakeVRTransfer2DRegions(regions, DefaultVRTransfer2DWidth, DefaultVRTransfer2DHeight, 0, 1000, 600)
		if err != nil {
			t.Fatal(err)
		}
		preset := opaqueAbovePreset()
		preset.Transfer2D = table
		return RenderVR(vol, cam, preset, window, false, nil, VRQuality{Width: 48, Height: 48, MaxSteps: DefaultVRMaxSteps}).(*image.NRGBA)
	}
	center := func(img *image.NRGBA) (r, g uint8) {
		c := img.NRGBAAt(24, 24)
		return c.R, c.G
	}
	if r, g := center(render(interior)); r < 128 || g != 0 {
		t.Fatalf("interior-only centre r=%d g=%d, want red", r, g)
	}
	// The edge-only table leaves the interior transparent: the centre ray
	// crosses only the two faces, the projected rim of the cube is greener.
	edgeOnly := render(edge)
	if r, g := center(edgeOnly); r != 0 || g == 0 {
		t.Fatalf("edge-only centre r=%d g=%d, want green", r, g)
	}
	// Both regions: the centre is the edge's green in front of the red.
	if r, g := center(render(interior, edge)); r == 0 || g == 0 {
		t.Fatalf("combined centre r=%d g=%d", r, g)
	}
	// The 1D function cannot tell them apart: its colour at 1000 is the
	// same at every gradient.
	preset := opaqueAbovePreset()
	oneD := RenderVR(vol, cam, preset, window, false, nil, VRQuality{Width: 48, Height: 48, MaxSteps: DefaultVRMaxSteps}).(*image.NRGBA)
	if c := oneD.NRGBAAt(24, 24); c.R != c.G {
		t.Fatalf("1D centre %+v is not gray", c)
	}
}

// TestVRTransfer2DSpaceLeapingKeepsTheImage: the blocks whose values are
// transparent at every gradient are skipped without changing a pixel.
func TestVRTransfer2DSpaceLeapingKeepsTheImage(t *testing.T) {
	vol, err := BuildVolume(ctPhantomStack(37, 29, 21))
	if err != nil {
		t.Fatal(err)
	}
	minimum, maximum, _ := vol.HURange()
	table, err := BakeVRTransfer2DRegions([]VRTransfer2DRegion{
		{IntensityMin: 900, IntensityMax: maximum, GradientMin: 0, GradientMax: 400, R: 1, G: 0.9, B: 0.8, Opacity: 0.7, Softness: 0.3},
		{IntensityMin: -200, IntensityMax: 100, GradientMin: 20, GradientMax: 300, R: 0.8, G: 0.4, B: 0.3, Opacity: 0.4, Softness: 0.5},
	}, DefaultVRTransfer2DWidth, DefaultVRTransfer2DHeight, minimum, maximum, 400)
	if err != nil {
		t.Fatal(err)
	}
	preset := DefaultVRPreset()
	preset.Transfer2D = table
	cam := NewVRCamera(vol.BoundingRadiusMM())
	cam.FitVolume(vol)
	cam.Rotate(30, 12)
	window := WindowLevel{Center: 40, Width: 400}
	for _, shading := range []bool{false, true} {
		got := RenderVR(vol, cam, preset, window, shading, nil, DefaultVRQuality(36, 30)).(*image.NRGBA)
		want := RenderVR(vol, cam, preset, window, shading, fullMarch(nil), DefaultVRQuality(36, 30)).(*image.NRGBA)
		if !bytes.Equal(got.Pix, want.Pix) {
			t.Errorf("shading %v: image differs from the full march", shading)
		}
	}
	prepared, err := vol.PrepareVRVolume(VRPrefilterNone)
	if err != nil {
		t.Fatal(err)
	}
	sampler, err := prepared.newSampler()
	if err != nil {
		t.Fatal(err)
	}
	defer sampler.Close()
	lut, effective, dataMin, dataMax := vrTransferSetup(prepared, preset, window)
	leap, err := newRenderLeap(context.Background(), prepared, sampler, preset, lut, effective, dataMin, dataMax)
	if err != nil {
		t.Fatal(err)
	}
	empty := 0
	for _, skipped := range leap.empty {
		if skipped {
			empty++
		}
	}
	if empty == 0 {
		t.Fatal("no block of air is skipped under the 2D transfer function")
	}
}

func TestVRTransfer2DLookupInterpolatesAndClamps(t *testing.T) {
	// 3 values (0, 50, 100) by 2 gradients (0, 10): alpha = column/2 + row/2.
	table := make([]RGBA, 6)
	for row := 0; row < 2; row++ {
		for column := 0; column < 3; column++ {
			table[row*3+column] = RGBA{R: float64(column) / 2, G: float64(row), A: float64(column)/4 + float64(row)/2}
		}
	}
	transfer, err := NewVRTransfer2D(3, 2, 0, 100, 10, table)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		value, gradient float64
		want            RGBA
	}{
		{0, 0, RGBA{}},
		{25, 5, RGBA{R: 0.25, G: 0.5, A: 0.125 + 0.25}},
		{100, 10, RGBA{R: 1, G: 1, A: 1}},
		{-50, -1, RGBA{}},
		{500, 99, RGBA{R: 1, G: 1, A: 1}},
		{math.NaN(), math.NaN(), RGBA{}},
	} {
		got := transfer.Lookup(c.value, c.gradient)
		if math.Abs(got.R-c.want.R) > 1e-12 || math.Abs(got.G-c.want.G) > 1e-12 || math.Abs(got.A-c.want.A) > 1e-12 {
			t.Errorf("Lookup(%v, %v) = %+v, want %+v", c.value, c.gradient, got, c.want)
		}
		if bound := transfer.maxAlphaAt(c.value); bound < got.A-1e-12 {
			t.Errorf("maxAlphaAt(%v) = %v below %v", c.value, bound, got.A)
		}
	}
	if w, h := transfer.Size(); w != 3 || h != 2 || transfer.GradientMax() != 10 || transfer.At(2, 1).A != 1 || transfer.At(3, 0) != (RGBA{}) {
		t.Fatal("accessors")
	}
	table[0].R = 0.9
	if transfer.At(0, 0).R != 0 {
		t.Fatal("the table aliases the caller's slice")
	}
}

func TestNewVRTransfer2DRejectsMalformedTables(t *testing.T) {
	good := make([]RGBA, 4)
	for name, build := range map[string]func() (*VRTransfer2D, error){
		"narrow":     func() (*VRTransfer2D, error) { return NewVRTransfer2D(1, 4, 0, 1, 1, good) },
		"huge":       func() (*VRTransfer2D, error) { return NewVRTransfer2D(MaxVRTransfer2DSide+1, 2, 0, 1, 1, good) },
		"range":      func() (*VRTransfer2D, error) { return NewVRTransfer2D(2, 2, 1, 1, 1, good) },
		"gradient":   func() (*VRTransfer2D, error) { return NewVRTransfer2D(2, 2, 0, 1, 0, good) },
		"length":     func() (*VRTransfer2D, error) { return NewVRTransfer2D(2, 2, 0, 1, 1, good[:3]) },
		"channel":    func() (*VRTransfer2D, error) { return NewVRTransfer2D(2, 2, 0, 1, 1, []RGBA{{A: 2}, {}, {}, {}}) },
		"nan":        func() (*VRTransfer2D, error) { return NewVRTransfer2D(2, 2, math.NaN(), 1, 1, good) },
		"no regions": func() (*VRTransfer2D, error) { return BakeVRTransfer2DRegions(nil, 4, 4, 0, 1, 1) },
		"region": func() (*VRTransfer2D, error) {
			return BakeVRTransfer2DRegions([]VRTransfer2DRegion{{IntensityMin: 1, IntensityMax: 0, GradientMax: 1}}, 4, 4, 0, 1, 1)
		},
		"region colour": func() (*VRTransfer2D, error) {
			return BakeVRTransfer2DRegions([]VRTransfer2DRegion{{IntensityMax: 1, GradientMax: 1, R: 1.5}}, 4, 4, 0, 1, 1)
		},
		"too many regions": func() (*VRTransfer2D, error) {
			regions := make([]VRTransfer2DRegion, MaxVRTransfer2DRegions+1)
			for i := range regions {
				regions[i] = VRTransfer2DRegion{IntensityMax: 1, GradientMax: 1}
			}
			return BakeVRTransfer2DRegions(regions, 4, 4, 0, 1, 1)
		},
	} {
		if transfer, err := build(); transfer != nil || !errors.Is(err, ErrInvalidVRTransfer2D) {
			t.Errorf("%s: %v, %v", name, transfer, err)
		}
	}
}

// TestBakeVRTransfer2DRegions: inside a hard region the table is its colour
// (sRGB decoded once) and opacity; overlaps combine as independent layers; a
// soft region ramps from its inner edges but not from an edge on the domain.
func TestBakeVRTransfer2DRegions(t *testing.T) {
	red := VRTransfer2DRegion{IntensityMin: 0, IntensityMax: 50, GradientMin: 0, GradientMax: 100, R: 1, Opacity: 0.5}
	blue := VRTransfer2DRegion{IntensityMin: 25, IntensityMax: 100, GradientMin: 0, GradientMax: 100, B: 0.5, Opacity: 0.5}
	transfer, err := BakeVRTransfer2DRegions([]VRTransfer2DRegion{red, blue}, 5, 3, 0, 100, 100)
	if err != nil {
		t.Fatal(err)
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-12 }
	if e := transfer.At(0, 1); !near(e.R, 1) || e.B != 0 || !near(e.A, 0.5) {
		t.Fatalf("red only %+v", e)
	}
	if e := transfer.At(4, 1); e.R != 0 || !near(e.B, srgbToLinear(0.5)) || !near(e.A, 0.5) {
		t.Fatalf("blue only %+v", e)
	}
	// Column 1 (25) and 2 (50) are in both: alpha 1-(0.5·0.5), equal mix.
	if e := transfer.At(2, 1); !near(e.A, 0.75) || !near(e.R, 0.5) || !near(e.B, srgbToLinear(0.5)/2) {
		t.Fatalf("overlap %+v", e)
	}
	soft := VRTransfer2DRegion{IntensityMin: 0, IntensityMax: 80, GradientMin: 0, GradientMax: 100, G: 1, Opacity: 1, Softness: 1}
	transfer, err = BakeVRTransfer2DRegions([]VRTransfer2DRegion{soft}, 5, 2, 0, 100, 100)
	if err != nil {
		t.Fatal(err)
	}
	// Columns 0, 25, 50, 75, 100: value 0 is on the domain edge (full), the
	// upper edge at 80 ramps over 40 (50 → 0.75, 75 → 0.125), 100 is out.
	// Row 0 (gradient 0) lies on the domain edge and is not softened.
	for column, want := range []float64{1, 1, 0.75, 0.125, 0} {
		if got := transfer.At(column, 0).A; !near(got, want) {
			t.Errorf("soft column %d alpha %v, want %v", column, got, want)
		}
	}
}

// TestVRJointHistogramCountsValueAndGradient: a ramp of 10 per voxel along
// x on 1 mm voxels has a 10 HU/mm gradient inside and 5 HU/mm at the two
// clamped faces; every voxel is counted once in its value column.
func TestVRJointHistogramCountsValueAndGradient(t *testing.T) {
	const n = 16
	vol, err := BuildVolume(stack16(n, false, func(int) Rescale { return Rescale{Slope: 1} }, func(x, y, z int) int { return 10 * x }))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := vol.PrepareVRVolume(VRPrefilterNone)
	if err != nil {
		t.Fatal(err)
	}
	histogram, err := prepared.JointHistogramContext(context.Background(), n, 4, 20)
	if err != nil {
		t.Fatal(err)
	}
	if histogram.IntensityMin != 0 || histogram.IntensityMax != 150 || histogram.Samples != n*n*n || histogram.Stride != 1 {
		t.Fatalf("histogram %+v", histogram)
	}
	var total uint64
	for row := 0; row < 4; row++ {
		for column := 0; column < n; column++ {
			count := histogram.At(column, row)
			total += count
			want := uint64(0)
			switch {
			case (column == 0 || column == n-1) && row == 1:
				want = n * n // the faces: 5 HU/mm
			case column > 0 && column < n-1 && row == 2:
				want = n * n // the interior: 10 HU/mm
			}
			if count != want {
				t.Errorf("bin (%d, %d) = %d, want %d", column, row, count, want)
			}
		}
	}
	if total != histogram.Samples {
		t.Fatalf("counted %d of %d", total, histogram.Samples)
	}
	auto, err := prepared.JointHistogramContext(context.Background(), 8, 8, 0)
	if err != nil || auto.GradientMax != 10 {
		t.Fatalf("automatic gradient maximum %v, %v", auto.GradientMax, err)
	}
	if _, err := prepared.JointHistogramContext(context.Background(), 1, 8, 0); err == nil {
		t.Fatal("one value bin accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := prepared.JointHistogramContext(ctx, 8, 8, 0); err == nil {
		t.Fatal("a canceled histogram returned no error")
	}
}

// BenchmarkRenderVRTransfer2D compares DVR of the 192x192x128 CT phantom
// with the bones-and-skin 1D function and with a two-region 2D table.
func BenchmarkRenderVRTransfer2D(b *testing.B) {
	vol, err := BuildVolume(ctPhantomStack(192, 192, 128))
	if err != nil {
		b.Fatal(err)
	}
	minimum, maximum, _ := vol.HURange()
	cam := NewVRCamera(vol.BoundingRadiusMM())
	cam.FitVolume(vol)
	window := WindowLevel{Center: 40, Width: 400}
	table, err := BakeVRTransfer2DRegions([]VRTransfer2DRegion{
		{IntensityMin: 900, IntensityMax: maximum, GradientMin: 0, GradientMax: 400, R: 1, G: 0.9, B: 0.8, Opacity: 0.7, Softness: 0.3},
		{IntensityMin: -200, IntensityMax: 100, GradientMin: 20, GradientMax: 300, R: 0.8, G: 0.4, B: 0.3, Opacity: 0.4, Softness: 0.5},
	}, DefaultVRTransfer2DWidth, DefaultVRTransfer2DHeight, minimum, maximum, 400)
	if err != nil {
		b.Fatal(err)
	}
	twoD := DefaultVRPreset()
	twoD.Transfer2D = table
	for _, c := range []struct {
		name   string
		preset VRPreset
	}{{"1d", DefaultVRPreset()}, {"2d", twoD}} {
		b.Run(c.name, func(b *testing.B) {
			quality := DefaultVRQuality(160, 160)
			benchVRImage = RenderVR(vol, cam, c.preset, window, true, nil, quality)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				benchVRImage = RenderVR(vol, cam, c.preset, window, true, nil, quality)
			}
		})
	}
}
