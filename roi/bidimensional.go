package roi

import "math"

// Bidimensional is the bidimensional tumour measurement of RECIST and WHO
// (#1074): a long axis and a short axis perpendicular to it, crossing it
// within both segments. Points are sub-pixel image coordinates (an integer
// is a pixel centre); perpendicularity is kept in millimetres, so it holds
// on the patient even when the pixels are not square.
//
// The editing rules follow the behaviour described for VTK's
// vtkBiDimensionalWidget: the short axis is created on both sides of the
// long axis and keeps its perpendicular relation through every edit.
type Bidimensional struct {
	LongStart, LongEnd   Point2D
	ShortStart, ShortEnd Point2D
}

// Bidimensional handle indices for MoveBidimensionalHandle.
const (
	BidimensionalLongStart = iota
	BidimensionalLongEnd
	BidimensionalShortStart
	BidimensionalShortEnd
)

// bidimensionalFrame is a Bidimensional in millimetres: the long axis from
// a along u with length length, the short axis at fraction t of it with its
// endpoints at signed distances d0 and d1 along the normal n.
type bidimensionalFrame struct {
	a, u, n [2]float64
	length  float64
	t       float64
	d0, d1  float64
	sx, sy  float64
}

func bidimensionalScale(sp MeasureSpacing) (float64, float64) {
	if sp.valid() {
		return sp.spacingXY()
	}
	return 1, 1
}

func newBidimensionalAxis(start, end Point2D, sp MeasureSpacing) (bidimensionalFrame, bool) {
	sx, sy := bidimensionalScale(sp)
	f := bidimensionalFrame{sx: sx, sy: sy, a: [2]float64{start.X * sx, start.Y * sy}}
	dx, dy := (end.X-start.X)*sx, (end.Y-start.Y)*sy
	f.length = math.Hypot(dx, dy)
	if !(f.length > 0) || math.IsInf(f.length, 0) {
		return bidimensionalFrame{}, false
	}
	f.u = [2]float64{dx / f.length, dy / f.length}
	f.n = [2]float64{-f.u[1], f.u[0]}
	return f, true
}

func (f bidimensionalFrame) mm(p Point2D) [2]float64 { return [2]float64{p.X * f.sx, p.Y * f.sy} }

func (f bidimensionalFrame) along(p Point2D) float64 {
	q := f.mm(p)
	return (q[0]-f.a[0])*f.u[0] + (q[1]-f.a[1])*f.u[1]
}

func (f bidimensionalFrame) across(p Point2D) float64 {
	q := f.mm(p)
	return (q[0]-f.a[0])*f.n[0] + (q[1]-f.a[1])*f.n[1]
}

func (f bidimensionalFrame) point(along, across float64) Point2D {
	x := f.a[0] + f.u[0]*along + f.n[0]*across
	y := f.a[1] + f.u[1]*along + f.n[1]*across
	return Point2D{X: x / f.sx, Y: y / f.sy}
}

// frame decomposes b; the foot of the short axis is the mean projection of
// its endpoints, so a slightly skewed input is read as the nearest
// perpendicular configuration.
func (b Bidimensional) frame(sp MeasureSpacing) (bidimensionalFrame, bool) {
	f, ok := newBidimensionalAxis(b.LongStart, b.LongEnd, sp)
	if !ok {
		return bidimensionalFrame{}, false
	}
	f.t = (f.along(b.ShortStart) + f.along(b.ShortEnd)) / 2 / f.length
	f.d0, f.d1 = f.across(b.ShortStart), f.across(b.ShortEnd)
	return f, finiteAll(f.t, f.d0, f.d1)
}

func (f bidimensionalFrame) build() Bidimensional {
	foot := f.length * math.Max(0, math.Min(1, f.t))
	return Bidimensional{
		LongStart:  f.point(0, 0),
		LongEnd:    f.point(f.length, 0),
		ShortStart: f.point(foot, f.d0),
		ShortEnd:   f.point(foot, f.d1),
	}
}

// shortAxisUsable rejects a short axis collapsed to (almost) nothing, whose
// direction round-off would dominate.
func (f bidimensionalFrame) shortAxisUsable() bool {
	return math.Abs(f.d1-f.d0) > 1e-6*math.Max(1, f.length)
}

