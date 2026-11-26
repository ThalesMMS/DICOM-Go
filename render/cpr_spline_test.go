package render

import (
	"context"
	"encoding/binary"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// Analytic checks for the centripetal Catmull-Rom centerline and its
// precomputed rotation-minimizing frames (#1011).

func cprAngleDegrees(a, b Vec3) float64 {
	return math.Atan2(a.Cross(b).Length(), a.Dot(b)) * 180 / math.Pi
}

func cprArcPoints(centre Vec3, radius, from, to float64, count int) []Vec3 {
	points := make([]Vec3, count)
	for i := range points {
		a := from + (to-from)*float64(i)/float64(count-1)
		points[i] = centre.Add(Vec3{X: radius * math.Cos(a), Y: radius * math.Sin(a)})
	}
	return points
}

func TestCPRPathPassesThroughUnevenControlPoints(t *testing.T) {
	points := []Vec3{
		{X: 1, Y: 2, Z: 3}, {X: 1.4, Y: 2.1, Z: 3}, {X: 9, Y: 5, Z: 4},
		{X: 9.2, Y: 5.5, Z: 4.1}, {X: 20, Y: -3, Z: 8}, {X: 21, Y: -3, Z: 30},
	}
	path, err := NewCPRPathChecked(points)
	if err != nil {
		t.Fatal(err)
	}
	arcs := path.ControlPointArcLengths()
	if len(arcs) != len(points) || arcs[0] != 0 || arcs[len(arcs)-1] != path.Length() {
		t.Fatalf("control point arcs = %v, want %d values from 0 to Length %.6f", arcs, len(points), path.Length())
	}
	for i, arc := range arcs {
		if i > 0 && arc <= arcs[i-1] {
			t.Fatalf("control point arcs not increasing: %v", arcs)
		}
		if i > 0 && arc-arcs[i-1] < points[i].Sub(points[i-1]).Length()-1e-9 {
			t.Fatalf("span %d arc %.6f is shorter than its chord", i-1, arc-arcs[i-1])
		}
		if got := path.PointAt(arc); got != points[i] {
			t.Fatalf("PointAt(arc of control point %d) = %+v, want %+v", i, got, points[i])
		}
		if frame := path.FrameAt(arc); frame.Position != points[i] {
			t.Fatalf("FrameAt(arc of control point %d).Position = %+v, want %+v", i, frame.Position, points[i])
		}
	}
}

func TestCPRPathStraightPathsReproduceTheLine(t *testing.T) {
	start := Vec3{X: -4, Y: 7, Z: 2}
	dir := Vec3{X: 2, Y: -1, Z: 2}.Normalize()
	t.Run("two points", func(t *testing.T) {
		path := NewCPRPath([]Vec3{start, start.Add(dir.Scale(30))})
		if got := path.Length(); math.Abs(got-30) > 1e-12 {
			t.Fatalf("Length = %.15f, want 30", got)
		}
		for s := 0.0; s <= 30; s += 0.37 {
			frame := path.FrameAt(s)
			if d := frame.Position.Sub(start.Add(dir.Scale(s))).Length(); d > 1e-12 {
				t.Fatalf("PointAt(%.2f) is %.3g mm off the line", s, d)
			}
			if d := frame.Tangent.Sub(dir).Length(); d > 1e-12 {
				t.Fatalf("TangentAt(%.2f) = %+v, want %+v", s, frame.Tangent, dir)
			}
			if d := frame.Normal.Sub(initialNormal(dir)).Length(); d > 1e-12 {
				t.Fatalf("Normal(%.2f) rotated on a straight path: %+v", s, frame.Normal)
			}
		}
	})
	// Unevenly spaced collinear points are the configuration where the
	// uniform Catmull-Rom spline backtracks: the curve must stay on the line
	// and advance monotonically.
	t.Run("uneven collinear points", func(t *testing.T) {
		offsets := []float64{0, 1, 1.1, 10, 10.05, 30}
		points := make([]Vec3, len(offsets))
		for i, offset := range offsets {
			points[i] = start.Add(dir.Scale(offset))
		}
		path := NewCPRPath(points)
		if got := path.Length(); math.Abs(got-30) > 1e-9 {
			t.Fatalf("Length = %.12f, want 30", got)
		}
		previous := -1.0
		for _, sample := range path.Resample(0.01) {
			offset := sample.Position.Sub(start)
			along := offset.Dot(dir)
			if d := offset.Sub(dir.Scale(along)).Length(); d > 1e-9 {
				t.Fatalf("s=%.2f left the line by %.3g mm", sample.ArcLength, d)
			}
			if along <= previous {
				t.Fatalf("s=%.2f backtracked: %.9f after %.9f", sample.ArcLength, along, previous)
			}
			if math.Abs(along-sample.ArcLength) > 1e-6 {
				t.Fatalf("s=%.2f lies at %.9f along the line, want arc-length parameterization", sample.ArcLength, along)
			}
			previous = along
		}
	})
}

// uniformCatmullRom evaluates the uniform (alpha = 0) Catmull-Rom spline with
// the same reflected end points, for comparison only.
func uniformCatmullRom(points []Vec3, perSpan int) []Vec3 {
	at := func(i int) Vec3 {
		if i < 0 {
			return points[0].Scale(2).Sub(points[1])
		}
		if i >= len(points) {
			n := len(points)
			return points[n-1].Scale(2).Sub(points[n-2])
		}
		return points[i]
	}
	var out []Vec3
	for i := 0; i+1 < len(points); i++ {
		p0, p1, p2, p3 := at(i-1), at(i), at(i+1), at(i+2)
		m1, m2 := p2.Sub(p0).Scale(0.5), p3.Sub(p1).Scale(0.5)
		for j := 0; j < perSpan; j++ {
			t := float64(j) / float64(perSpan)
			h00, h10 := 2*t*t*t-3*t*t+1, t*t*t-2*t*t+t
			h01, h11 := -2*t*t*t+3*t*t, t*t*t-t*t
			out = append(out, p1.Scale(h00).Add(m1.Scale(h10)).Add(p2.Scale(h01)).Add(m2.Scale(h11)))
		}
	}
	return append(out, points[len(points)-1])
}

// cprPolylineSelfIntersects reports whether two non-adjacent segments of a
// planar (z = 0) polyline cross.
func cprPolylineSelfIntersects(points []Vec3) bool {
	cross := func(o, a, b Vec3) float64 { return (a.X-o.X)*(b.Y-o.Y) - (a.Y-o.Y)*(b.X-o.X) }
	for i := 0; i+1 < len(points); i++ {
		for j := i + 2; j+1 < len(points); j++ {
			a, b, c, d := points[i], points[i+1], points[j], points[j+1]
			if cross(a, b, c)*cross(a, b, d) < 0 && cross(c, d, a)*cross(c, d, b) < 0 {
				return true
			}
		}
	}
	return false
}

func TestCPRPathCentripetalSplineHasNoLoopWhereUniformSplineLoops(t *testing.T) {
	// A short kink between a long straight run and a sharp turn: the uniform
	// parameterization overshoots the short span and loops back on itself.
	points := []Vec3{{}, {X: 10}, {X: 10.2, Y: 0.1}, {X: 11, Y: 10}}
	if !cprPolylineSelfIntersects(uniformCatmullRom(points, 200)) {
		t.Fatal("uniform Catmull-Rom does not loop on the pathological configuration; the test lost its point")
	}
	path := NewCPRPath(points)
	var curve []Vec3
	for _, sample := range path.Resample(0.005) {
		curve = append(curve, sample.Position)
	}
	if cprPolylineSelfIntersects(curve) {
		t.Fatal("centripetal centerline self-intersects")
	}
	arcs := path.ControlPointArcLengths()
	for span := 0; span+1 < len(points); span++ {
		chord := points[span+1].Sub(points[span]).Normalize()
		for s := arcs[span]; s <= arcs[span+1]; s += 0.001 {
			if d := path.TangentAt(s).Dot(chord); d <= 0 {
				t.Fatalf("span %d reverses at s=%.3f (tangent·chord=%.3f): cusp", span, s, d)
			}
		}
	}
}

func TestCPRPathTangentIsContinuousAcrossControlPoints(t *testing.T) {
	path := NewCPRPath(cprArcPoints(Vec3{}, 50, 0, math.Pi/2, 5))
	const eps = 1e-6
	for i, arc := range path.ControlPointArcLengths() {
		if angle := cprAngleDegrees(path.TangentAt(arc-eps), path.TangentAt(arc+eps)); angle > 1e-3 {
			t.Fatalf("tangent jumps %.4f° at control point %d (the polyline jumped 22.5°)", angle, i)
		}
		if angle := cprAngleDegrees(path.FrameAt(arc-eps).Normal, path.FrameAt(arc+eps).Normal); angle > 1e-3 {
			t.Fatalf("normal jumps %.4f° at control point %d", angle, i)
		}
	}
	// Between the interior control points the turning rate stays close to the
	// circle's 1/50 rad/mm. The end spans start along the end chord (reflected
	// end condition) and so turn faster to rejoin the arc.
	arcs := path.ControlPointArcLengths()
	worstInner, worstEnd := 0.0, 0.0
	for s := 0.0; s+0.1 <= path.Length(); s += 0.1 {
		rate := cprAngleDegrees(path.TangentAt(s), path.TangentAt(s+0.1)) * math.Pi / 180 / 0.1
		if s >= arcs[1] && s+0.1 <= arcs[len(arcs)-2] {
			worstInner = math.Max(worstInner, rate)
		} else {
			worstEnd = math.Max(worstEnd, rate)
		}
	}
	t.Logf("max turning rate: interior %.5f rad/mm, end spans %.5f rad/mm (circle 0.02)", worstInner, worstEnd)
	if math.Abs(worstInner-1.0/50) > 0.15/50 {
		t.Fatalf("interior turning rate %.5f rad/mm, want 1/50 within 15%%", worstInner)
	}
	if worstEnd > 2.5/50 {
		t.Fatalf("end span turning rate %.5f rad/mm, want bounded", worstEnd)
	}
}

func TestCPRPathLengthConvergesOnCircularArc(t *testing.T) {
	const radius = 50.0
	want := radius * math.Pi / 2
	previousError := math.Inf(1)
	for _, count := range []int{5, 9, 17, 33} {
		path := NewCPRPath(cprArcPoints(Vec3{}, radius, 0, math.Pi/2, count))
		relative := math.Abs(path.Length()-want) / want
		chordSum := 2 * radius * math.Sin(math.Pi/4/float64(count-1)) * float64(count-1)
		t.Logf("%2d points: length %.5f mm (arc %.5f, chord sum %.5f), relative error %.2e", count, path.Length(), want, chordSum, relative)
		if relative >= previousError {
			t.Fatalf("%d points: error %.3g did not shrink from %.3g", count, relative, previousError)
		}
		if polyline := (want - chordSum) / want; relative > polyline/2 {
			t.Fatalf("%d points: error %.3g, want well below the polyline's %.3g", count, relative, polyline)
		}
		previousError = relative
	}
	if previousError > 1e-4 {
		t.Fatalf("33-point arc length error %.3g, want < 1e-4", previousError)
	}
}

func TestCPRPathResampleIsUniformInArcLength(t *testing.T) {
	path := NewCPRPath(cprHelixControlPoints(Vec3{X: 5}, 20, 30, 2, 17))
	const spacing = 0.5
	samples := path.Resample(spacing)
	if len(samples) < 100 {
		t.Fatalf("got %d samples", len(samples))
	}
	for k := 1; k < len(samples); k++ {
		gotArc := samples[k].ArcLength - samples[k-1].ArcLength
		if k < len(samples)-1 && gotArc != spacing {
			t.Fatalf("sample %d arc step %.15f, want %.1f", k, gotArc, spacing)
		}
		// The chord of an arc of length h and curvature κ is h - κ²h³/24.
		chord := samples[k].Position.Sub(samples[k-1].Position).Length()
		if math.Abs(chord-gotArc) > 1e-3*gotArc {
			t.Fatalf("sample %d chord %.6f mm for arc step %.6f mm: parameterization is not arc length", k, chord, gotArc)
		}
	}
}

func TestCPRPathFramesAreOrthonormalAndRotationMinimizingOnHelix(t *testing.T) {
	const radius, pitch = 20.0, 30.0 // pitch in mm per turn
	c := pitch / (2 * math.Pi)
	speed := math.Hypot(radius, c)
	torsion := c / (speed * speed)
	path := NewCPRPath(cprHelixControlPoints(Vec3{}, radius, pitch, 3, 97)) // 32 points per turn
	const step = 0.25
	samples := path.Resample(step)
	for _, sample := range samples {
		n, b, tan := sample.Normal, sample.Binormal, sample.Tangent
		for _, v := range []float64{n.Length() - 1, b.Length() - 1, tan.Length() - 1, n.Dot(tan), b.Dot(tan), n.Dot(b), b.Sub(tan.Cross(n)).Length()} {
			if math.Abs(v) > 1e-12 {
				t.Fatalf("s=%.2f frame is not orthonormal: T=%+v N=%+v B=%+v", sample.ArcLength, tan, n, b)
			}
		}
	}
	// A rotation-minimizing frame has no angular velocity about the tangent
	// (dN/ds·B = 0); the Frenet frame of this helix turns about it at the
	// torsion. Central differences keep the estimate second-order.
	worst := 0.0
	for k := 1; k+1 < len(samples); k++ {
		twist := math.Abs(samples[k+1].Normal.Sub(samples[k-1].Normal).Dot(samples[k].Binormal)) / (2 * step)
		worst = math.Max(worst, twist)
	}
	t.Logf("max twist about the tangent %.3g rad/mm, helix torsion %.3g rad/mm", worst, torsion)
	if worst > 0.03*torsion {
		t.Fatalf("normal twists about the tangent at %.3g rad/mm, want ≪ torsion %.3g", worst, torsion)
	}
	// Relative to the Frenet normal (pointing at the axis) the frame turns at
	// exactly minus the torsion: 2π·c/sqrt(r²+c²) per helix turn.
	angle := func(sample CPRPathSample) float64 {
		frenet := Vec3{X: -sample.Position.X, Y: -sample.Position.Y}.Normalize()
		return math.Atan2(sample.Tangent.Dot(frenet.Cross(sample.Normal)), frenet.Dot(sample.Normal))
	}
	turn := 2 * math.Pi * speed
	from, to := path.FrameAt(turn), path.FrameAt(2*turn)
	total := 0.0
	previous := angle(from)
	for s := turn; s <= 2*turn; s += step {
		current := angle(path.FrameAt(s))
		delta := math.Remainder(current-previous, 2*math.Pi)
		total += delta
		previous = current
	}
	total += math.Remainder(angle(to)-previous, 2*math.Pi)
	want := -torsion * turn
	t.Logf("frame rotation relative to Frenet over one turn: %.5f rad, analytic %.5f rad", total, want)
	if math.Abs(total-want) > 0.01*math.Abs(want) {
		t.Fatalf("frame rotated %.5f rad relative to Frenet over one turn, want %.5f", total, want)
	}
}

func TestCPRPathFramesDoNotDependOnQuerySpacing(t *testing.T) {
	path := NewCPRPath(cprHelixControlPoints(Vec3{Y: 3}, 15, 40, 1.5, 23))
	fine, coarse := path.Resample(0.5), path.Resample(1.5)
	for k, sample := range coarse[:len(coarse)-1] {
		if got := fine[3*k]; got != sample {
			t.Fatalf("s=%.1f: Resample(0.5) frame %+v differs from Resample(1.5) frame %+v", sample.ArcLength, got, sample)
		}
		if got := path.FrameAt(sample.ArcLength); got != sample {
			t.Fatalf("s=%.1f: FrameAt %+v differs from Resample %+v", sample.ArcLength, got, sample)
		}
	}
	if last, end := coarse[len(coarse)-1], path.FrameAt(path.Length()); last != end {
		t.Fatalf("Resample end frame %+v differs from FrameAt(Length) %+v", last, end)
	}
}

func TestCPRPathSurvivesPathsThatDoubleBack(t *testing.T) {
	path, err := NewCPRPathChecked([]Vec3{{}, {X: 10}, {}, {X: 10, Y: 0.001}})
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range path.Resample(0.1) {
		for _, v := range []Vec3{sample.Position, sample.Tangent, sample.Normal, sample.Binormal} {
			if !finiteVec3(v) {
				t.Fatalf("s=%.2f produced a non-finite frame %+v", sample.ArcLength, sample)
			}
		}
		if math.Abs(sample.Tangent.Length()-1) > 1e-9 || math.Abs(sample.Normal.Length()-1) > 1e-9 || math.Abs(sample.Normal.Dot(sample.Tangent)) > 1e-9 {
			t.Fatalf("s=%.2f frame is not orthonormal: %+v", sample.ArcLength, sample)
		}
	}
}

// cprRadialDistanceStack encodes the signed distance from a vertical cylinder
// of radius R around (cx, cy) as 100 + 40·(r - R), a field trilinear
// interpolation reproduces almost exactly near the cylinder.
func cprRadialDistanceStack(rows, cols, depth int, cx, cy, radius float64) *Stack {
	stack := &Stack{PixelSpacing: []float64{1, 1}, SliceThickness: 1}
	for z := 0; z < depth; z++ {
		data := make([]byte, rows*cols)
		for y := 0; y < rows; y++ {
			for x := 0; x < cols; x++ {
				v := 100 + 40*(math.Hypot(float64(x)-cx, float64(y)-cy)-radius)
				data[y*cols+x] = byte(math.Round(math.Max(0, math.Min(255, v))))
			}
		}
		stack.Frames = append(stack.Frames, &Frame{
			Metadata: pixeldata.Metadata{
				Rows: uint16(rows), Columns: uint16(cols), SamplesPerPixel: 1,
				BitsAllocated: 8, BitsStored: 8, HighBit: 7,
				PhotometricInterpretation: "MONOCHROME2",
			},
			ByteOrder: binary.LittleEndian, PixelBytes: data, Rescale: Rescale{Slope: 1},
			ImageOrientation: []float64{1, 0, 0, 0, 1, 0}, ImagePosition: []float64{0, 0, float64(z)},
		})
	}
	return stack
}

// A straightened CPR along five control points on a curved vessel keeps its
// centre row on the vessel axis: the polyline cut the chords and strayed
// 0.54 mm inside the curve midway between control points.
func TestRenderCPRStraightenedFollowsCurvedVesselBetweenControlPoints(t *testing.T) {
	const radius = 44.0
	vol, err := BuildVolume(cprRadialDistanceStack(64, 64, 9, 8, 8, radius))
	if err != nil {
		t.Fatal(err)
	}
	var points []Vec3
	for _, p := range cprArcPoints(Vec3{X: 8, Y: 8, Z: 4}, radius, 0.05*math.Pi, 0.45*math.Pi, 5) {
		points = append(points, vol.VoxelToPatient(p))
	}
	path := NewCPRPath(points)
	scalar, err := RenderCPRScalar(context.Background(), CPRRequest{
		Mode: CPRStraightened, Volume: vol, Path: path, Width: 9, ArcSpacing: 0.25, CrossSpacing: 0.5,
	})
	if err != nil {
		t.Fatal(err)
	}
	arcs := path.ControlPointArcLengths()
	row := scalar.Height / 2
	worstInner, worstEnd := 0.0, 0.0
	for col := 0; col < scalar.Width; col++ {
		index := row*scalar.Width + col
		if !scalar.Valid[index] {
			t.Fatalf("column %d left the volume", col)
		}
		offAxis := math.Abs(scalar.Values[index]-100) / 40
		if s := float64(col) * 0.25; s > arcs[1] && s < arcs[len(arcs)-2] {
			worstInner = math.Max(worstInner, offAxis)
		} else {
			worstEnd = math.Max(worstEnd, offAxis)
		}
	}
	t.Logf("max distance from the vessel axis: interior spans %.3f mm, end spans %.3f mm", worstInner, worstEnd)
	// Interior spans: 8-bit quantization of the field (±0.0125 mm) plus
	// trilinear error. End spans leave along the end chord.
	if worstInner > 0.05 {
		t.Fatalf("interior spans stray %.3f mm from the vessel axis", worstInner)
	}
	if worstEnd > 0.4 {
		t.Fatalf("end spans stray %.3f mm from the vessel axis", worstEnd)
	}
}
