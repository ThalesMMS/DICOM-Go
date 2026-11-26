package roi

import (
	"image"
	"math"
	"sort"
)

// RasterizePolygon fills a closed polygon whose vertices are sub-pixel image
// coordinates and returns it as a RasterMask of the given size.
//
// Coordinates follow the DICOM pixel convention: pixel (x, y) is the unit
// square centred on the point (x, y), so an integer coordinate is a pixel
// centre and pixel edges lie on half-integers. Vertices are never rounded: a
// pixel is inside exactly when its centre is inside the polygon under the
// even-odd (parity) rule, which also gives self-intersecting outlines and
// keyhole contours a well-defined interior.
//
// Centres that lie exactly on an edge follow a half-open "top-left" rule: an
// edge covers the centre rows y with min(y0, y1) <= y < max(y0, y1), and on a
// row a span between sorted crossings [left, right) contains the centres x
// with left <= x < right. Left and top boundaries are therefore inside and
// right and bottom boundaries outside, so polygons that share an edge tile
// the plane without gaps or overlaps. Because the parity of every centre is a
// sum over edges, XOR-combining the masks of several contours equals the
// even-odd fill of all of them together (the RTSTRUCT CLOSEDPLANAR_XOR rule).
//
// A vertex coordinate within 1e-6 px of an integer is snapped onto that pixel
// centre first. This is far below any meaningful offset but above the
// floating-point error of an image -> patient -> image round trip (including
// 16-character DS encoding), so outlines authored on pixel centres resolve
// their edge ties exactly like the integer outline.
//
// Vertices outside the grid are clipped. A polygon with fewer than three
// vertices or with a non-finite coordinate yields an empty mask.
func RasterizePolygon(points []Point2D, columns, rows int) *RasterMask {
	mask := NewRasterMask(columns, rows)
	if len(points) < 3 {
		return mask
	}
	snapped := make([]Point2D, len(points))
	for i, point := range points {
		snapped[i] = Point2D{X: snapToPixelCentre(point.X), Y: snapToPixelCentre(point.Y)}
	}
	rasterizePolygonScanlines(mask, snapped)
	return mask
}

// pixelCentreSnapTolerance bounds the round-off absorbed by RasterizePolygon.
const pixelCentreSnapTolerance = 1e-6

func snapToPixelCentre(value float64) float64 {
	if nearest := math.Round(value); math.Abs(value-nearest) <= pixelCentreSnapTolerance {
		return nearest
	}
	return value
}

func imagePointsToPoint2D(points []image.Point) []Point2D {
	out := make([]Point2D, len(points))
	for i, point := range points {
		out[i] = point2D(point)
	}
	return out
}

// rasterizePolygonScanlines fills an even-odd polygon directly as horizontal
// runs. Its half-open rule matches roiPointInPolygon exactly for integer pixel
// centres: an edge is active on rows y with min(yi,yj) <= y < max(yi,yj), i.e.
// [ceil(min), ceil(max)), and a sorted intersection pair [left,right]
// contributes integer x where x >= left and x < right, i.e. [ceil(left),
// ceil(right)).
func rasterizePolygonScanlines(mask *RasterMask, points []Point2D) {
	if mask == nil || len(points) < 3 || mask.Columns <= 0 || mask.Rows <= 0 {
		return
	}
	minVertexY, maxVertexY := math.Inf(1), math.Inf(-1)
	for _, point := range points {
		if !finite(point.X) || !finite(point.Y) {
			return
		}
		minVertexY = math.Min(minVertexY, point.Y)
		maxVertexY = math.Max(maxVertexY, point.Y)
	}
	// Rows whose centre lies in [minVertexY, maxVertexY) can be crossed.
	minY := polygonScanlineBoundary(minVertexY, mask.Rows)
	maxY := polygonScanlineBoundary(maxVertexY, mask.Rows) - 1
	if minY > maxY {
		return
	}
	// Schedule each edge on its first intersecting scanline so the fill costs
	// O(H + P + crossings) rather than H*P.
	type scanlineEdge struct {
		pointIndex int
		endY       int
		nextStart  int
	}
	edges := make([]scanlineEdge, 0, len(points))
	startHeads := make([]int, maxY-minY+1)
	for index := range startHeads {
		startHeads[index] = -1
	}
	for current := len(points) - 1; current >= 0; current-- {
		previous := current - 1
		if previous < 0 {
			previous = len(points) - 1
		}
		currentPoint, previousPoint := points[current], points[previous]
		startY := polygonScanlineBoundary(math.Min(currentPoint.Y, previousPoint.Y), mask.Rows)
		endY := polygonScanlineBoundary(math.Max(currentPoint.Y, previousPoint.Y), mask.Rows)
		// Horizontal edges and edges between two centre rows cross nothing.
		startY = max(startY, minY)
		endY = min(endY, maxY+1)
		if startY >= endY {
			continue
		}
		bucket := startY - minY
		edges = append(edges, scanlineEdge{
			pointIndex: current,
			endY:       endY,
			nextStart:  startHeads[bucket],
		})
		startHeads[bucket] = len(edges) - 1
	}
	active := make([]int, 0, len(edges))
	intersections := make([]float64, 0, len(points))
	for y := minY; y <= maxY; y++ {
		kept := active[:0]
		for _, edgeIndex := range active {
			if edges[edgeIndex].endY > y {
				kept = append(kept, edgeIndex)
			}
		}
		active = kept
		for edgeIndex := startHeads[y-minY]; edgeIndex >= 0; edgeIndex = edges[edgeIndex].nextStart {
			active = append(active, edgeIndex)
		}
		intersections = intersections[:0]
		py := float64(y)
		for _, edgeIndex := range active {
			edge := edges[edgeIndex]
			currentPoint := points[edge.pointIndex]
			previousPoint := points[(edge.pointIndex+len(points)-1)%len(points)]
			xi, yi := currentPoint.X, currentPoint.Y
			xj, yj := previousPoint.X, previousPoint.Y
			intersections = append(intersections, (xj-xi)*(py-yi)/(yj-yi)+xi)
		}
		sort.Float64s(intersections)
		runs := make([]MaskRun, 0, len(intersections)/2)
		for i := 0; i+1 < len(intersections); i += 2 {
			start := polygonScanlineBoundary(intersections[i], mask.Columns)
			end := polygonScanlineBoundary(intersections[i+1], mask.Columns)
			if start >= end {
				continue
			}
			if len(runs) > 0 && runs[len(runs)-1].End >= start {
				if end > runs[len(runs)-1].End {
					runs[len(runs)-1].End = end
				}
				continue
			}
			runs = append(runs, MaskRun{Start: start, End: end})
		}
		if len(runs) > 0 {
			mask.runs[y] = runs
		}
	}
}

// polygonScanlineBoundary returns ceil(coordinate) clamped to [0, limit]: the
// first pixel index whose centre is at or after the coordinate.
func polygonScanlineBoundary(coordinate float64, limit int) int {
	if coordinate <= 0 {
		return 0
	}
	if coordinate >= float64(limit) {
		return limit
	}
	return int(math.Ceil(coordinate))
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
