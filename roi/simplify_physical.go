package roi

import (
	"context"
	"errors"
	"math"
)

// Point3D is a physical point in millimetres in a caller-selected common frame.
// The caller supplies frame identity; this type does not infer DICOM geometry.
type Point3D struct{ X, Y, Z float64 }

// PolylineTopology selects the intersection contract for one independent line.
type PolylineTopology uint8

const (
	// PolylineRequireSimple refuses original or introduced segment intersections.
	// It does not guarantee knot type, ambient isotopy or minimum clearance.
	PolylineRequireSimple PolylineTopology = iota
	// PolylineIgnoreIntersections explicitly skips intersection qualification.
	PolylineIgnoreIntersections
)

var (
	ErrPolylineInvalid  = errors.New("roi: invalid physical polyline or options")
	ErrPolylineLimit    = errors.New("roi: physical polyline resource limit exceeded")
	ErrPolylineTopology = errors.New("roi: physical polyline intersects or overlaps itself")
)

const (
	polylineRange               = 1e9
	polylineFixedBytes    int64 = 256 * 1024
	polylineBytesPerPoint int64 = 256
)

// PolylineLimits bounds point-sized allocations and charged work. Zero fields
// select finite defaults. The byte estimate includes output and exact-predicate
// scratch; it is a structural admission estimate, not an allocator or RSS cap.
type PolylineLimits struct {
	MaxPoints int
	MaxWork   int64
	MaxBytes  int64
}

func DefaultPolylineLimits() PolylineLimits {
	return PolylineLimits{MaxPoints: 4096, MaxWork: 32_000_000, MaxBytes: 8 * 1024 * 1024}
}

// PolylineSimplifyOptions applies physical error and protected original indices.
// ToleranceMM is finite in [0,1e9]; zero only removes exactly collinear samples.
// Closed is semantic closure. An exact repeated terminal point is preserved.
type PolylineSimplifyOptions struct {
	Closed           bool
	ToleranceMM      float64
	ProtectedIndices []int
	Topology         PolylineTopology
	Limits           PolylineLimits
}

// PolylineSimplifyResult owns its points and ordered original indices. Retained
// points are original float64 coordinates. MaxDeviationMM and lengths are
// float64 diagnostics; admission of each chord uses exact rational arithmetic
// on the input float64 numbers. The bound applies to this piecewise-linear path,
// not a later spline, rasterization, measurement or CPR interpretation.
type PolylineSimplifyResult struct {
	Points                                        []Point3D
	OriginalIndices                               []int
	Closed, ExplicitClosure                       bool
	Topology                                      PolylineTopology
	InputLengthMM, OutputLengthMM, MaxDeviationMM float64
	RemovedPoints                                 int
	Reduction                                     float64
	Work, EstimatedBytes                          int64
}

// PolylinePixelPlane maps subpixel coordinates explicitly into physical space:
// OriginMM + x*ColumnStepMM + y*RowStepMM. The independent step vectors include
// spacing; they need not be unit length or orthogonal. No spacing is guessed.
type PolylinePixelPlane struct{ OriginMM, ColumnStepMM, RowStepMM Point3D }

// PixelPolylineSimplifyResult owns original subpixel points and physical output.
type PixelPolylineSimplifyResult struct {
	Points   []Point2D
	Physical PolylineSimplifyResult
}

type polylineRun struct {
	ctx    context.Context
	limits PolylineLimits
	work   int64
}

func (r *polylineRun) step() error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	if r.work >= r.limits.MaxWork {
		return ErrPolylineLimit
	}
	r.work++
	return nil
}

