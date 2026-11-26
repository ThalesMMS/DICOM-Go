package gsps

import (
	"errors"
	"image"
	"image/color"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/dicomcolor"
)

func TestCompoundPaintClippedDashesUseColorAndOpacity(t *testing.T) {
	red := dicomcolor.RGBToCIELab([3]int{240, 10, 20})
	blue := dicomcolor.RGBToCIELab([3]int{20, 30, 230})
	pattern, half, quarter := uint32(0xAAAAAAAA), .5, .25
	dst := image.NewNRGBA(image.Rect(2, 4, 12, 11))
	graphic := &CompoundGraphic{
		LineStyle: &CompoundLineStyle{
			OnColor: &red, OnOpacity: &half, OffColor: &blue, OffOpacity: &quarter,
			Dashing: "DASHED", Pattern: &pattern,
		},
	}
	// The first visible pixel is three pixels after the start, so it
	// receives the off color even though the path is clipped at X=2.
	path := []Point2D{{X: -.5, Y: 5.5}, {X: 13.5, Y: 5.5}}
	budget := int64(1 << 14)
	err := paintCompoundPaths(dst, [][]Point2D{path}, false, graphic, color.NRGBA{G: 255, A: 255}, 1, &budget)
	if err != nil {
		t.Fatal(err)
	}
	for x := 2; x < 12; x++ {
		want := color.NRGBA{R: 240, G: 10, B: 20, A: 128}
		if x%2 == 0 {
			want = color.NRGBA{R: 20, G: 30, B: 230, A: 64}
		}
		if p := dst.NRGBAAt(x, 5); !compoundPaintNearColor(p, want) {
			t.Fatalf("dash pixel (%d,5) = %+v, want %+v", x, p, want)
		}
	}
	if p := dst.NRGBAAt(3, 6); p != (color.NRGBA{}) {
		t.Fatalf("uncovered pixel = %+v", p)
	}
}

func TestCompoundPaintStippleUsesDisplayPixels(t *testing.T) {
	dst := image.NewNRGBA(image.Rect(0, 0, 40, 40))
	filled, none, opaque := true, 0.0, 1.0
	red := dicomcolor.RGBToCIELab([3]int{240, 10, 20})
	green := dicomcolor.RGBToCIELab([3]int{10, 220, 30})
	pattern := make([]byte, 128)
	pattern[4] = 0x40 // row 1, column 1, MSB first
	state := &CompoundGraphic{
		Filled:    &filled,
		LineStyle: &CompoundLineStyle{OnOpacity: &none},
		FillStyle: &CompoundFillStyle{
			OnColor: &red, OnOpacity: &opaque, OffColor: &green, OffOpacity: &opaque,
			Mode: "STIPPELED", Pattern: pattern,
		},
	}
	path := []Point2D{{0, 0}, {39, 0}, {39, 39}, {0, 39}, {0, 0}}
	budget := int64(1 << 14)
	if err := paintCompoundPaths(dst, [][]Point2D{path}, true, state, color.NRGBA{}, 1, &budget); err != nil {
		t.Fatal(err)
	}
	for _, p := range []image.Point{{1, 1}, {33, 1}, {1, 33}, {33, 33}} {
		if got := dst.NRGBAAt(p.X, p.Y); !compoundPaintNearColor(got, color.NRGBA{R: 240, G: 10, B: 20, A: 255}) {
			t.Fatalf("on bit at %v = %+v", p, got)
		}
	}
	got := dst.NRGBAAt(2, 1)
	if !compoundPaintNearColor(got, color.NRGBA{R: 10, G: 220, B: 30, A: 255}) || dst.NRGBAAt(39, 39).A != 0 {
		t.Fatalf("off bit or clipped edge incorrect: %+v, %+v", got, dst.NRGBAAt(39, 39))
	}
}

