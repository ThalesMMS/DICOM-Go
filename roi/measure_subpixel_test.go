package roi

import (
	"image"
	"math"
	"slices"
	"testing"
)

func TestCobbAngleDegRejectsUndefinedSegments(t *testing.T) {
	point := Point2D{}
	horizontal := Point2D{X: 2}
	diagonal := Point2D{X: 2, Y: 2}
	for _, segments := range [][4]Point2D{
		{point, point, point, diagonal},
		{point, diagonal, point, point},
		{point, Point2D{X: math.Inf(1)}, point, diagonal},
		{Point2D{X: -math.MaxFloat64}, Point2D{X: math.MaxFloat64}, point, diagonal},
	} {
		if got := SubpixelCobbAngleDeg(segments[0], segments[1], segments[2], segments[3], MeasureSpacing{}); !math.IsNaN(got) {
			t.Errorf("SubpixelCobbAngleDeg(%+v) = %v, want undefined", segments, got)
		}
	}
	if got := SubpixelCobbAngleDeg(point, horizontal, horizontal, point, MeasureSpacing{}); got != 0 {
		t.Errorf("Cobb angle of nonzero parallel segments = %v, want 0", got)
	}
}

func TestSubpixelMeasuresMatchAnalyticPhantoms(t *testing.T) {
	sp := MeasureSpacing{X: 0.7, Y: 1.1}
	a, b := Point2D{X: 10.49, Y: 5.49}, Point2D{X: 40.51, Y: 22.51}
	want := math.Hypot(30.02*0.7, 17.02*1.1)
	if got, ok := SubpixelLengthMM(a, b, sp); !ok || math.Abs(got-want) > 1e-12 {
		t.Fatalf("length %v, want %v", got, want)
	}
	// Rounding the end points, as the integer API forces, is off by a
	// sizeable fraction of a pixel on the same segment.
	rounded, _ := LengthMM(image.Pt(10, 5), image.Pt(41, 23), sp)
	if math.Abs(rounded-want) < 0.9 {
		t.Fatalf("the rounded length %v is suspiciously exact", rounded)
	}
	// A right angle in mm is 90° only once the anisotropic spacing applies.
	vertex := Point2D{X: 20.25, Y: 20.5}
	arm1 := Point2D{X: vertex.X + 10/0.7, Y: vertex.Y}
	arm2 := Point2D{X: vertex.X, Y: vertex.Y + 7/1.1}
	if got := SubpixelAngleDeg(arm1, vertex, arm2, sp); math.Abs(got-90) > 1e-9 {
		t.Fatalf("angle %v", got)
	}
	diagonal := Point2D{X: vertex.X + 5/0.7, Y: vertex.Y + 5/1.1}
	if got := SubpixelAngleDeg(arm1, vertex, diagonal, sp); math.Abs(got-45) > 1e-9 {
		t.Fatalf("45° in mm measured %v", got)
	}
	if got := SubpixelCobbAngleDeg(vertex, arm1, vertex, diagonal, sp); math.Abs(got-45) > 1e-9 {
		t.Fatalf("Cobb %v", got)
	}
	triangle := []Point2D{{X: 1.5, Y: 2.25}, {X: 11.5, Y: 2.25}, {X: 1.5, Y: 8.75}}
	if got, ok := SubpixelPolygonAreaMM2(triangle, sp); !ok || math.Abs(got-10*6.5/2*0.7*1.1) > 1e-12 {
		t.Fatalf("area %v", got)
	}
	if got, ok := SubpixelPolygonLengthMM(triangle[:2], sp); !ok || math.Abs(got-7) > 1e-12 {
		t.Fatalf("polyline %v", got)
	}
	if got, ok := SubpixelEllipseAreaMM2(Point2D{X: 1.5, Y: 2}, Point2D{X: 7.5, Y: 10.5}, sp); !ok || math.Abs(got-math.Pi*3*0.7*4.25*1.1) > 1e-12 {
		t.Fatalf("ellipse %v", got)
	}
	if got, ok := SubpixelDeviationMM(Point2D{X: 0, Y: 0.5}, Point2D{X: 10, Y: 0.5}, Point2D{X: 3.3, Y: 4.5}, sp); !ok || math.Abs(got-4*1.1) > 1e-12 {
		t.Fatalf("deviation %v", got)
	}
	// Whole-pixel points give the integer API's results.
	if got, _ := PolygonAreaMM2([]image.Point{{1, 2}, {11, 2}, {1, 9}}, sp); math.Abs(got-10*7/2*0.7*1.1) > 1e-12 {
		t.Fatalf("integer area %v", got)
	}
	feet, ok := SubpixelPerpendicularLinePoints(Point2D{X: 5.5, Y: 7.25}, Point2D{X: 8.25, Y: -3}, Point2D{X: 0, Y: 0.5}, Point2D{X: 10, Y: 0.5})
	if !ok || feet[0] != (Point2D{X: 5.5, Y: 0.5}) || feet[2] != (Point2D{X: 8.25, Y: 0.5}) {
		t.Fatalf("feet %v", feet)
	}
}

