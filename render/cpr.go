package render

import (
	"fmt"
	"math"
	"sort"
)

// CPRPathSample is one arc-length sample along a CPR centerline together with a
// stable local frame. The frame is rotation-minimizing (parallel transport),
// not Frenet, so {Tangent, Normal, Binormal} stays continuous through straight
// and low-curvature regions instead of flipping near zero curvature.
type CPRPathSample struct {
	ArcLength float64
	Position  Vec3
	Tangent   Vec3 // unit direction of travel
	Normal    Vec3 // unit, perpendicular to Tangent
	Binormal  Vec3 // unit, Tangent × Normal
}

// CPRPath is a patient-space centerline for curved planar reformation. The
// curve is a centripetal Catmull-Rom spline (alpha = 0.5) through the control
// points: it passes through every control point, has a continuous tangent
// across them, and, unlike the uniform parameterization, forms no cusp or
// self-intersection within a span however unevenly the points are spaced.
//
// Construction measures the arc length of the spline and precomputes a dense
// table of rotation-minimizing frames (double-reflection transport), so
// PointAt, TangentAt and FrameAt are O(log n) lookups and every frame depends
// only on the path and the queried arc length, never on the order or spacing
// of earlier queries. A path is immutable and safe for concurrent use.
type CPRPath struct {
	points  []Vec3    // distinct control points
	spans   []cprSpan // one cubic per pair of consecutive control points
	knotArc []float64 // arc length of each control point
	frames  []cprFrame
	length  float64
}

// cprSpan is the cubic p(t) = ((a t + b) t + c) t + d, t in [0, 1], between
// two consecutive control points.
type cprSpan struct {
	a, b, c, d Vec3
}

func (sp cprSpan) point(t float64) Vec3 {
	return sp.a.Scale(t).Add(sp.b).Scale(t).Add(sp.c).Scale(t).Add(sp.d)
}

func (sp cprSpan) velocity(t float64) Vec3 {
	return sp.a.Scale(3 * t).Add(sp.b.Scale(2)).Scale(t).Add(sp.c)
}

func (sp cprSpan) linear() bool {
	return sp.a == (Vec3{}) && sp.b == (Vec3{})
}

// cprFrame is one entry of the precomputed arc-length table: the spline
// parameter (span, t) at arc length s with its position and
// rotation-minimizing frame.
type cprFrame struct {
	s       float64
	span    int
	t       float64
	pos     Vec3
	tangent Vec3
	normal  Vec3
}

const (
	// cprTableStepMM is the target arc-length spacing of the precomputed frame
	// table. Between entries the spline parameter is found by inverting the
	// arc length (Newton from the linear guess) and the position and tangent
	// are evaluated on the spline exactly.
	cprTableStepMM     = 0.5
	cprTableMinPerSpan = 4
	cprTableMaxPerSpan = 64
	// cprTableMaxEntries bounds the table (about 48 MiB) for the largest
	// accepted control-point count.
	cprTableMaxEntries = 1 << 19
)

// Five-point Gauss-Legendre nodes and weights on [-1, 1], used to integrate
// the spline speed for arc length.
var (
	cprGaussNodes   = [5]float64{-0.9061798459386640, -0.5384693101056831, 0, 0.5384693101056831, 0.9061798459386640}
	cprGaussWeights = [5]float64{0.2369268850561891, 0.4786286704993665, 0.5688888888888889, 0.4786286704993665, 0.2369268850561891}
)

// NewCPRPath builds a bounded path from patient-space control points. It is the
// compatibility form of NewCPRPathChecked and returns nil for invalid or
// excessive input.
func NewCPRPath(controlPoints []Vec3) *CPRPath {
	path, _ := NewCPRPathChecked(controlPoints)
	return path
}

