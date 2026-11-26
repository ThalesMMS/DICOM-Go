package roi

// Sub-pixel measurement (#1073). Measurement control points are image
// coordinates in pixels where an integer is a pixel centre (the convention of
// RasterizePolygon); rounding them to whole pixels adds up to about 0.7 px
// of error per length. These functions keep the fractional position; the
// image.Point functions of measure.go call them with whole-pixel points.

import (
	"image"
	"math"
)

// Point2DsFromImage converts whole-pixel points to Point2D.
func Point2DsFromImage(points []image.Point) []Point2D {
	return imagePointsToPoint2D(points)
}

// SubpixelLengthMM returns the distance between two points in mm.
func SubpixelLengthMM(a, b Point2D, sp MeasureSpacing) (float64, bool) {
	if !sp.valid() || !finitePoint(a) || !finitePoint(b) {
		return 0, false
	}
	x, y := sp.spacingXY()
	return math.Hypot((a.X-b.X)*x, (a.Y-b.Y)*y), true
}

// SubpixelAngleDeg returns the angle at vertex between the rays to a and b in
// degrees. A valid spacing is applied first, so anisotropic pixels do not skew
// the angle; otherwise the angle is measured in pixel units.
func SubpixelAngleDeg(a, vertex, b Point2D, sp MeasureSpacing) float64 {
	x, y := 1.0, 1.0
	if sp.valid() {
		x, y = sp.spacingXY()
	}
	v1x, v1y := (a.X-vertex.X)*x, (a.Y-vertex.Y)*y
	v2x, v2y := (b.X-vertex.X)*x, (b.Y-vertex.Y)*y
	m1, m2 := math.Hypot(v1x, v1y), math.Hypot(v2x, v2y)
	if m1 == 0 || m2 == 0 || math.IsNaN(m1) || math.IsNaN(m2) {
		return 0
	}
	c := (v1x*v2x + v1y*v2y) / (m1 * m2)
	return math.Acos(math.Max(-1, math.Min(1, c))) * 180 / math.Pi
}

// SubpixelRectangleAreaMM2 returns the area of the rectangle spanned by a
// and b in mm².
func SubpixelRectangleAreaMM2(a, b Point2D, sp MeasureSpacing) (float64, bool) {
	if !sp.valid() || !finitePoint(a) || !finitePoint(b) {
		return 0, false
	}
	x, y := sp.spacingXY()
	return math.Abs(a.X-b.X) * x * math.Abs(a.Y-b.Y) * y, true
}

// SubpixelEllipseAreaMM2 returns the area of the ellipse inscribed in the
// rectangle spanned by a and b in mm².
func SubpixelEllipseAreaMM2(a, b Point2D, sp MeasureSpacing) (float64, bool) {
	if !sp.valid() || !finitePoint(a) || !finitePoint(b) {
		return 0, false
	}
	x, y := sp.spacingXY()
	return math.Pi * math.Abs(a.X-b.X) / 2 * x * math.Abs(a.Y-b.Y) / 2 * y, true
}

// SubpixelPolygonLengthMM returns the length of the polyline through pts in
// mm.
func SubpixelPolygonLengthMM(pts []Point2D, sp MeasureSpacing) (float64, bool) {
	if !sp.valid() || len(pts) < 2 {
		return 0, false
	}
	x, y := sp.spacingXY()
	total := 0.0
	for i := 1; i < len(pts); i++ {
		total += math.Hypot((pts[i].X-pts[i-1].X)*x, (pts[i].Y-pts[i-1].Y)*y)
	}
	return total, finiteNumber(total)
}

// SubpixelPolygonAreaMM2 returns the area of the closed polygon through pts
// (shoelace) in mm².
func SubpixelPolygonAreaMM2(pts []Point2D, sp MeasureSpacing) (float64, bool) {
	if !sp.valid() || len(pts) < 3 {
		return 0, false
	}
	x, y := sp.spacingXY()
	area := math.Abs(ClosedPolygonArea(pts)) * x * y
	return area, finiteNumber(area)
}

// SubpixelCobbAngleDeg returns the acute angle between lines l1 and l2 in
// degrees, after applying a valid spacing. A zero-length or nonfinite segment
// makes the angle undefined and returns NaN.
func SubpixelCobbAngleDeg(l1a, l1b, l2a, l2b Point2D, sp MeasureSpacing) float64 {
	x, y := 1.0, 1.0
	if sp.valid() {
		x, y = sp.spacingXY()
	}
	first := Point2D{X: (l1b.X - l1a.X) * x, Y: (l1b.Y - l1a.Y) * y}
	second := Point2D{X: (l2b.X - l2a.X) * x, Y: (l2b.Y - l2a.Y) * y}
	if first == (Point2D{}) || second == (Point2D{}) || !finitePoint(first) || !finitePoint(second) {
		return math.NaN()
	}
	a1 := math.Atan2(first.Y, first.X)
	a2 := math.Atan2(second.Y, second.X)
	d := math.Mod(math.Abs(a1-a2)*180/math.Pi, 180)
	if d > 90 {
		d = 180 - d
	}
	return d
}

// SubpixelDeviationMM returns the distance in mm from point to the line
// through lineA and lineB.
func SubpixelDeviationMM(lineA, lineB, point Point2D, sp MeasureSpacing) (float64, bool) {
	if !sp.valid() {
		return 0, false
	}
	x, y := sp.spacingXY()
	ax, ay := lineA.X*x, lineA.Y*y
	bx, by := lineB.X*x, lineB.Y*y
	px, py := point.X*x, point.Y*y
	dx, dy := bx-ax, by-ay
	l := math.Hypot(dx, dy)
	if l == 0 || !finiteNumber(l) {
		return 0, false
	}
	return math.Abs((px-ax)*dy-(py-ay)*dx) / l, true
}

// ClosedPolygonArea is the signed shoelace area of a closed polygon in image
// coordinates: positive when it turns clockwise on screen (y down).
func ClosedPolygonArea(points []Point2D) float64 {
	if len(points) < 3 {
		return 0
	}
	// Measure the signed triangles relative to the first vertex. Subtracting
	// the common offset before multiplying preserves small areas far from zero.
	origin := points[0]
	ax, ay := points[1].X-origin.X, points[1].Y-origin.Y
	area := 0.0
	for _, point := range points[2:] {
		bx, by := point.X-origin.X, point.Y-origin.Y
		area += ax*by - bx*ay
		ax, ay = bx, by
	}
	return area / 2
}
