package gsps

import (
	"fmt"
	"image"
	"math"
	"strings"
)

type annotationProjector func(Point2D, string) (Point2D, error)

type projectedCompound struct {
	paths  [][]Point2D
	closed bool
	scale  float64
	labels []projectedTickLabel
}

type projectedTickLabel struct {
	anchor Point2D
	below  bool
	text   string
}

func projectCompound(graphic *CompoundGraphic, place annotationProjector, bounds image.Rectangle) (projectedCompound, error) {
	base, err := place(Point2D{}, graphic.Units)
	if err != nil {
		return projectedCompound{}, err
	}
	right, err := place(Point2D{X: 1}, graphic.Units)
	if err != nil {
		return projectedCompound{}, err
	}
	down, err := place(Point2D{Y: 1}, graphic.Units)
	if err != nil {
		return projectedCompound{}, err
	}
	x, y := deltaPoint(right, base), deltaPoint(down, base)
	width, height := pointLength(x), pointLength(y)
	determinant := x.X*y.Y - x.Y*y.X
	if !safeAnnotationPoint(base) || !safeAnnotationPoint(right) || !safeAnnotationPoint(down) ||
		width == 0 || height == 0 || determinant == 0 {
		return projectedCompound{}, fmt.Errorf("%w: invalid annotation projection", ErrInvalidObject)
	}
	display := normalizedAnnotationUnits(graphic.Units) == AnnotationUnitsDisplay
	scale := math.Min(width, height)
	mapping := func(p Point2D) Point2D {
		if graphic.RotationAngle != nil {
			center := *graphic.RotationPoint
			delta := deltaPoint(p, center)
			// A DISPLAY angle is measured on the shown plane, whose
			// normalized horizontal and vertical extents need not be square.
			xScale, yScale := 1.0, 1.0
			if display {
				xScale, yScale = width, height
			}
			sine, cosine := math.Sincos(*graphic.RotationAngle * math.Pi / 180)
			dx, dy := delta.X*xScale, delta.Y*yScale
			p = sumPoint(center, Point2D{X: (cosine*dx + sine*dy) / xScale, Y: (-sine*dx + cosine*dy) / yScale})
		}
		return sumPoint(base, sumPoint(scalePoint(x, p.X), scalePoint(y, p.Y)))
	}
	projected := projectedCompound{scale: scale}
	points := make([]Point2D, len(graphic.Points))
	for i, p := range graphic.Points {
		points[i] = mapping(p)
	}
	typ := strings.ToUpper(strings.TrimSpace(graphic.Type))
	size := float64(min(bounds.Dx(), bounds.Dy()))
	tickLength := min(12.0, max(3.0, size*0.02))
	handedness := math.Copysign(1, determinant)
	switch typ {
	case "RECTANGLE":
		a, b := graphic.Points[0], graphic.Points[1]
		projected.closed = true
		projected.paths = [][]Point2D{{
			points[0], mapping(Point2D{X: b.X, Y: a.Y}), points[1],
			mapping(Point2D{X: a.X, Y: b.Y}), points[0],
		}}
	case "ELLIPSE":
		a, b := graphic.Points[0], graphic.Points[1]
		// Sample using the destination scale so DISPLAY ellipses do not
		// become coarser than PIXEL ellipses on large screens.
		const maxSteps = 1024
		steps := int(min(maxSteps, max(16, math.Pi*max(math.Abs(b.X-a.X)*width, math.Abs(b.Y-a.Y)*height)/2)))
		outline := make([]Point2D, steps+1)
		center := scalePoint(sumPoint(a, b), 0.5)
		radius := scalePoint(deltaPoint(b, a), 0.5)
		for i := range outline {
			sine, cosine := math.Sincos(float64(i%steps) * (2 * math.Pi) / float64(steps))
			outline[i] = mapping(sumPoint(center, Point2D{X: cosine * radius.X, Y: sine * radius.Y}))
		}
		projected.closed = true
		projected.paths = [][]Point2D{outline}
	case "MULTILINE":
		for i := 0; i+1 < len(points); i += 2 {
			projected.paths = append(projected.paths, points[i:i+2])
		}
	case "ARROW":
		projected.paths = compoundArrow(points[1], points[0], tickLength)
	case "RANGELINE", "RULER", "AXIS":
		projected.paths = [][]Point2D{points}
		ticks := graphic.MajorTicks
		alignment := graphic.TickAlignment
		if typ == "RANGELINE" {
			ticks = []CompoundTick{{Position: 0}, {Position: 1}}
			alignment = "CENTER"
		}
		if typ == "RULER" {
			// RULER tick positions are implementation-defined. Use five
			// equally spaced marks without implying a physical unit.
			ticks = []CompoundTick{{Position: 0}, {Position: .25}, {Position: .5}, {Position: .75}, {Position: 1}}
		}
		axis := deltaPoint(points[1], points[0])
		n := scalePoint(perpendicularPoint(axis), handedness)
		for _, tick := range ticks {
			p := sumPoint(points[0], scalePoint(axis, tick.Position))
			left, right := compoundTick(p, n, tickLength, alignment)
			projected.paths = append(projected.paths, []Point2D{left, right})
			if graphic.ShowTickLabel != nil && *graphic.ShowTickLabel && tick.Label != "" {
				below := strings.EqualFold(graphic.TickLabelAlignment, "BOTTOM")
				distance := tickLength + 2
				if !below {
					distance = -distance
				}
				projected.labels = append(projected.labels, projectedTickLabel{
					anchor: sumPoint(p, scalePoint(n, distance)), below: below, text: tick.Label,
				})
			}
		}
	case "CROSSHAIR":
		gap := *graphic.GapLength * size / 2
		radius := *graphic.DiameterOfVisibility * size / 2
		directions := []Point2D{{X: -1}, {X: 1}, {Y: -1}, {Y: 1}}
		for _, direction := range directions {
			projectedDirection := deltaPoint(mapping(sumPoint(graphic.Points[0], direction)), points[0])
			length := pointLength(projectedDirection)
			if length == 0 {
				return projectedCompound{}, fmt.Errorf("%w: collapsed crosshair projection", ErrInvalidObject)
			}
			unit := scalePoint(projectedDirection, 1/length)
			projected.paths = append(projected.paths, []Point2D{
				sumPoint(points[0], scalePoint(unit, gap)),
				sumPoint(points[0], scalePoint(unit, radius)),
			})
		}
	case "CUTLINE", "INFINITELINE":
		center := mapping(*graphic.RotationPoint)
		gap := *graphic.GapLength * size / 2
		lines := compoundInfiniteLine(points[0], points[1], center, gap, bounds, typ == CompoundTypeCutline)
		projected.paths = lines
		if typ == "CUTLINE" {
			normal := scalePoint(perpendicularPoint(deltaPoint(points[1], points[0])), handedness)
			for _, line := range lines {
				head := scalePoint(sumPoint(line[0], line[1]), 0.5)
				foot := sumPoint(head, scalePoint(normal, tickLength*2))
				projected.paths = append(projected.paths, compoundArrow(foot, head, tickLength*0.65)...)
			}
		}
	}
	for _, path := range projected.paths {
		for _, p := range path {
			if !safeAnnotationPoint(p) {
				return projectedCompound{}, fmt.Errorf("%w: nonfinite or oversized compound projection", ErrInvalidObject)
			}
		}
	}
	return projected, nil
}

