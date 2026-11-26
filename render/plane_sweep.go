package render

import (
	"fmt"
	"math"
)

type PlaneSweepKind string

const (
	PlaneSweepParallel   PlaneSweepKind = "parallel"
	PlaneSweepRotational PlaneSweepKind = "rotational"
	MaxPlaneSweepCount                  = 512
)

// PlaneSweepOptions uses patient LPS mm. Parallel Start/End/Step are mm along
// Base.U cross Base.V; rotational values are degrees about a fixed patient-space
// axis through Pivot. Step is positive; Start/End determine traversal direction.
// Both endpoints are included; the final interval may be shorter than Step.
type PlaneSweepOptions struct {
	Kind             PlaneSweepKind
	Base             Plane
	Start, End, Step float64
	Axis, Pivot      Vec3
	SliceThicknessMM float64
	MaxPlanes        int
}

// PlaneSweep is a bounded immutable geometry generator, shared by previews and
// exports. Construction allocates no plane/image arrays and samples no volume.
type PlaneSweep struct {
	options   PlaneSweepOptions
	count     int
	direction float64
	normal    Vec3
}

func NewPlaneSweep(o PlaneSweepOptions) (PlaneSweep, error) {
	fail := func(detail string) (PlaneSweep, error) {
		return PlaneSweep{}, fmt.Errorf("%w: plane sweep %s", ErrInvalidViewState, detail)
	}
	if o.Kind != PlaneSweepParallel && o.Kind != PlaneSweepRotational {
		return fail("kind")
	}
	if !validSweepPlane(o.Base) || !finiteScalar(o.Start) || !finiteScalar(o.End) || !finiteScalar(o.Step) || o.Step <= 0 ||
		!finiteScalar(o.SliceThicknessMM) || o.SliceThicknessMM <= 0 || o.SliceThicknessMM > 1000 {
		return fail("geometry, interval or thickness")
	}
	if o.MaxPlanes == 0 {
		o.MaxPlanes = MaxPlaneSweepCount
	}
	if o.MaxPlanes < 1 || o.MaxPlanes > MaxPlaneSweepCount {
		return fail("count cap")
	}
	bound := 10000.
	if o.Kind == PlaneSweepRotational {
		bound = 360
		if !finiteSweepVec(o.Axis) || !finiteSweepVec(o.Pivot) || o.Axis.Length() < 1e-12 || !finiteScalar(o.Axis.Length()) {
			return fail("rotation axis or pivot")
		}
		o.Axis = o.Axis.Normalize()
	}
	span := math.Abs(o.End - o.Start)
	if math.Abs(o.Start) > bound || math.Abs(o.End) > bound || span > bound || o.Step > 2*bound {
		return fail("physical/angular range")
	}
	intervals := span / o.Step
	if !finiteScalar(intervals) || intervals > float64(o.MaxPlanes-1) {
		return fail("exceeds count cap")
	}
	nearest := math.Round(intervals)
	if math.Abs(intervals-nearest) <= 1e-10 {
		intervals = nearest
	}
	count := int(math.Ceil(intervals)) + 1
	if count > o.MaxPlanes {
		return fail("exceeds count cap")
	}
	direction := 1.
	if o.End < o.Start {
		direction = -1
	}
	p := PlaneSweep{options: o, count: count, direction: direction, normal: o.Base.U.Normalize().Cross(o.Base.V.Normalize()).Normalize()}
	for i := 0; i < count; i++ {
		if _, ok := p.Plane(i); !ok {
			return fail("generated geometry is not finite")
		}
	}
	return p, nil
}

func (p PlaneSweep) Count() int                 { return p.count }
func (p PlaneSweep) Options() PlaneSweepOptions { return p.options }

// Position is the offset in mm or angle in degrees for the indexed plane.
func (p PlaneSweep) Position(index int) (float64, bool) {
	if index < 0 || index >= p.count {
		return 0, false
	}
	if index == p.count-1 {
		return p.options.End, true
	}
	return p.options.Start + p.direction*float64(index)*p.options.Step, true
}

func (p PlaneSweep) Plane(index int) (Plane, bool) {
	position, ok := p.Position(index)
	if !ok {
		return Plane{}, false
	}
	o := p.options
	if o.Kind == PlaneSweepParallel {
		out := o.Base
		out.Origin = out.Origin.Add(p.normal.Scale(position))
		return out, validSweepPlane(out)
	}
	angle := position * math.Pi / 180
	out := Plane{Origin: o.Pivot.Add(o.Base.Origin.Sub(o.Pivot).Rotate(o.Axis, angle)), U: o.Base.U.Rotate(o.Axis, angle), V: o.Base.V.Rotate(o.Axis, angle)}
	return out, validSweepPlane(out)
}

func finiteSweepVec(v Vec3) bool { return finiteScalar(v.X) && finiteScalar(v.Y) && finiteScalar(v.Z) }
func validSweepPlane(p Plane) bool {
	u, v := p.U.Length(), p.V.Length()
	return finiteSweepVec(p.Origin) && finiteSweepVec(p.U) && finiteSweepVec(p.V) && finiteScalar(u) && finiteScalar(v) && u > 1e-12 && v > 1e-12 && math.Abs(p.U.Normalize().Dot(p.V.Normalize())) <= 1e-6
}

// PlaneCrossSection clips the target plane's intersection to a reference image
// rectangle. Endpoints are fractional image coordinates [0,1]. A coplanar
// target is reported separately; parallel disjoint/outside planes have no line.
// This is geometry only, with no sampling or compositing of volume values.
func PlaneCrossSection(reference, target Plane) (line [2][2]float64, visible, coplanar bool) {
	if !validSweepPlane(reference) || !validSweepPlane(target) {
		return line, false, false
	}
	n := target.U.Normalize().Cross(target.V.Normalize()).Normalize()
	a, b, c := n.Dot(reference.U), n.Dot(reference.V), n.Dot(reference.Origin.Sub(target.Origin))
	tolerance := 1e-9 * math.Max(1, math.Max(reference.U.Length(), reference.V.Length()))
	if math.Abs(a) <= tolerance && math.Abs(b) <= tolerance {
		return line, false, math.Abs(c) <= tolerance
	}
	var points [4][2]float64
	count := 0
	add := func(x, y float64) {
		if !finiteScalar(x) || !finiteScalar(y) || x < -1e-9 || x > 1+1e-9 || y < -1e-9 || y > 1+1e-9 {
			return
		}
		x, y = math.Max(0, math.Min(1, x)), math.Max(0, math.Min(1, y))
		for i := 0; i < count; i++ {
			if math.Hypot(points[i][0]-x, points[i][1]-y) < 1e-9 {
				return
			}
		}
		if count < len(points) {
			points[count] = [2]float64{x, y}
			count++
		}
	}
	if math.Abs(b) > tolerance {
		add(0, -c/b)
		add(1, -(a+c)/b)
	}
	if math.Abs(a) > tolerance {
		add(-c/a, 0)
		add(-(b+c)/a, 1)
	}
	if count < 2 {
		return line, false, false
	}
	line = [2][2]float64{points[0], points[1]}
	return line, true, false
}