func newPolylineRun(ctx context.Context, n int, o PolylineSimplifyOptions) (*polylineRun, int64, error) {
	if ctx == nil || !polylineScalar(o.ToleranceMM) || o.ToleranceMM < 0 || o.Topology > PolylineIgnoreIntersections {
		return nil, 0, ErrPolylineInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	l := o.Limits
	if l.MaxPoints < 0 || l.MaxWork < 0 || l.MaxBytes < 0 || l.MaxPoints > 100_000 || l.MaxWork > 1_000_000_000 || l.MaxBytes > 256*1024*1024 {
		return nil, 0, ErrPolylineInvalid
	}
	d := DefaultPolylineLimits()
	if l.MaxPoints == 0 {
		l.MaxPoints = d.MaxPoints
	}
	if l.MaxWork == 0 {
		l.MaxWork = d.MaxWork
	}
	if l.MaxBytes == 0 {
		l.MaxBytes = d.MaxBytes
	}
	if n > l.MaxPoints || len(o.ProtectedIndices) > l.MaxPoints {
		return nil, 0, ErrPolylineLimit
	}
	estimate := polylineFixedBytes + polylineBytesPerPoint*int64(n)
	if estimate > l.MaxBytes {
		return nil, 0, ErrPolylineLimit
	}
	return &polylineRun{ctx: ctx, limits: l}, estimate, nil
}

func polylineScalar(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && math.Abs(v) <= polylineRange
}
func polylinePoint(p Point3D) bool {
	return polylineScalar(p.X) && polylineScalar(p.Y) && polylineScalar(p.Z)
}
func polylinePointDistance(a, b Point3D) float64 {
	return math.Hypot(math.Hypot(a.X-b.X, a.Y-b.Y), a.Z-b.Z)
}

// SimplifyPhysicalPolyline simplifies one open or closed physical line with
// protected original samples. It returns a zero result on every refusal or
// cancellation, never a partially qualified candidate. Inputs remain unchanged.
func SimplifyPhysicalPolyline(ctx context.Context, points []Point3D, o PolylineSimplifyOptions) (PolylineSimplifyResult, error) {
	r, estimate, err := newPolylineRun(ctx, len(points), o)
	if err != nil {
		return PolylineSimplifyResult{}, err
	}
	return r.simplify(points, o, estimate)
}

// SimplifyPixelPolyline performs the supplied pixel-to-physical transform before
// simplification. It preserves retained original pixel coordinates, not a
// rounded or inverse-transformed approximation. Physical range is [−1e9,1e9]
// mm per coordinate; pixel coordinates and plane fields use the same finite
// absolute range in their respective units. Overflow is refused, never clamped.
func SimplifyPixelPolyline(ctx context.Context, points []Point2D, plane PolylinePixelPlane, o PolylineSimplifyOptions) (PixelPolylineSimplifyResult, error) {
	r, estimate, err := newPolylineRun(ctx, len(points), o)
	if err != nil {
		return PixelPolylineSimplifyResult{}, err
	}
	if !polylinePoint(plane.OriginMM) || !polylinePoint(plane.ColumnStepMM) || !polylinePoint(plane.RowStepMM) || !polylineIndependent(plane.ColumnStepMM, plane.RowStepMM) {
		return PixelPolylineSimplifyResult{}, ErrPolylineInvalid
	}
	physical := make([]Point3D, len(points))
	for i, p := range points {
		if err = r.step(); err != nil {
			return PixelPolylineSimplifyResult{}, err
		}
		if !polylineScalar(p.X) || !polylineScalar(p.Y) {
			return PixelPolylineSimplifyResult{}, ErrPolylineInvalid
		}
		v := Point3D{plane.OriginMM.X + p.X*plane.ColumnStepMM.X + p.Y*plane.RowStepMM.X, plane.OriginMM.Y + p.X*plane.ColumnStepMM.Y + p.Y*plane.RowStepMM.Y, plane.OriginMM.Z + p.X*plane.ColumnStepMM.Z + p.Y*plane.RowStepMM.Z}
		if !polylinePoint(v) {
			return PixelPolylineSimplifyResult{}, ErrPolylineInvalid
		}
		physical[i] = v
	}
	result, err := r.simplify(physical, o, estimate)
	if err != nil {
		return PixelPolylineSimplifyResult{}, err
	}
	pixels := make([]Point2D, len(result.OriginalIndices))
	for i, index := range result.OriginalIndices {
		if err = r.step(); err != nil {
			return PixelPolylineSimplifyResult{}, err
		}
		pixels[i] = points[index]
	}
	result.Work = r.work
	return PixelPolylineSimplifyResult{Points: pixels, Physical: result}, nil
}

func (r *polylineRun) simplify(points []Point3D, o PolylineSimplifyOptions, estimate int64) (PolylineSimplifyResult, error) {
	n := len(points)
	for _, p := range points {
		if err := r.step(); err != nil {
			return PolylineSimplifyResult{}, err
		}
		if !polylinePoint(p) {
			return PolylineSimplifyResult{}, ErrPolylineInvalid
		}
	}
	keep := make([]bool, n)
	for _, index := range o.ProtectedIndices {
		if err := r.step(); err != nil {
			return PolylineSimplifyResult{}, err
		}
		if index < 0 || index >= n {
			return PolylineSimplifyResult{}, ErrPolylineInvalid
		}
		keep[index] = true
	}
	explicit := o.Closed && n > 1 && points[0] == points[n-1]
	canonical := n
	if explicit {
		canonical--
		keep[n-1] = true
	}
	if canonical > 0 {
		keep[0] = true
	}
	if !o.Closed && n > 0 {
		keep[n-1] = true
	}
	if o.Closed {
		if canonical < 3 {
			return PolylineSimplifyResult{}, ErrPolylineInvalid
		}
		second, third, err := r.closedAnchors(points[:canonical])
		if err != nil {
			return PolylineSimplifyResult{}, err
		}
		keep[second], keep[third] = true, true
	}
	if o.Topology == PolylineRequireSimple {
		if err := r.simple(points, o.Closed); err != nil {
			return PolylineSimplifyResult{}, err
		}
	}
	// Protected partitions are in original order; a closed final partition uses a
	// virtual endpoint canonical, whose coordinate is the original index zero.
	type span struct{ a, b int }
	stack := make([]span, 0, canonical+1)
	previous := 0
	for i := 1; i < canonical; i++ {
		if err := r.step(); err != nil {
			return PolylineSimplifyResult{}, err
		}
		if keep[i] {
			stack = append(stack, span{previous, i})
			previous = i
		}
	}
	end := canonical - 1
	if o.Closed {
		end = canonical
	}
	if end > previous {
		stack = append(stack, span{previous, end})
	}
	at := func(i int) Point3D {
		if i == canonical {
			return points[0]
		}
		return points[i]
	}
	for len(stack) > 0 {
		if err := r.step(); err != nil {
			return PolylineSimplifyResult{}, err
		}
		s := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if s.b <= s.a+1 {
			continue
		}
		a, b := at(s.a), at(s.b)
		split := -1
		maxDistance := -1.0
		firstOutside := -1
		for i := s.a + 1; i < s.b; i++ {
			if err := r.step(); err != nil {
				return PolylineSimplifyResult{}, err
			}
			p := at(i)
			distance := polylineSegmentDistance(p, a, b)
			if distance > maxDistance {
				maxDistance = distance
				split = i
			}
			if !polylineWithin(p, a, b, o.ToleranceMM) && firstOutside < 0 {
				firstOutside = i
			}
		}
		if firstOutside < 0 {
			continue
		}
		// Underflow/rounding can underestimate the float64 split heuristic. An exact
		// rejecting point still guarantees progress without weakening admission.
		if err := r.step(); err != nil {
			return PolylineSimplifyResult{}, err
		}
		if split < 0 || polylineWithin(at(split), a, b, o.ToleranceMM) {
			split = firstOutside
		}
		keep[split] = true
		stack = append(stack, span{s.a, split}, span{split, s.b})
	}
	out := PolylineSimplifyResult{Closed: o.Closed, ExplicitClosure: explicit, Topology: o.Topology, EstimatedBytes: estimate}
	out.Points = make([]Point3D, 0, n)
	out.OriginalIndices = make([]int, 0, n)
	for i, p := range points {
		if err := r.step(); err != nil {
			return PolylineSimplifyResult{}, err
		}
		if keep[i] {
			out.Points = append(out.Points, p)
			out.OriginalIndices = append(out.OriginalIndices, i)
		}
	}
	if o.Topology == PolylineRequireSimple {
		if err := r.simple(out.Points, o.Closed); err != nil {
			return PolylineSimplifyResult{}, err
		}
	}
	var err error
	out.InputLengthMM, err = r.length(points, o.Closed)
	if err != nil {
		return PolylineSimplifyResult{}, err
	}
	out.OutputLengthMM, err = r.length(out.Points, o.Closed)
	if err != nil {
		return PolylineSimplifyResult{}, err
	}
	out.MaxDeviationMM, err = r.deviation(points, out.OriginalIndices, canonical, o.Closed)
	if err != nil {
		return PolylineSimplifyResult{}, err
	}
	out.RemovedPoints = n - len(out.Points)
	if n > 0 {
		out.Reduction = float64(out.RemovedPoints) / float64(n)
	}
	out.Work = r.work
	return out, nil
}

func (r *polylineRun) closedAnchors(points []Point3D) (int, int, error) {
	second := -1
	distance := -1.0
	for i := 1; i < len(points); i++ {
		if err := r.step(); err != nil {
			return 0, 0, err
		}
		if points[i] != points[0] {
			d := polylinePointDistance(points[i], points[0])
			if d > distance {
				distance = d
				second = i
			}
		}
	}
	if second < 0 {
		return 0, 0, ErrPolylineInvalid
	}
	third := -1
	distance = -1
	for i := 1; i < len(points); i++ {
		if err := r.step(); err != nil {
			return 0, 0, err
		}
		if points[i] != points[0] && points[i] != points[second] {
			d := polylineSegmentDistance(points[i], points[0], points[second])
			if d > distance {
				distance = d
				third = i
			}
		}
	}
	if third < 0 {
		return 0, 0, ErrPolylineInvalid
	}
	return second, third, nil
}

func polylineSegmentDistance(p, a, b Point3D) float64 {
	d := Point3D{b.X - a.X, b.Y - a.Y, b.Z - a.Z}
	length := math.Hypot(math.Hypot(d.X, d.Y), d.Z)
	if length == 0 {
		return polylinePointDistance(p, a)
	}
	ux, uy, uz := d.X/length, d.Y/length, d.Z/length
	t := (p.X-a.X)*ux + (p.Y-a.Y)*uy + (p.Z-a.Z)*uz
	if t <= 0 {
		return polylinePointDistance(p, a)
	}
	if t >= length {
		return polylinePointDistance(p, b)
	}
	return math.Hypot(math.Hypot((p.X-a.X)-t*ux, (p.Y-a.Y)-t*uy), (p.Z-a.Z)-t*uz)
}

func (r *polylineRun) length(points []Point3D, closed bool) (float64, error) {
	total := 0.0
	for i := 1; i < len(points); i++ {
		if err := r.step(); err != nil {
			return 0, err
		}
		total += polylinePointDistance(points[i-1], points[i])
	}
	if closed && len(points) > 1 {
		if err := r.step(); err != nil {
			return 0, err
		}
		total += polylinePointDistance(points[len(points)-1], points[0])
	}
	return total, nil
}

func (r *polylineRun) deviation(points []Point3D, indices []int, canonical int, closed bool) (float64, error) {
	maxDeviation := 0.0
	for j := 1; j < len(indices); j++ {
		a, b := indices[j-1], indices[j]
		for i := a + 1; i < b; i++ {
			if err := r.step(); err != nil {
				return 0, err
			}
			maxDeviation = math.Max(maxDeviation, polylineExactSegmentDistance(points[i], points[a], points[b]))
		}
	}
	if closed && len(indices) > 0 && indices[len(indices)-1] < canonical {
		a := indices[len(indices)-1]
		for i := a + 1; i < canonical; i++ {
			if err := r.step(); err != nil {
				return 0, err
			}
			maxDeviation = math.Max(maxDeviation, polylineExactSegmentDistance(points[i], points[a], points[0]))
		}
	}
	return maxDeviation, nil
}

// simple uses a linear edge list, coalescing consecutive coincident samples
// only for intersection checks. There is no near-point epsilon merge.
func (r *polylineRun) simple(points []Point3D, closed bool) error {
	type edge struct{ a, b Point3D }
	edges := make([]edge, 0, len(points))
	for i := 1; i < len(points); i++ {
		if err := r.step(); err != nil {
			return err
		}
		if points[i-1] != points[i] {
			edges = append(edges, edge{points[i-1], points[i]})
		}
	}
	if closed && len(points) > 1 && points[len(points)-1] != points[0] {
		edges = append(edges, edge{points[len(points)-1], points[0]})
	}
	for i := range edges {
		for j := i + 1; j < len(edges); j++ {
			if err := r.step(); err != nil {
				return err
			}
			a, b := edges[i], edges[j]
			adjacent := j == i+1 || (closed && i == 0 && j == len(edges)-1)
			if adjacent {
				var outsideA, joint, outsideB Point3D
				if j == i+1 {
					outsideA, joint, outsideB = a.a, a.b, b.b
				} else {
					outsideA, joint, outsideB = a.b, a.a, b.a
				}
				if polylineWithin(outsideA, joint, outsideB, 0) || polylineWithin(outsideB, outsideA, joint, 0) {
					return ErrPolylineTopology
				}
			} else if polylineSegmentsIntersect(a.a, a.b, b.a, b.b) {
				return ErrPolylineTopology
			}
		}
	}
	return nil
}