// NewCPRPathChecked validates coordinates, point count, degeneracy and total
// length before allocating path-owned buffers, then fits the centripetal
// Catmull-Rom spline and precomputes its arc-length frame table.
// Consecutive duplicate control points are dropped.
func NewCPRPathChecked(controlPoints []Vec3) (*CPRPath, error) {
	if len(controlPoints) > MaxCPRControlPoints {
		return nil, &CPRLimitError{Resource: "control_points", Value: uint64(len(controlPoints)), Limit: MaxCPRControlPoints}
	}
	pts := make([]Vec3, 0, len(controlPoints))
	for index, p := range controlPoints {
		if !finiteVec3(p) {
			return nil, &CPRInputError{Field: fmt.Sprintf("control_points[%d]", index), Reason: "coordinate is not finite"}
		}
		if len(pts) > 0 && p == pts[len(pts)-1] {
			continue
		}
		pts = append(pts, p)
	}
	if len(pts) < 2 {
		return nil, &CPRInputError{Field: "control_points", Reason: "at least two distinct points are required"}
	}
	chordLength := 0.0
	for i := 1; i < len(pts); i++ {
		segment := pts[i].Sub(pts[i-1]).Length()
		if !positiveFinite(segment) {
			return nil, &CPRInputError{Field: fmt.Sprintf("segment[%d]", i-1), Reason: "length is zero or not finite"}
		}
		chordLength += segment
		if !positiveFinite(chordLength) {
			return nil, &CPRInputError{Field: "path_length", Reason: "length is not finite"}
		}
		if chordLength > MaxCPRPathLengthMM {
			return nil, &CPRLimitError{Resource: "path_length_mm", Value: uint64(math.Ceil(chordLength)), Limit: MaxCPRPathLengthMM}
		}
	}
	path := &CPRPath{points: pts, spans: centripetalCatmullRomSpans(pts)}
	if err := path.buildFrameTable(); err != nil {
		return nil, err
	}
	return path, nil
}

// centripetalCatmullRomSpans converts the centripetal Catmull-Rom spline
// through pts into one cubic Hermite span per pair of consecutive points. Knot
// intervals are |P(i+1) - P(i)|^0.5. The end spans use reflected phantom points
// (P(-1) = 2 P(0) - P(1)), so a two-point path is exactly its straight segment
// traversed at constant speed.
func centripetalCatmullRomSpans(pts []Vec3) []cprSpan {
	n := len(pts)
	spans := make([]cprSpan, n-1)
	for i := 0; i < n-1; i++ {
		d := pts[i+1].Sub(pts[i])
		prev, next := d, d // reflected phantom differences at the ends
		if i > 0 {
			prev = pts[i].Sub(pts[i-1])
		}
		if i+2 < n {
			next = pts[i+2].Sub(pts[i+1])
		}
		m1, m2 := CentripetalCatmullRomTangents(prev, d, next)
		// Written as deviations from the chord so a straight span has exactly
		// zero quadratic and cubic terms.
		e1, e2 := m1.Sub(d), m2.Sub(d)
		spans[i] = cprSpan{
			a: e1.Add(e2),
			b: e1.Scale(-2).Sub(e2),
			c: m1,
			d: pts[i],
		}
	}
	return spans
}

// CentripetalCatmullRomTangents returns a span's endpoint tangents in its
// unit parameter interval. prev, chord and next must be nonzero finite
// differences between successive control points. For an open endpoint,
// repeat chord for the missing neighbor.
func CentripetalCatmullRomTangents(prev, chord, next Vec3) (start, end Vec3) {
	k0 := math.Sqrt(prev.Length())
	k1 := math.Sqrt(chord.Length())
	k2 := math.Sqrt(next.Length())
	start = prev.Scale(k1 / k0).Sub(prev.Add(chord).Scale(k1 / (k0 + k1))).Add(chord)
	end = chord.Sub(chord.Add(next).Scale(k1 / (k1 + k2))).Add(next.Scale(k1 / k2))
	return start, end
}

