package roi

import (
	"context"
	"errors"
	"math"
	"sort"
)

var ErrPolygonFeret = errors.New("roi: invalid polygon for Feret measurement")

// PolygonFeretDiametersContext measures the minimum supporting-line width
// and maximum point-to-point diameter of a polygon's convex hull. Coordinates
// must already use the desired physical unit. Input ownership is retained by
// the caller; hull construction and rotating calipers are bounded O(n log n).
func PolygonFeretDiametersContext(ctx context.Context, points []Point2D) (minimum, maximum float64, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	if len(points) < 3 || len(points) > 1<<20 {
		return 0, 0, ErrPolygonFeret
	}
	ordered := append([]Point2D(nil), points...)
	for _, p := range ordered {
		if math.IsNaN(p.X) || math.IsNaN(p.Y) || math.IsInf(p.X, 0) || math.IsInf(p.Y, 0) {
			return 0, 0, ErrPolygonFeret
		}
	}
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].X < ordered[j].X || (ordered[i].X == ordered[j].X && ordered[i].Y < ordered[j].Y)
	})
	cross := func(a, b, c Point2D) float64 { return (b.X-a.X)*(c.Y-a.Y) - (b.Y-a.Y)*(c.X-a.X) }
	hull := make([]Point2D, 0, 2*len(ordered))
	for _, p := range ordered {
		for len(hull) >= 2 && cross(hull[len(hull)-2], hull[len(hull)-1], p) <= 0 {
			hull = hull[:len(hull)-1]
		}
		hull = append(hull, p)
	}
	upperStart := len(hull) + 1
	for i := len(ordered) - 2; i >= 0; i-- {
		p := ordered[i]
		for len(hull) >= upperStart && cross(hull[len(hull)-2], hull[len(hull)-1], p) <= 0 {
			hull = hull[:len(hull)-1]
		}
		hull = append(hull, p)
	}
	hull = hull[:len(hull)-1]
	if len(hull) < 3 {
		return 0, 0, ErrPolygonFeret
	}
	minimum = math.Inf(1)
	j := 1
	for i, a := range hull {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, 0, err
			}
		}
		b := hull[(i+1)%len(hull)]
		area := func(k int) float64 { return math.Abs(cross(a, b, hull[k])) }
		for area((j+1)%len(hull)) > area(j) {
			j = (j + 1) % len(hull)
		}
		width := area(j) / math.Hypot(b.X-a.X, b.Y-a.Y)
		minimum = math.Min(minimum, width)
		for _, p := range []Point2D{a, b} {
			maximum = math.Max(maximum, math.Hypot(p.X-hull[j].X, p.Y-hull[j].Y))
			if area((j+1)%len(hull)) == area(j) {
				q := hull[(j+1)%len(hull)]
				maximum = math.Max(maximum, math.Hypot(p.X-q.X, p.Y-q.Y))
			}
		}
	}
	if !(minimum > 0) || math.IsInf(minimum, 0) || math.IsNaN(minimum) || math.IsInf(maximum, 0) || math.IsNaN(maximum) {
		return 0, 0, ErrPolygonFeret
	}
	return minimum, maximum, nil
}
