package roi

import (
	"image"
	"math"
)

// MeasureSpacing is the effective in-plane mm-per-pixel of a (possibly resliced)
// pane: X = horizontal (column) spacing, Y = vertical (row) spacing. It lets the
// mm helpers work on reslice panes whose effective spacing is not the series'
// native PixelSpacing (mpr-toolbar-plan.md §5.6).
//
// Note on oblique planes: when U⊥V (the panes this engine produces), distances
// computed from per-axis spacing are exact Euclidean mm. For a hypothetical
// sheared plane (U not ⊥ V) this is an approximation; the engine never builds
// such planes.
type MeasureSpacing struct {
	X        float64
	Y        float64
	ColumnMM float64
	RowMM    float64
}

func (m MeasureSpacing) valid() bool {
	x, y := m.spacingXY()
	return x > 0 && y > 0 && !math.IsNaN(x) && !math.IsNaN(y) && !math.IsInf(x, 0) && !math.IsInf(y, 0)
}

func (m MeasureSpacing) spacingXY() (float64, float64) {
	x, y := m.X, m.Y
	if x <= 0 {
		x = m.ColumnMM
	}
	if y <= 0 {
		y = m.RowMM
	}
	return x, y
}

// LengthMMWith returns the distance between two pane pixels in mm using the
// pane's effective spacing.
func LengthMMWith(a, b image.Point, sp MeasureSpacing) (float64, bool) {
	return SubpixelLengthMM(point2D(a), point2D(b), sp)
}

// RectangleAreaMM2With returns the area of the rectangle spanned by a,b in mm².
func RectangleAreaMM2With(a, b image.Point, sp MeasureSpacing) (float64, bool) {
	return SubpixelRectangleAreaMM2(point2D(a), point2D(b), sp)
}

// EllipseAreaMM2With returns the area of the ellipse inscribed in bbox in mm².
func EllipseAreaMM2With(bbox image.Rectangle, sp MeasureSpacing) (float64, bool) {
	return SubpixelEllipseAreaMM2(point2D(bbox.Min), point2D(bbox.Max), sp)
}

// PolygonLengthMMWith returns the polyline length through pts in mm.
func PolygonLengthMMWith(pts []image.Point, sp MeasureSpacing) (float64, bool) {
	return SubpixelPolygonLengthMM(imagePointsToPoint2D(pts), sp)
}

// PolygonAreaMM2With returns the area of the closed polygon (shoelace) in mm².
func PolygonAreaMM2With(pts []image.Point, sp MeasureSpacing) (float64, bool) {
	return SubpixelPolygonAreaMM2(imagePointsToPoint2D(pts), sp)
}