// buildFrameTable samples every span densely, integrates its arc length and
// transports the rotation-minimizing normal along the samples.
func (p *CPRPath) buildFrameTable() error {
	spanCount := len(p.spans)
	perSpanMax := clampInt(cprTableMaxEntries/spanCount, 2, cprTableMaxPerSpan)
	perSpanMin := min(cprTableMinPerSpan, perSpanMax)
	counts := make([]int, spanCount)
	total := 1
	for i := range p.spans {
		chord := p.points[i+1].Sub(p.points[i]).Length()
		steps := perSpanMax
		if want := math.Ceil(chord / cprTableStepMM); want < float64(perSpanMax) {
			steps = max(perSpanMin, int(want))
		}
		counts[i] = steps
		total += steps
	}

	frames := make([]cprFrame, 0, total)
	knotArc := make([]float64, len(p.points))
	tangent := p.tangentAtParam(0, 0)
	frames = append(frames, cprFrame{pos: p.points[0], tangent: tangent, normal: initialNormal(tangent)})
	s := 0.0
	for i, span := range p.spans {
		steps := counts[i]
		spanStart := s
		speed := span.c.Length()
		for j := 1; j <= steps; j++ {
			t0 := float64(j-1) / float64(steps)
			t1 := float64(j) / float64(steps)
			if span.linear() {
				s = spanStart + speed*t1
			} else {
				s += span.arcLength(t0, t1)
			}
			pos := span.point(t1)
			if j == steps {
				pos = p.points[i+1]
			}
			prev := frames[len(frames)-1]
			tangent := p.tangentAtParam(i, t1)
			normal := rotationMinimizingNormal(prev.pos, prev.tangent, prev.normal, pos, tangent)
			frames = append(frames, cprFrame{s: s, span: i, t: t1, pos: pos, tangent: tangent, normal: normal})
		}
		if !positiveFinite(s) || s <= spanStart {
			return &CPRInputError{Field: fmt.Sprintf("segment[%d]", i), Reason: "arc length is zero or not finite"}
		}
		if s > MaxCPRPathLengthMM {
			return &CPRLimitError{Resource: "path_length_mm", Value: uint64(math.Ceil(s)), Limit: MaxCPRPathLengthMM}
		}
		knotArc[i+1] = s
	}
	p.frames = frames
	p.knotArc = knotArc
	p.length = s
	return nil
}

// arcLength integrates the span speed over [t0, t1].
func (sp cprSpan) arcLength(t0, t1 float64) float64 {
	half := (t1 - t0) / 2
	mid := (t1 + t0) / 2
	sum := 0.0
	for k, x := range cprGaussNodes {
		sum += cprGaussWeights[k] * sp.velocity(mid+half*x).Length()
	}
	return sum * half
}

// tangentAtParam returns the unit spline tangent of span i at t. Where the
// spline speed vanishes (a path that doubles back on itself) it falls back to
// the local secant and then to the span chord.
func (p *CPRPath) tangentAtParam(i int, t float64) Vec3 {
	span := p.spans[i]
	if tangent := span.velocity(t).Normalize(); tangent != (Vec3{}) {
		return tangent
	}
	const h = 1e-6
	if tangent := span.point(math.Min(1, t+h)).Sub(span.point(math.Max(0, t-h))).Normalize(); tangent != (Vec3{}) {
		return tangent
	}
	return p.points[i+1].Sub(p.points[i]).Normalize()
}

// Length is the total arc length (mm) of the centerline.
func (p *CPRPath) Length() float64 {
	if p == nil {
		return 0
	}
	return p.length
}

// ControlPointCount is the number of distinct control points retained.
func (p *CPRPath) ControlPointCount() int {
	if p == nil {
		return 0
	}
	return len(p.points)
}

// ControlPointArcLengths returns the arc length (mm) at which the curve passes
// through each distinct control point, starting at 0 and ending at Length.
func (p *CPRPath) ControlPointArcLengths() []float64 {
	if p == nil {
		return nil
	}
	return append([]float64(nil), p.knotArc...)
}

// PointAt returns the patient-space point at arc length s, clamped to
// [0, Length].
func (p *CPRPath) PointAt(s float64) Vec3 {
	k, span, t, exact := p.locate(s)
	if exact {
		return p.frames[k].pos
	}
	return p.spans[span].point(t)
}

// TangentAt returns the unit tangent of the spline at arc length s, clamped
// to [0, Length]. It is continuous across control points.
func (p *CPRPath) TangentAt(s float64) Vec3 {
	k, span, t, exact := p.locate(s)
	if exact {
		return p.frames[k].tangent
	}
	return p.tangentAtParam(span, t)
}

