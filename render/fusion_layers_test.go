package render

import (
	"context"
	"errors"
	"image"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

// scalarRow is a width×1 plane of values, all sampled unless NaN.
func scalarRow(values ...float64) *ScalarPlane {
	plane := &ScalarPlane{width: len(values), height: 1, values: values, valid: make([]byte, len(values)), photometric: "MONOCHROME2"}
	for i, v := range values {
		if v == v {
			plane.valid[i] = 1
		}
	}
	return plane
}

func rgbaAt(t *testing.T, img image.Image, x int) [4]uint8 {
	t.Helper()
	rgba, ok := img.(*image.RGBA)
	if !ok {
		t.Fatalf("composite is %T", img)
	}
	i := x * 4
	return [4]uint8{rgba.Pix[i], rgba.Pix[i+1], rgba.Pix[i+2], rgba.Pix[i+3]}
}

var fusionTestWindow = WindowLevel{Center: 50, Width: 100}

// TestFusionTransparentBelowKeepsTheBase (#1069): values under the overlay's
// window floor draw nothing, so low uptake does not tint the base; values
// within the window still blend.
func TestFusionTransparentBelowKeepsTheBase(t *testing.T) {
	base := scalarRow(50, 50, 50)
	overlay := scalarRow(-20, 60, 100)
	layers := &FusedScalarLayers{Planes: []*ScalarPlane{base, overlay}}
	grayBase := FusionLayerPresentation{Window: fusionTestWindow, Alpha: 1}
	hot := FusionLayerPresentation{Window: fusionTestWindow, Palette: display.HotIronLUT(), Alpha: 0.5}
	tinted, err := layers.Composite(context.Background(), []FusionLayerPresentation{grayBase, hot}, FusionCompositeMode{})
	if err != nil {
		t.Fatal(err)
	}
	baseGray := rgbaAt(t, tinted, 0)
	// Without the threshold, Hot Iron's black darkens the base below range.
	if baseGray[0] >= 120 {
		t.Fatalf("below-range overlay did not tint: %v", baseGray)
	}
	hot.TransparentBelow = true
	clean, err := layers.Composite(context.Background(), []FusionLayerPresentation{grayBase, hot}, FusionCompositeMode{})
	if err != nil {
		t.Fatal(err)
	}
	if got := rgbaAt(t, clean, 0); got[0] != got[1] || got[1] != got[2] || got[0] < 125 || got[0] > 130 {
		t.Fatalf("below-range pixel = %v, want the base's mid-gray", got)
	}
	if rgbaAt(t, clean, 2) != rgbaAt(t, tinted, 2) {
		t.Fatal("in-range pixels changed with the threshold")
	}
	// The same semantics over an already presented 2D image.
	over, err := overlay.CompositeOver(context.Background(), image.NewGray(image.Rect(0, 0, 3, 1)), hot)
	if err != nil {
		t.Fatal(err)
	}
	if got := rgbaAt(t, over, 0); got != [4]uint8{0, 0, 0, 255} {
		t.Fatalf("CompositeOver below range = %v, want the base", got)
	}
}

func TestFusionOpacityRampAndBands(t *testing.T) {
	base := scalarRow(0, 0, 0, 0)
	overlay := scalarRow(0, 50, 100, 30)
	layers := &FusedScalarLayers{Planes: []*ScalarPlane{base, overlay}}
	black := FusionLayerPresentation{Window: fusionTestWindow, Alpha: 1}
	ramp := FusionLayerPresentation{Window: fusionTestWindow, Alpha: 1,
		OpacityRamp: []FusionOpacityPoint{{Position: 0, Opacity: 0}, {Position: 1, Opacity: 1}}}
	img, err := layers.Composite(context.Background(), []FusionLayerPresentation{black, ramp}, FusionCompositeMode{})
	if err != nil {
		t.Fatal(err)
	}
	// Opacity = window coordinate: 0 → nothing, 0.5 → half of 50% gray,
	// 1 → white.
	if got := rgbaAt(t, img, 0); got[0] != 0 {
		t.Fatalf("ramp at the floor = %v", got)
	}
	if got := rgbaAt(t, img, 1); got[0] < 60 || got[0] > 68 {
		t.Fatalf("ramp at the middle = %v, want about 64", got)
	}
	if got := rgbaAt(t, img, 2); got[0] != 255 {
		t.Fatalf("ramp at the ceiling = %v", got)
	}
	banded := FusionLayerPresentation{Window: fusionTestWindow, Alpha: 1, Bands: 3}
	img, err = layers.Composite(context.Background(), []FusionLayerPresentation{black, banded}, FusionCompositeMode{})
	if err != nil {
		t.Fatal(err)
	}
	// Three bands: 0, 128 and 255 only; 30% lands in the first.
	for x, want := range []uint8{0, 128, 255, 0} {
		if got := rgbaAt(t, img, x)[0]; got != want {
			t.Fatalf("band of pixel %d = %d, want %d", x, got, want)
		}
	}
	if _, err := layers.Composite(context.Background(), []FusionLayerPresentation{black, {Alpha: 1,
		OpacityRamp: []FusionOpacityPoint{{Position: 1}, {Position: 0}}}}, FusionCompositeMode{}); err == nil {
		t.Fatal("unsorted ramp accepted")
	}
}

// TestFusionThreeLayersCompositeInOrder: layers stack source-over in order.
func TestFusionThreeLayersCompositeInOrder(t *testing.T) {
	planes := &FusedScalarLayers{Planes: []*ScalarPlane{scalarRow(100, 100), scalarRow(100, 100), scalarRow(100, -1)}}
	white := FusionLayerPresentation{Window: fusionTestWindow, Alpha: 1}
	red := FusionLayerPresentation{Window: fusionTestWindow, Alpha: 1, Palette: display.HotIronLUT()}
	top := FusionLayerPresentation{Window: fusionTestWindow, Alpha: 0.5, Palette: display.HotMetalBlueLUT(), TransparentBelow: true}
	img, err := planes.Composite(context.Background(), []FusionLayerPresentation{white, red, top}, FusionCompositeMode{})
	if err != nil {
		t.Fatal(err)
	}
	// Pixel 1: the top layer is below range, so the opaque second layer
	// (Hot Iron at its ceiling, white) shows; pixel 0 blends the top over it.
	if got := rgbaAt(t, img, 1); got != [4]uint8{255, 255, 255, 255} {
		t.Fatalf("pixel under a transparent top layer = %v", got)
	}
	r, g, b := display.HotMetalBlueLUT().At(255)
	want := [3]uint8{uint8((int(r) + 255 + 1) / 2), uint8((int(g) + 255 + 1) / 2), uint8((int(b) + 255 + 1) / 2)}
	if got := rgbaAt(t, img, 0); abs8(got[0], want[0]) > 1 || abs8(got[1], want[1]) > 1 || abs8(got[2], want[2]) > 1 {
		t.Fatalf("three-layer pixel = %v, want %v", got, want)
	}
}

func abs8(a, b uint8) int {
	if a > b {
		return int(a - b)
	}
	return int(b - a)
}

func TestFusionCheckerboardSwipeAndDifference(t *testing.T) {
	const width = 8
	first, second := make([]float64, width), make([]float64, width)
	for i := range first {
		first[i], second[i] = 0, 100
	}
	second[width-1] = 0 // equal values: zero difference
	layers := &FusedScalarLayers{Planes: []*ScalarPlane{scalarRow(first...), scalarRow(second...)}}
	gray := []FusionLayerPresentation{{Window: fusionTestWindow, Alpha: 1}, {Window: fusionTestWindow, Alpha: 0.3}}
	compose := func(mode FusionCompositeMode) image.Image {
		t.Helper()
		img, err := layers.Composite(context.Background(), gray, mode)
		if err != nil {
			t.Fatal(err)
		}
		return img
	}
	checker := compose(FusionCompositeMode{Kind: FusionCheckerboard, CheckerSizePx: 2})
	for x, want := range []uint8{0, 0, 255, 255, 0, 0, 255} {
		if got := rgbaAt(t, checker, x); got[0] != want || got[3] != 255 {
			t.Fatalf("checkerboard pixel %d = %v, want %d opaque", x, got, want)
		}
	}
	swipe := compose(FusionCompositeMode{Kind: FusionSwipe, SwipePosition: 0.25})
	for x, want := range []uint8{0, 0, 255, 255} {
		if got := rgbaAt(t, swipe, x)[0]; got != want {
			t.Fatalf("swipe pixel %d = %d, want %d", x, got, want)
		}
	}
	difference := compose(FusionCompositeMode{Kind: FusionDifference, DifferenceRange: 100})
	warm := rgbaAt(t, difference, 0)
	neutral := rgbaAt(t, difference, width-1)
	if r, _, b := display.CoolWarmLUT().At(255); warm[0] != r || warm[2] != b {
		t.Fatalf("difference +100 = %v, want the warm end", warm)
	}
	if neutral[0] < 210 || neutral[2] < 210 {
		t.Fatalf("zero difference = %v, want neutral", neutral)
	}
	if _, err := (&FusedScalarLayers{Planes: []*ScalarPlane{scalarRow(1)}}).Composite(context.Background(),
		gray[:1], FusionCompositeMode{Kind: FusionSwipe}); err == nil {
		t.Fatal("comparison of one layer accepted")
	}
}

// TestResliceFusedMPRLayersMatchesPairwiseReslices: N-layer reslicing gives
// each overlay the plane its own two-layer reslice gives.
func TestResliceFusedMPRLayersMatchesPairwiseReslices(t *testing.T) {
	base, err := BuildVolume(gradientXZStack(3, 4, 3))
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	shifted := gradientXZStack(3, 4, 3)
	for _, frame := range shifted.Frames {
		frame.ImagePosition[0] = 10
	}
	overlay, err := BuildVolume(shifted)
	if err != nil {
		t.Fatal(err)
	}
	defer overlay.Close()
	plane := base.OrthogonalPlane(MPRPlaneAxial, Vec3{Z: 1})
	shift := func(point Vec3) (Vec3, error) { return point.Add(Vec3{X: 10}), nil }
	layers, err := ResliceFusedMPRLayersContext(context.Background(), FusedMPRLayersRequest{
		Base: base, TargetPlane: plane, Width: 4, Height: 3,
		Overlays: []FusedMPROverlay{{Volume: base}, {Volume: overlay, TargetToSource: shift}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(layers.Planes) != 3 || len(layers.Reports) != 2 {
		t.Fatalf("%d planes, %d reports", len(layers.Planes), len(layers.Reports))
	}
	pair, err := ResliceFusedMPRScalarContext(context.Background(), FusedMPRRequest{
		Base: base, Overlay: overlay, TargetPlane: plane, Width: 4, Height: 3, TargetToSource: shift,
	})
	if err != nil {
		t.Fatal(err)
	}
	for y := 0; y < 3; y++ {
		for x := 0; x < 4; x++ {
			a, _ := layers.Planes[2].ValueAt(x, y)
			b, _ := pair.Overlay.ValueAt(x, y)
			c, _ := layers.Planes[0].ValueAt(x, y)
			d, _ := pair.Base.ValueAt(x, y)
			if a != b || c != d {
				t.Fatalf("pixel %d,%d: layers %v/%v, pair %v/%v", x, y, c, a, d, b)
			}
		}
	}
	miss := func(point Vec3) (Vec3, error) { return point.Add(Vec3{X: 1000}), nil }
	if _, err := ResliceFusedMPRLayersContext(context.Background(), FusedMPRLayersRequest{
		Base: base, TargetPlane: plane, Width: 4, Height: 3,
		Overlays: []FusedMPROverlay{{Volume: overlay, TargetToSource: miss}},
	}); !errors.Is(err, ErrFusionNoOverlap) {
		t.Fatalf("missing overlay err = %v", err)
	}
}