func finiteAll(values ...float64) bool {
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// NewBidimensional builds a bidimensional measurement from its long axis and
// one endpoint of the short axis: the short axis crosses the long axis at
// the foot of the perpendicular from that point (kept within the long axis)
// and extends as far on the other side. ok is false for a zero-length long
// axis or a point on it.
func NewBidimensional(longStart, longEnd, shortPoint Point2D, sp MeasureSpacing) (Bidimensional, bool) {
	f, ok := newBidimensionalAxis(longStart, longEnd, sp)
	if !ok {
		return Bidimensional{}, false
	}
	d := f.across(shortPoint)
	f.t = f.along(shortPoint) / f.length
	f.d0, f.d1 = d, -d
	if !f.shortAxisUsable() || !finiteAll(d, f.t) {
		return Bidimensional{}, false
	}
	out := f.build()
	out.LongStart, out.LongEnd = longStart, longEnd
	return out, true
}

// Constrained returns b with the short axis made exactly perpendicular to
// the long axis, crossing it within its extent and with its endpoints on
// opposite sides (or on) the long axis. ok is false when no measurement
// remains.
func (b Bidimensional) Constrained(sp MeasureSpacing) (Bidimensional, bool) {
	f, ok := b.frame(sp)
	if !ok {
		return Bidimensional{}, false
	}
	if f.d0*f.d1 > 0 {
		// Both endpoints on one side: the nearer one moves onto the axis.
		if math.Abs(f.d0) < math.Abs(f.d1) {
			f.d0 = 0
		} else {
			f.d1 = 0
		}
	}
	if !f.shortAxisUsable() {
		return Bidimensional{}, false
	}
	out := f.build()
	out.LongStart, out.LongEnd = b.LongStart, b.LongEnd
	return out, true
}

// MoveBidimensionalHandle moves one endpoint to target keeping the axes
// perpendicular. A long-axis endpoint turns and stretches the long axis about
// the other one; the short axis follows, keeping its relative position along
// the long axis and its lengths on each side. A short-axis endpoint slides
// along the short axis to the projection of target, and cannot cross to the
// other side of the long axis. ok is false when the edit would leave a
// zero-length axis.
func MoveBidimensionalHandle(b Bidimensional, handle int, target Point2D, sp MeasureSpacing) (Bidimensional, bool) {
	if !finiteAll(target.X, target.Y) {
		return Bidimensional{}, false
	}
	f, ok := b.frame(sp)
	if !ok {
		return Bidimensional{}, false
	}
	switch handle {
	case BidimensionalLongStart, BidimensionalLongEnd:
		start, end := b.LongStart, b.LongEnd
		if handle == BidimensionalLongStart {
			start = target
		} else {
			end = target
		}
		g, ok := newBidimensionalAxis(start, end, sp)
		if !ok {
			return Bidimensional{}, false
		}
		g.t, g.d0, g.d1 = f.t, f.d0, f.d1
		out := g.build()
		out.LongStart, out.LongEnd = start, end
		return out, true
	case BidimensionalShortStart, BidimensionalShortEnd:
		d := f.across(target)
		// Keep the endpoint on its own side of the long axis.
		other := f.d1
		if handle == BidimensionalShortEnd {
			other = f.d0
		}
		if d*other > 0 {
			d = 0
		}
		if handle == BidimensionalShortStart {
			f.d0 = d
		} else {
			f.d1 = d
		}
		if !f.shortAxisUsable() {
			return Bidimensional{}, false
		}
		out := f.build()
		out.LongStart, out.LongEnd = b.LongStart, b.LongEnd
		return out, true
	default:
		return Bidimensional{}, false
	}
}

// SlideBidimensionalShortAxis moves the short axis along the long axis to
// the projection of target, within the long axis.
func SlideBidimensionalShortAxis(b Bidimensional, target Point2D, sp MeasureSpacing) (Bidimensional, bool) {
	f, ok := b.frame(sp)
	if !ok || !finiteAll(target.X, target.Y) {
		return Bidimensional{}, false
	}
	f.t = f.along(target) / f.length
	out := f.build()
	out.LongStart, out.LongEnd = b.LongStart, b.LongEnd
	return out, true
}

// Intersection returns where the axes cross, in image coordinates.
func (b Bidimensional) Intersection(sp MeasureSpacing) (Point2D, bool) {
	f, ok := b.frame(sp)
	if !ok {
		return Point2D{}, false
	}
	return f.point(f.length*math.Max(0, math.Min(1, f.t)), 0), true
}

// PerpendicularityError returns the angle in degrees between the short axis
// and the normal of the long axis, measured in millimetres.
func (b Bidimensional) PerpendicularityError(sp MeasureSpacing) float64 {
	f, ok := b.frame(sp)
	if !ok {
		return math.NaN()
	}
	s0, s1 := f.mm(b.ShortStart), f.mm(b.ShortEnd)
	dx, dy := s1[0]-s0[0], s1[1]-s0[1]
	length := math.Hypot(dx, dy)
	if length == 0 {
		return math.NaN()
	}
	cosine := math.Abs(dx*f.u[0]+dy*f.u[1]) / length
	return 90 - math.Acos(math.Min(1, cosine))*180/math.Pi
}

// Points returns the four points in handle order.
func (b Bidimensional) Points() []Point2D {
	return []Point2D{b.LongStart, b.LongEnd, b.ShortStart, b.ShortEnd}
}

// BidimensionalFromPoints reads the four points of Points.
func BidimensionalFromPoints(points []Point2D) (Bidimensional, bool) {
	if len(points) != 4 {
		return Bidimensional{}, false
	}
	return Bidimensional{LongStart: points[0], LongEnd: points[1], ShortStart: points[2], ShortEnd: points[3]}, true
}

// BidimensionalAxesMM returns the long and short axis lengths in mm; ok is
// false without a valid spacing.
func BidimensionalAxesMM(b Bidimensional, sp MeasureSpacing) (long, short float64, ok bool) {
	long, okLong := SubpixelLengthMM(b.LongStart, b.LongEnd, sp)
	short, okShort := SubpixelLengthMM(b.ShortStart, b.ShortEnd, sp)
	return long, short, okLong && okShort
}