// locate clamps s to [0, Length] (NaN maps to 0) and returns the table entry k
// at or before it together with the spline parameter at s. exact reports that
// s falls on entry k itself.
func (p *CPRPath) locate(s float64) (k, span int, t float64, exact bool) {
	s = p.clampArc(s)
	last := len(p.frames) - 1
	k = sort.Search(len(p.frames), func(i int) bool { return p.frames[i].s > s }) - 1
	if k < 0 {
		k = 0
	}
	lo := p.frames[k]
	if k >= last || lo.s == s {
		return k, lo.span, lo.t, true
	}
	hi := p.frames[k+1]
	tLo := lo.t
	if lo.span != hi.span {
		tLo = 0
	}
	ds := hi.s - lo.s
	frac := 0.0
	if ds > 0 {
		frac = (s - lo.s) / ds
	}
	t = tLo + (hi.t-tLo)*frac
	if sp := p.spans[hi.span]; !sp.linear() {
		t = sp.invertArcLength(tLo, hi.t, s-lo.s, t, 1e-12*math.Max(1, ds))
	}
	return k, hi.span, t, false
}

// invertArcLength finds t in [tLo, tHi] whose arc length from tLo is target,
// starting from guess, by Newton steps safeguarded with bisection. The table
// keeps each interval short, so it converges in one or two steps.
func (sp cprSpan) invertArcLength(tLo, tHi, target, guess, tolerance float64) float64 {
	lo, hi, t := tLo, tHi, guess
	for iteration := 0; iteration < 8; iteration++ {
		f := sp.arcLength(tLo, t) - target
		if math.Abs(f) <= tolerance {
			break
		}
		if f > 0 {
			hi = t
		} else {
			lo = t
		}
		next := (lo + hi) / 2
		if speed := sp.velocity(t).Length(); speed > 0 {
			if newton := t - f/speed; newton > lo && newton < hi {
				next = newton
			}
		}
		t = next
	}
	return t
}

func (p *CPRPath) clampArc(s float64) float64 {
	if s <= 0 || math.IsNaN(s) {
		return 0
	}
	if s > p.length {
		return p.length
	}
	return s
}

// Resample returns frames spaced at most `spacing` mm apart along the arc
// length, starting at s=0 and always including the path endpoint. The final
// interval may be shorter than spacing. Including both endpoints keeps the
// longitudinal physical field of view independent of the requested sampling
// resolution. Each sample is the FrameAt frame at its arc length, so frames do
// not depend on the spacing. It returns nil for a non-positive or non-finite
// spacing.
func (p *CPRPath) Resample(spacing float64) []CPRPathSample {
	samples, _ := p.ResampleChecked(spacing)
	return samples
}

// ResampleChecked is Resample with typed validation and sample-count errors.
func (p *CPRPath) ResampleChecked(spacing float64) ([]CPRPathSample, error) {
	sampleCount, err := p.SampleCountChecked(spacing)
	if err != nil {
		return nil, err
	}
	regularCount := int(math.Floor(p.length/spacing)) + 1
	samples := make([]CPRPathSample, 0, sampleCount)
	for k := 0; k < regularCount; k++ {
		samples = append(samples, p.FrameAt(float64(k)*spacing))
	}
	if len(samples) > 0 && p.length-samples[len(samples)-1].ArcLength > 1e-9 {
		samples = append(samples, p.FrameAt(p.length))
	}
	return samples, nil
}

// SampleCountChecked returns the exact number of frames ResampleChecked will
// produce, including a shorter endpoint interval, without allocating them.
func (p *CPRPath) SampleCountChecked(spacing float64) (int, error) {
	if p == nil {
		return 0, &CPRInputError{Field: "path", Reason: "path is nil"}
	}
	if !positiveFinite(spacing) {
		return 0, &CPRInputError{Field: "spacing", Reason: "spacing must be positive and finite"}
	}
	regularCount := math.Floor(p.length/spacing) + 1
	if !positiveFinite(regularCount) {
		return 0, &CPRInputError{Field: "sample_count", Reason: "sample count is not finite"}
	}
	count := regularCount
	last := (regularCount - 1) * spacing
	if p.length-last > 1e-9 {
		count++
	}
	if count > MaxCPRPathSamples {
		return 0, &CPRLimitError{Resource: "path_samples", Value: MaxCPRPathSamples + 1, Limit: MaxCPRPathSamples}
	}
	if count < 1 {
		return 0, &CPRInputError{Field: "sample_count", Reason: "sample count is less than one"}
	}
	return int(count), nil
}

