package roi

import (
	"errors"
	"math"
	"sort"

	"github.com/ThalesMMS/dicom-go/render"
)

const (
	// MaxCurveControlPoints bounds one editable point path.
	MaxCurveControlPoints = 256
	// MaxCurveContourPoints bounds the piecewise-linear approximation.
	MaxCurveContourPoints = 4096
	// CurveTolerancePx is the geometric chord tolerance in image pixels.
	CurveTolerancePx           = 0.01
	maxCurveCoordinate         = 1 << 20
	minCurveControlSpacingPx   = 1e-6
	curveLengthErrorPerSpanPx  = 0.005
	maxCurveDepth              = 20
	maxCurveIntersectionChecks = 1 << 20
)

var (
	ErrCurveGeometry = errors.New("curve requires distinct finite controls and a simple, nonzero closed interior")
	ErrCurveLimit    = errors.New("curve exceeds the control-point, contour-point or approximation limit")
)

// ApproximateControlCurve samples a centripetal Catmull-Rom spline through
// image-pixel control points. Its chords follow the curve within
// CurveTolerancePx. The image-space contour is independent of viewport and
// measurement calibration. Both endpoints of an open path are included.
// A closed contour includes the first point once, with an implicit closing
// edge. Open crossings have a well-defined path length. A closed sampled
// contour with crossings, retraced edges, or zero area is refused.
func ApproximateControlCurve(points []Point2D, closed bool) ([]Point2D, error) {
	if len(points) > MaxCurveControlPoints {
		return nil, ErrCurveLimit
	}
	if closed && len(points) > 1 && points[0] == points[len(points)-1] {
		points = points[:len(points)-1]
	}
	if len(points) < 2 || (closed && len(points) < 3) {
		return nil, ErrCurveGeometry
	}
	controls := make([]render.Vec3, len(points))
	for i, point := range points {
		if !finitePoint(point) || math.Abs(point.X) > maxCurveCoordinate || math.Abs(point.Y) > maxCurveCoordinate {
			return nil, ErrCurveGeometry
		}
		controls[i] = render.Vec3{X: point.X, Y: point.Y}
	}
	spans := len(controls) - 1
	if closed {
		spans++
	}
	chords := make([]render.Vec3, spans)
	for i := range chords {
		chords[i] = controls[(i+1)%len(controls)].Sub(controls[i])
		if chords[i].Length() < minCurveControlSpacingPx {
			return nil, ErrCurveGeometry
		}
	}
	contour := make([]Point2D, 1, min(MaxCurveContourPoints, spans*4+1))
	contour[0] = points[0]
	for i, chord := range chords {
		prev, next := chord, chord
		if i > 0 || closed {
			prev = chords[(i+spans-1)%spans]
		}
		if i+1 < spans || closed {
			next = chords[(i+1)%spans]
		}
		start, end := controls[i], controls[(i+1)%len(controls)]
		startTangent, endTangent := render.CentripetalCatmullRomTangents(prev, chord, next)
		curve := curveBezier{start, start.Add(startTangent.Scale(1.0 / 3)), end.Sub(endTangent.Scale(1.0 / 3)), end}
		if !curve.flatten(&contour, curveLengthErrorPerSpanPx, 0) {
			return nil, ErrCurveLimit
		}
	}
	if closed {
		contour = contour[:len(contour)-1]
		if err := validateClosedCurve(contour); err != nil {
			return nil, err
		}
	}
	return contour, nil
}

type curveBezier struct {
	start, c1, c2, end render.Vec3
}

// Chord error is bounded by convexity of the distance to a line segment.
// Subdivision also bounds the summed arc-length deficit by allocating each
// child half of its parent's polygon-length excess budget.
func (b curveBezier) flatten(out *[]Point2D, lengthError float64, depth int) bool {
	chord := b.end.Sub(b.start)
	controlLength := b.c1.Sub(b.start).Length() + b.c2.Sub(b.c1).Length() + b.end.Sub(b.c2).Length()
	if controlLength-chord.Length() <= lengthError &&
		chordDistance(b.c1.Sub(b.start), chord) <= CurveTolerancePx &&
		chordDistance(b.c2.Sub(b.start), chord) <= CurveTolerancePx {
		if len(*out) >= MaxCurveContourPoints {
			return false
		}
		*out = append(*out, Point2D{X: b.end.X, Y: b.end.Y})
		return true
	}
	if depth >= maxCurveDepth {
		return false
	}
	a := b.start.Add(b.c1).Scale(0.5)
	m := b.c1.Add(b.c2).Scale(0.5)
	c := b.c2.Add(b.end).Scale(0.5)
	d := a.Add(m).Scale(0.5)
	e := m.Add(c).Scale(0.5)
	split := d.Add(e).Scale(0.5)
	return (curveBezier{b.start, a, d, split}).flatten(out, lengthError/2, depth+1) &&
		(curveBezier{split, e, c, b.end}).flatten(out, lengthError/2, depth+1)
}

func chordDistance(point, chord render.Vec3) float64 {
	if chord == (render.Vec3{}) {
		return point.Length()
	}
	fraction := math.Max(0, math.Min(1, point.Dot(chord)/chord.Dot(chord)))
	return point.Sub(chord.Scale(fraction)).Length()
}

type curveEdge struct {
	index                  int
	minX, maxX, minY, maxY float64
}

func validateClosedCurve(points []Point2D) error {
	edges := make([]curveEdge, len(points))
	origin := points[0]
	area := 0.0
	for i, a := range points {
		b := points[(i+1)%len(points)]
		c := points[(i+2)%len(points)]
		if a == b || orientation(a, b, c) == 0 && ((a.X-b.X)*(c.X-b.X)+(a.Y-b.Y)*(c.Y-b.Y)) > 0 {
			return ErrCurveGeometry
		}
		area += (a.X-origin.X)*(b.Y-origin.Y) - (b.X-origin.X)*(a.Y-origin.Y)
		edges[i] = curveEdge{i, math.Min(a.X, b.X), math.Max(a.X, b.X), math.Min(a.Y, b.Y), math.Max(a.Y, b.Y)}
	}
	if math.Abs(area) <= 1e-9 {
		return ErrCurveGeometry
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].minX < edges[j].minX })
	checks := 0
	for i, first := range edges {
		a, b := points[first.index], points[(first.index+1)%len(points)]
		for j := i + 1; j < len(edges) && edges[j].minX <= first.maxX+1e-9; j++ {
			checks++
			if checks > maxCurveIntersectionChecks {
				return ErrCurveLimit
			}
			second := edges[j]
			if second.minY > first.maxY+1e-9 || first.minY > second.maxY+1e-9 ||
				(first.index+1)%len(points) == second.index || (second.index+1)%len(points) == first.index {
				continue
			}
			c, d := points[second.index], points[(second.index+1)%len(points)]
			if segmentsIntersect(a, b, c, d) {
				return ErrCurveGeometry
			}
		}
	}
	return nil
}
