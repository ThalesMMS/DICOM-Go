package render

import (
	"errors"
	"math"
	"testing"
)

func TestViewportReslicePixelCentresAndZoomCrop(t *testing.T) {
	source := Plane{Origin: Vec3{X: 10, Y: 20, Z: 30}, U: Vec3{X: 399}, V: Vec3{Y: 199}}
	req := ViewportResliceRequest{SourceWidth: 400, SourceHeight: 200, OutputWidth: 400, OutputHeight: 200, ViewportWidth: 400, ViewportHeight: 200}
	plan, err := PlanViewportReslice(source, req)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Plane != source {
		t.Fatalf("identity %+v != %+v", plan.Plane, source)
	}
	req.Zoom = 4
	zoom, err := PlanViewportReslice(source, req)
	if err != nil {
		t.Fatal(err)
	}
	nearVec(t, zoom.Plane.U, source.U.Scale(.25))
	nearVec(t, zoom.Plane.V, source.V.Scale(.25))
	nearVec(t, zoom.Plane.At(.5, .5), source.At(.5, .5))
	// Two backing pixels cover each reference pixel; the first centre is
	// a quarter reference pixel before centre zero, not a shifted ROI origin.
	req.Zoom = 1
	req.OutputWidth *= 2
	req.OutputHeight *= 2
	high, err := PlanViewportReslice(source, req)
	if err != nil {
		t.Fatal(err)
	}
	x, y, ok := high.OutputToSource(0, 0)
	if !ok || math.Abs(x+.25) > 1e-12 || math.Abs(y+.25) > 1e-12 {
		t.Fatalf("HiDPI first centre %g,%g", x, y)
	}
	nearVec(t, high.Plane.At(.5, .5), source.At(.5, .5))
}

func TestViewportResliceReferenceGeometryRoundTripsAtAnyDensity(t *testing.T) {
	source := Plane{Origin: Vec3{X: 7, Y: -9, Z: 3}, U: Vec3{X: 30, Y: 20, Z: 5}, V: Vec3{X: -10, Y: 15, Z: 4}}
	for _, fit := range []ViewportFit{ViewportFitContain, ViewportFitCover} {
		for _, dims := range [][2]int{{620, 310}, {1240, 620}, {512, 256}} {
			req := ViewportResliceRequest{SourceWidth: 301, SourceHeight: 203, OutputWidth: dims[0], OutputHeight: dims[1], ViewportWidth: 620, ViewportHeight: 310, Zoom: 4, PanX: 17.25, PanY: -8.75, Rotation: math.Pi / 2, Fit: fit}
			plan, err := PlanViewportReslice(source, req)
			if err != nil {
				t.Fatal(err)
			}
			for _, q := range [][2]float64{{0, 0}, {150, 101}, {12.25, 18.5}, {300, 202}} {
				x, y, ok := plan.SourceToOutput(q[0], q[1])
				if !ok {
					t.Fatal("forward")
				}
				sx, sy, ok := plan.OutputToSource(x, y)
				if !ok || math.Abs(sx-q[0]) > 1e-10 || math.Abs(sy-q[1]) > 1e-10 {
					t.Fatalf("round trip %v -> %g,%g", q, sx, sy)
				}
			}
			x, y, _ := plan.OutputToSource(29.5, 47.25)
			nearVec(t, plan.Plane.At(29.5/float64(plan.Width-1), 47.25/float64(plan.Height-1)), source.At(x/300, y/202))
		}
	}
}
func TestViewportResliceRejectsUnsafeOrDegenerateGeometry(t *testing.T) {
	source := Plane{U: Vec3{X: 3}, V: Vec3{Y: 3}}
	valid := ViewportResliceRequest{SourceWidth: 4, SourceHeight: 4, OutputWidth: 10, OutputHeight: 8, ViewportWidth: 10, ViewportHeight: 8}
	cases := []ViewportResliceRequest{valid, valid, valid, valid, valid, valid, valid, valid}
	cases[0].Zoom = -1
	cases[1].Zoom = math.NaN()
	cases[2].PanX = math.Inf(1)
	cases[3].ViewportWidth = 0
	cases[4].OutputWidth = 8193
	cases[5].OutputHeight = int(^uint(0) >> 1)
	cases[6].Fit = 99
	cases[7].SourceWidth = 1
	for _, request := range cases {
		if _, err := PlanViewportReslice(source, request); !errors.Is(err, ErrInvalidViewState) {
			t.Fatalf("unsafe %+v: %v", request, err)
		}
	}
	if _, err := PlanViewportReslice(Plane{}, valid); !errors.Is(err, ErrInvalidViewState) {
		t.Fatalf("degenerate: %v", err)
	}
	if _, _, ok := (ViewportReslicePlan{}).OutputToSource(0, 0); ok {
		t.Fatal("zero plan")
	}
}
func nearVec(t *testing.T, got, want Vec3) {
	t.Helper()
	if got.Sub(want).Length() > 1e-9 {
		t.Fatalf("got %+v want %+v", got, want)
	}
}