func compoundTick(p, normal Point2D, size float64, align string) (Point2D, Point2D) {
	switch strings.ToUpper(strings.TrimSpace(align)) {
	case "TOP":
		return p, sumPoint(p, scalePoint(normal, -size))
	case "BOTTOM":
		return p, sumPoint(p, scalePoint(normal, size))
	default:
		return sumPoint(p, scalePoint(normal, -size/2)), sumPoint(p, scalePoint(normal, size/2))
	}
}

func compoundArrow(foot, head Point2D, length float64) [][]Point2D {
	axis := deltaPoint(head, foot)
	l := pointLength(axis)
	if l <= 0 {
		return nil
	}
	length = min(length, l/2)
	center := sumPoint(head, scalePoint(axis, -length/l))
	wing := scalePoint(perpendicularPoint(axis), length/2)
	return [][]Point2D{{foot, head}, {deltaPoint(center, wing), head, sumPoint(center, wing)}}
}

func compoundInfiniteLine(p, q, center Point2D, radius float64, bounds image.Rectangle, split bool) [][]Point2D {
	axis := deltaPoint(q, p)
	length := pointLength(axis)
	if length == 0 {
		return nil
	}
	axis = scalePoint(axis, 1/length)
	reach := 1 + pointLength(deltaPoint(p, Point2D{X: float64(bounds.Min.X), Y: float64(bounds.Min.Y)})) +
		math.Hypot(float64(bounds.Dx()), float64(bounds.Dy()))
	lo, hi, ok := clipSegment(sumPoint(p, scalePoint(axis, -reach)), sumPoint(p, scalePoint(axis, reach)),
		float64(bounds.Min.X), float64(bounds.Min.Y), float64(bounds.Max.X), float64(bounds.Max.Y))
	if !ok {
		return nil
	}
	offset := deltaPoint(center, lo)
	along := offset.X*axis.X + offset.Y*axis.Y
	height := offset.X*axis.Y - offset.Y*axis.X
	full := pointLength(deltaPoint(hi, lo))
	if math.Abs(height) >= radius {
		if split && along > 0 && along < full {
			center := sumPoint(lo, scalePoint(axis, along))
			return [][]Point2D{{lo, center}, {center, hi}}
		}
		return [][]Point2D{{lo, hi}}
	}
	gap := math.Sqrt(max(0.0, radius*radius-height*height))
	start, end := along-gap, along+gap
	if end <= 0 || start >= full {
		return [][]Point2D{{lo, hi}}
	}
	paths := make([][]Point2D, 0, 2)
	if start > 0 {
		paths = append(paths, []Point2D{lo, sumPoint(lo, scalePoint(axis, start))})
	}
	if end < full {
		paths = append(paths, []Point2D{sumPoint(lo, scalePoint(axis, end)), hi})
	}
	return paths
}

func sumPoint(p, q Point2D) Point2D   { return Point2D{X: p.X + q.X, Y: p.Y + q.Y} }
func deltaPoint(p, q Point2D) Point2D { return Point2D{X: p.X - q.X, Y: p.Y - q.Y} }
func scalePoint(p Point2D, s float64) Point2D {
	return Point2D{X: p.X * s, Y: p.Y * s}
}
func pointLength(p Point2D) float64 { return math.Hypot(p.X, p.Y) }
func perpendicularPoint(p Point2D) Point2D {
	l := pointLength(p)
	if l == 0 {
		return Point2D{}
	}
	return Point2D{X: -p.Y / l, Y: p.X / l}
}

func safeAnnotationPoint(p Point2D) bool {
	const maxCoordinate = float64(1 << 29)
	return math.Abs(p.X) <= maxCoordinate && math.Abs(p.Y) <= maxCoordinate
}