func TestCompoundPaintStrokeJoinsAndSolidShadow(t *testing.T) {
	dst := image.NewGray(image.Rect(0, 0, 24, 24))
	half, dy := .5, 3.0
	white := dicomcolor.RGBToCIELab([3]int{255, 255, 255})
	pattern := uint32(0xAAAAAAAA)
	state := &CompoundGraphic{LineStyle: &CompoundLineStyle{
		OnColor: &white, OnOpacity: &half, Dashing: "DASHED", Pattern: &pattern,
		Shadow: "NORMAL", ShadowOffsetY: &dy, ShadowColor: &white, ShadowOpacity: &half,
	}}
	points := []Point2D{{2.5, 3.5}, {14.5, 3.5}}
	budget := int64(1 << 14)
	if err := paintCompoundPaths(dst, [][]Point2D{points, points}, false, state, color.NRGBA{}, 1, &budget); err != nil {
		t.Fatal(err)
	}
	for x := 3; x < 14; x++ {
		want := uint8(128)
		if (x-2)%2 == 1 {
			want = 0
		}
		if got := dst.GrayAt(x, 3).Y; got != want {
			t.Fatalf("dashed line at %d = %d, want %d", x, got, want)
		}
		if got := dst.GrayAt(x, 6).Y; got != 128 {
			t.Fatalf("shadow at %d = %d, want solid single-alpha shadow", x, got)
		}
	}
}

func TestCompoundPaintOutlinedShadowCompositesBehindVerticalStroke(t *testing.T) {
	dst := image.NewNRGBA(image.Rect(0, 0, 18, 18))
	opaqueBlue := color.NRGBA{B: 255, A: 255}
	dst.SetNRGBA(5, 8, opaqueBlue)
	red := dicomcolor.RGBToCIELab([3]int{255, 0, 0})
	white := dicomcolor.RGBToCIELab([3]int{255, 255, 255})
	extent, half := 2.0, .5
	graphic := &CompoundGraphic{LineStyle: &CompoundLineStyle{
		OnColor: &red, Shadow: "OUTLINED",
		ShadowOffsetX: &extent, ShadowColor: &white, ShadowOpacity: &half,
	}}
	budget := int64(1 << 14)
	path := []Point2D{{7.5, 2.5}, {7.5, 15.5}}
	if err := paintCompoundPaths(dst, [][]Point2D{path}, false, graphic, color.NRGBA{}, 1, &budget); err != nil {
		t.Fatal(err)
	}
	if got := dst.NRGBAAt(7, 8); !compoundPaintNearColor(got, color.NRGBA{R: 255, A: 255}) {
		t.Fatalf("stroke = %+v", got)
	}
	if got := dst.NRGBAAt(5, 8); got != (color.NRGBA{R: 128, G: 128, B: 255, A: 255}) {
		t.Fatalf("shadow over opaque blue = %+v", got)
	}
	if right, outside := dst.NRGBAAt(9, 8), dst.NRGBAAt(10, 8); right.A != 128 || outside.A != 0 {
		t.Fatalf("outlined radius: inside=%+v, outside=%+v", right, outside)
	}
}

func TestCompoundPaintBoundsWorkBeforePublishingStroke(t *testing.T) {
	dst := image.NewNRGBA(image.Rect(0, 0, 20, 20))
	wide := 100.0
	graphic := &CompoundGraphic{LineStyle: &CompoundLineStyle{Thickness: &wide}}
	budget := int64(16)
	path := []Point2D{{-1e8, 10}, {1e8, 10}}
	err := paintCompoundPaths(dst, [][]Point2D{path}, false, graphic, color.NRGBA{A: 255}, 1, &budget)
	if !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("bounded paint error = %v", err)
	}
	for _, b := range dst.Pix {
		if b != 0 {
			t.Fatal("a rejected stroke modified the destination")
		}
	}
	path[0].X = math.Inf(-1)
	budget = 1 << 16
	if err := paintCompoundPaths(dst, [][]Point2D{path}, false, graphic, color.NRGBA{A: 255}, 1, &budget); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("invalid projection error = %v", err)
	}
}

func compoundPaintNearColor(got, want color.NRGBA) bool {
	ok := func(a, b uint8) bool { return math.Abs(float64(a)-float64(b)) <= 1 }
	return got.A == want.A && ok(got.R, want.R) && ok(got.G, want.G) && ok(got.B, want.B)
}