func TestPolygonAreaKeepsPrecisionAwayFromOrigin(t *testing.T) {
	const x, y, step = 65534.125, 65534.375, 1.0 / 2048
	const wantArea = step * step
	points := []Point2D{{X: x, Y: y}, {X: x + step, Y: y}, {X: x + step, Y: y + step}, {X: x, Y: y + step}}
	spacing := MeasureSpacing{X: 0.5, Y: 0.25}
	if got := ClosedPolygonArea(points); got != wantArea {
		t.Errorf("signed area = %g, want %g", got, wantArea)
	}
	if got, ok := SubpixelPolygonAreaMM2(points, spacing); !ok || got != wantArea/8 {
		t.Errorf("scaled area = %g, %v, want %g, true", got, ok, wantArea/8)
	}

	// Concave, explicitly closed contours retain their signed area on reversal.
	concave := []Point2D{
		{X: x, Y: y}, {X: x + 3*step, Y: y}, {X: x + 3*step, Y: y + step},
		{X: x + step, Y: y + step}, {X: x + step, Y: y + 3*step}, {X: x, Y: y + 3*step}, {X: x, Y: y},
	}
	if got := ClosedPolygonArea(concave); got != 5*wantArea {
		t.Errorf("concave area = %g, want %g", got, 5*wantArea)
	}
	slices.Reverse(concave)
	if got := ClosedPolygonArea(concave); got != -5*wantArea {
		t.Errorf("reversed area = %g, want %g", got, -5*wantArea)
	}

	const offset = 1000000000
	pixels := []image.Point{{X: offset, Y: offset}, {X: offset + 1, Y: offset}, {X: offset + 1, Y: offset + 1}, {X: offset, Y: offset + 1}}
	if got, ok := PolygonAreaMM2(pixels, spacing); !ok || got != 0.125 {
		t.Errorf("whole-pixel area = %g, %v, want 0.125, true", got, ok)
	}
}

func TestPolygonAreaHandlesDegenerateAndInvalidPoints(t *testing.T) {
	spacing := MeasureSpacing{X: 1, Y: 1}
	for _, points := range [][]Point2D{nil, {{X: 2}}, {{X: 2}, {X: 3}}} {
		if got := ClosedPolygonArea(points); got != 0 {
			t.Errorf("signed area for %v = %g, want zero", points, got)
		}
		if _, ok := SubpixelPolygonAreaMM2(points, spacing); ok {
			t.Errorf("subpixel area accepted too few points: %v", points)
		}
	}
	collinear := []Point2D{{X: 2, Y: 3}, {X: 4, Y: 6}, {X: 6, Y: 9}}
	if got, ok := SubpixelPolygonAreaMM2(collinear, spacing); !ok || got != 0 {
		t.Errorf("collinear area = %g, %v, want zero, true", got, ok)
	}
	for _, value := range []float64{math.NaN(), math.Inf(1)} {
		invalid := []Point2D{{X: value}, {X: 2}, {Y: 1}}
		if _, ok := SubpixelPolygonAreaMM2(invalid, spacing); ok {
			t.Errorf("subpixel area accepted nonfinite points: %v", invalid)
		}
	}
}

func TestVectorROISubpixelPointsRasterizeByPixelCentre(t *testing.T) {
	polygon := []Point2D{{X: 2.4, Y: 1.6}, {X: 12.7, Y: 3.1}, {X: 6.2, Y: 11.9}}
	roi := VectorROI{Shape: ROIPolygon, SubpixelPoints: polygon}
	got, want := roi.Rasterize(16, 14), RasterizePolygon(polygon, 16, 14)
	if !sameMask(got, want) || got.Count() == 0 {
		t.Fatal("sub-pixel polygon differs from RasterizePolygon")
	}
	for y := 0; y < 14; y++ {
		for x := 0; x < 16; x++ {
			if roi.Contains(x, y) != want.Get(x, y) {
				t.Fatalf("Contains(%d,%d)", x, y)
			}
		}
	}
	// A rectangle from 1.6 to 4.4 holds the centres 2, 3 and 4.
	rect := VectorROI{Shape: ROIRectangle, SubpixelPoints: []Point2D{{X: 1.6, Y: 1.6}, {X: 4.4, Y: 2.4}}}
	if mask := rect.Rasterize(8, 8); mask.Count() != 3 || !mask.Get(2, 2) || mask.Get(1, 2) || mask.Get(5, 2) {
		t.Fatalf("rectangle count %d", mask.Count())
	}
	if pts := rect.ControlPoints(); len(pts) != 2 || pts[1].X != 4.4 {
		t.Fatalf("control points %v", pts)
	}
}

func sameMask(a, b *RasterMask) bool {
	for y := 0; y < a.Rows; y++ {
		for x := 0; x < a.Columns; x++ {
			if a.Get(x, y) != b.Get(x, y) {
				return false
			}
		}
	}
	return true
}