// FrameAt returns the rotation-minimizing frame at arc length s, clamped to
// [0, Length]. It looks the nearest precomputed frame up in O(log n) and
// transports it to s, so it equals the frame Resample produces at the same arc
// length.
func (p *CPRPath) FrameAt(s float64) CPRPathSample {
	s = p.clampArc(s)
	k, span, t, exact := p.locate(s)
	lo := p.frames[k]
	position, tangent, normal := lo.pos, lo.tangent, lo.normal
	if !exact {
		position = p.spans[span].point(t)
		tangent = p.tangentAtParam(span, t)
		normal = rotationMinimizingNormal(lo.pos, lo.tangent, lo.normal, position, tangent)
	}
	return CPRPathSample{
		ArcLength: s,
		Position:  position,
		Tangent:   tangent,
		Normal:    normal,
		Binormal:  tangent.Cross(normal).Normalize(),
	}
}

// initialNormal picks a deterministic unit vector perpendicular to the tangent
// to seed the rotation-minimizing frame.
func initialNormal(tangent Vec3) Vec3 {
	ref := Vec3{X: 1}
	if math.Abs(tangent.X) > 0.9 {
		ref = Vec3{Y: 1}
	}
	n := ref.Sub(tangent.Scale(ref.Dot(tangent))).Normalize()
	if n == (Vec3{}) {
		ref = Vec3{Z: 1}
		n = ref.Sub(tangent.Scale(ref.Dot(tangent))).Normalize()
	}
	return n
}

// rotationMinimizingNormal transports the normal of the frame at fromPos to
// toPos with the double-reflection method of Wang, Jüttler, Zheng and Liu
// ("Computation of rotation minimizing frames", ACM TOG 27(1), 2008): reflect
// through the plane bisecting the chord, then through the plane that maps the
// reflected tangent onto the new one. The result is re-orthonormalized against
// the new tangent.
func rotationMinimizingNormal(fromPos, fromTangent, fromNormal, toPos, toTangent Vec3) Vec3 {
	reflectedNormal, reflectedTangent := fromNormal, fromTangent
	chord := toPos.Sub(fromPos)
	if c1 := chord.Dot(chord); c1 > 0 {
		reflectedNormal = fromNormal.Sub(chord.Scale(2 * chord.Dot(fromNormal) / c1))
		reflectedTangent = fromTangent.Sub(chord.Scale(2 * chord.Dot(fromTangent) / c1))
	}
	normal := reflectedNormal
	v2 := toTangent.Sub(reflectedTangent)
	if c2 := v2.Dot(v2); c2 > 0 {
		normal = reflectedNormal.Sub(v2.Scale(2 * v2.Dot(reflectedNormal) / c2))
	}
	binormal := toTangent.Cross(normal).Normalize()
	if binormal == (Vec3{}) {
		// The transported normal collapsed onto the tangent (a path that
		// reverses direction); rotate the previous normal minimally instead.
		binormal = toTangent.Cross(parallelTransport(fromNormal, fromTangent, toTangent)).Normalize()
		if binormal == (Vec3{}) {
			return initialNormal(toTangent)
		}
	}
	return binormal.Cross(toTangent).Normalize()
}

// parallelTransport rotates a normal from the previous tangent frame onto the
// new tangent by the minimal rotation between the two tangents. When the
// tangents are (anti)parallel the normal is carried forward unchanged.
func parallelTransport(normal, prevTangent, tangent Vec3) Vec3 {
	axis := prevTangent.Cross(tangent)
	axisLen := axis.Length()
	if axisLen < 1e-9 {
		return normal
	}
	axis = axis.Scale(1 / axisLen)
	angle := math.Atan2(axisLen, prevTangent.Dot(tangent))
	return normal.Rotate(axis, angle)
}
