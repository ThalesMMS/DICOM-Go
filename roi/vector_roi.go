package roi

import (
	"image"
	"math"
)

// ROIShape identifies a vector ROI's geometry.
type ROIShape int

const (
	// ROIRectangle is defined by two opposite corner points.
	ROIRectangle ROIShape = iota
	// ROIEllipse is the ellipse inscribed in the bounding box of two corners.
	ROIEllipse
	// ROIPolygon is the closed polygon through its vertices.
	ROIPolygon
)

// VectorROI is a geometric region of interest in image-pixel coordinates. It is
// the vector layer of the ROI model; Rasterize converts it to a RasterMask (the
// raster layer) which can then be placed in a Segmentation3D (the volumetric
// layer). A pixel belongs to the ROI when its centre does.
type VectorROI struct {
	Shape  ROIShape
	Points []image.Point
	// SubpixelPoints, when set, replaces Points with sub-pixel control
	// points (#1073): an integer coordinate is a pixel centre.
	SubpixelPoints []Point2D
}

// ControlPoints returns the control points as Point2D: SubpixelPoints when
// set, else Points.
func (r VectorROI) ControlPoints() []Point2D {
	return append([]Point2D(nil), r.points2D()...)
}

// points2D returns the control points as Point2D.
func (r VectorROI) points2D() []Point2D {
	if len(r.SubpixelPoints) > 0 {
		return r.SubpixelPoints
	}
	return imagePointsToPoint2D(r.Points)
}

// Contains reports whether pixel (x,y) lies inside the ROI.
func (r VectorROI) Contains(x, y int) bool {
	switch r.Shape {
	case ROIEllipse:
		return r.ellipseContains(x, y)
	case ROIPolygon:
		return pointInPolygonEvenOdd(float64(x), float64(y), r.points2D())
	default:
		return r.rectContains(x, y)
	}
}

func (r VectorROI) bbox() (minX, minY, maxX, maxY float64, ok bool) {
	points := r.points2D()
	if len(points) < 2 {
		return 0, 0, 0, 0, false
	}
	minX, maxX = points[0].X, points[0].X
	minY, maxY = points[0].Y, points[0].Y
	for _, p := range points {
		if !finitePoint(p) {
			return 0, 0, 0, 0, false
		}
		minX, maxX = math.Min(minX, p.X), math.Max(maxX, p.X)
		minY, maxY = math.Min(minY, p.Y), math.Max(maxY, p.Y)
	}
	return minX, minY, maxX, maxY, true
}

func (r VectorROI) rectContains(x, y int) bool {
	minX, minY, maxX, maxY, ok := r.bbox()
	if !ok {
		return false
	}
	px, py := float64(x), float64(y)
	return px >= minX && px <= maxX && py >= minY && py <= maxY
}

func (r VectorROI) ellipseContains(x, y int) bool {
	minX, minY, maxX, maxY, ok := r.bbox()
	if !ok {
		return false
	}
	cx := (minX + maxX) / 2
	cy := (minY + maxY) / 2
	rx := (maxX - minX) / 2
	ry := (maxY - minY) / 2
	if rx <= 0 || ry <= 0 {
		return false
	}
	dx := (float64(x) - cx) / rx
	dy := (float64(y) - cy) / ry
	return dx*dx+dy*dy <= 1
}

func roiPointInPolygon(x, y int, pts []image.Point) bool {
	return pointInPolygonEvenOdd(float64(x), float64(y), imagePointsToPoint2D(pts))
}

// pointInPolygonEvenOdd is the crossing test of RasterizePolygon's rule.
func pointInPolygonEvenOdd(px, py float64, pts []Point2D) bool {
	if len(pts) < 3 {
		return false
	}
	in := false
	n := len(pts)
	j := n - 1
	for i := 0; i < n; i++ {
		xi, yi := pts[i].X, pts[i].Y
		xj, yj := pts[j].X, pts[j].Y
		if (yi > py) != (yj > py) {
			cross := (xj-xi)*(py-yi)/(yj-yi) + xi
			if px < cross {
				in = !in
			}
		}
		j = i
	}
	return in
}

// Rasterize converts the vector ROI into a RasterMask of the given size by
// scanning each row for contiguous inside spans.
func (r VectorROI) Rasterize(columns, rows int) *RasterMask {
	mask := NewRasterMask(columns, rows)
	if r.Shape == ROIPolygon {
		r.rasterizePolygonScanlines(mask)
		return mask
	}
	for y := 0; y < rows; y++ {
		x := 0
		for x < columns {
			if !r.Contains(x, y) {
				x++
				continue
			}
			start := x
			for x < columns && r.Contains(x, y) {
				x++
			}
			mask.SetRun(y, start, x)
		}
	}
	return mask
}

// Statistics rasterizes the vector ROI and computes its rescaled pixel-value
// distribution. The callback is invoked only for pixels inside the image-sized
// mask; bins follows Stats2DWithHistogram semantics.
func (r VectorROI) Statistics(columns, rows int, valueAt func(x, y int) (float64, bool), bins int) Stats {
	return Stats2DWithHistogram(r.Rasterize(columns, rows), valueAt, bins)
}

// StatisticsWithHistogramBounds is Statistics with an explicit display range
// for histogram bins. Values outside the range are retained in the edge bins.
func (r VectorROI) StatisticsWithHistogramBounds(columns, rows int, valueAt func(x, y int) (float64, bool), bins int, histogramMin, histogramMax float64) Stats {
	return Stats2DWithHistogramBounds(r.Rasterize(columns, rows), valueAt, bins, histogramMin, histogramMax)
}

// rasterizePolygonScanlines fills the integer-vertex polygon with the shared
// sub-pixel scanline filler (see RasterizePolygon). Integer vertices convert to
// float64 exactly, so the result equals roiPointInPolygon at every pixel.
func (r VectorROI) rasterizePolygonScanlines(mask *RasterMask) {
	points := r.points2D()
	if mask == nil || len(points) < 3 {
		return
	}
	if len(r.SubpixelPoints) > 0 {
		snapped := make([]Point2D, len(points))
		for i, point := range points {
			snapped[i] = Point2D{X: snapToPixelCentre(point.X), Y: snapToPixelCentre(point.Y)}
		}
		points = snapped
	}
	rasterizePolygonScanlines(mask, points)
}
