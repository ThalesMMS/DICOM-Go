package mesh

import (
	"context"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

// Windowed-sinc smoothing (#1014), after Taubin, Zhang and Golub, "Optimal
// surface smoothing as filter design" (1996), and VTK's
// vtkWindowedSincPolyDataFilter: the vertex positions are filtered by a
// low-pass polynomial of the umbrella operator W (each vertex replaced by
// the mean of its neighbours), the truncated Fourier series of the ideal
// low-pass response windowed and evaluated with the Chebyshev recurrence.
// Unlike repeated Laplacian relaxation it does not shrink the surface.
// Written from the published method; no VTK code is copied.
//
// The passes are cancellable and parallel over vertex ranges (#1253): every
// vertex of a pass depends only on the previous passes, and each vertex's
// neighbours are summed in ascending index order, so the result is the same
// for any worker count. Feature edges can be kept sharp, as with VTK's
// FeatureEdgeSmoothing and FeatureAngle (its EdgeAngle test along feature
// polylines is not implemented).

// WindowFunction tapers the truncated low-pass series.
type WindowFunction int

const (
	// WindowNuttall is the four-term Nuttall window (VTK's default).
	WindowNuttall WindowFunction = iota
	// WindowBlackman is the three-term Blackman window.
	WindowBlackman
	// WindowHanning is the raised cosine.
	WindowHanning
	// WindowHamming is the Hamming window (VTK's historical choice).
	WindowHamming
)

// DefaultFeatureAngle is the feature angle, in degrees, when
// SmoothOptions.FeatureAngle is zero.
const DefaultFeatureAngle = 45.0

// SmoothOptions configures SmoothWindowedSinc.
type SmoothOptions struct {
	// Iterations is the degree of the filter polynomial, the number of
	// umbrella-operator passes; zero uses 20.
	Iterations int
	// PassBand is the pass-band frequency k in (0, 2) of the umbrella
	// operator's spectrum (k = 1 - cos theta); smaller values smooth more.
	// Zero uses 0.1.
	PassBand float64
	Window   WindowFunction
	// FixBoundary keeps the boundary vertices in place; otherwise they are
	// smoothed along the boundary only, so an open surface keeps its rim.
	FixBoundary bool
	// PreserveFeatureEdges keeps creases sharp: an interior edge whose two
	// triangles' normals differ by more than FeatureAngle is a feature
	// edge; a vertex on exactly two feature edges is smoothed along them
	// only, and one on one or more than two (a corner) stays in place.
	PreserveFeatureEdges bool
	// FeatureAngle is the feature-edge angle in degrees, in (0, 180); zero
	// uses DefaultFeatureAngle.
	FeatureAngle float64
	// Workers bounds the goroutines of each pass; zero uses
	// min(shared budget, DefaultMaxWorkers). The result does not depend on it.
	Workers int
}

// smoothParallelMinimum is the vertex count below which a pass runs on the
// calling goroutine.
const smoothParallelMinimum = 16384

// SmoothWindowedSinc is SmoothWindowedSincContext without cancellation.
func SmoothWindowedSinc(m *Mesh, opts SmoothOptions) (*Mesh, error) {
	return SmoothWindowedSincContext(context.Background(), m, opts)
}

// SmoothWindowedSincContext returns m with its vertices low-pass filtered.
// The triangles are unchanged; vertex normals are recomputed when m has
// them. Vertices on non-manifold edges (shared by more than two triangles)
// stay in place, as do boundary vertices whose boundary is not a simple
// curve. The context is checked between the slabs of every pass, so
// cancellation takes effect within a fraction of one pass.
func SmoothWindowedSincContext(ctx context.Context, m *Mesh, opts SmoothOptions) (*Mesh, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	degree := opts.Iterations
	if degree == 0 {
		degree = 20
	}
	passBand := opts.PassBand
	if passBand == 0 {
		passBand = 0.1
	}
	if degree < 1 || !(passBand > 0 && passBand < 2) {
		return nil, fmt.Errorf("%w: smoothing degree %d, pass band %v", ErrInvalidField, degree, passBand)
	}
	featureAngle := -1.0
	if opts.PreserveFeatureEdges {
		featureAngle = opts.FeatureAngle
		if featureAngle == 0 {
			featureAngle = DefaultFeatureAngle
		}
		if !(featureAngle > 0 && featureAngle < 180) {
			return nil, fmt.Errorf("%w: feature angle %v", ErrInvalidField, opts.FeatureAngle)
		}
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = min(parallel.DefaultWorkers(), DefaultMaxWorkers)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	neighbours, start, err := smoothingAdjacency(ctx, m, opts.FixBoundary, featureAngle, workers)
	if err != nil {
		return nil, err
	}
	coefficients := sincCoefficients(degree, passBand, opts.Window)
	n := len(m.Vertices)
	pass := func(work func(lo, hi int)) error {
		if workers == 1 || n < smoothParallelMinimum {
			work(0, n)
			return ctx.Err()
		}
		return forEachSlab(ctx, n, workers, func() slabFunc {
			return func(_ context.Context, lo, hi int) error {
				work(lo, hi)
				return nil
			}
		})
	}
	previous := make([][3]float64, n)
	current := make([][3]float64, n)
	next := make([][3]float64, n)
	sum := make([][3]float64, n)
	// umbrella returns W x at vertex i: the mean of its neighbours, or x
	// itself for a fixed vertex.
	umbrella := func(x [][3]float64, i int) [3]float64 {
		list := neighbours[start[i]:start[i+1]]
		if len(list) == 0 {
			return x[i]
		}
		var mean [3]float64
		for _, j := range list {
			mean[0] += x[j][0]
			mean[1] += x[j][1]
			mean[2] += x[j][2]
		}
		scale := 1 / float64(len(list))
		return [3]float64{mean[0] * scale, mean[1] * scale, mean[2] * scale}
	}
	if err := pass(func(lo, hi int) {
		for i := lo; i < hi; i++ {
			v := m.Vertices[i]
			previous[i] = [3]float64{float64(v[0]), float64(v[1]), float64(v[2])}
			for axis := 0; axis < 3; axis++ {
				sum[i][axis] = coefficients[0] * previous[i][axis]
			}
		}
	}); err != nil {
		return nil, err
	}
	if err := pass(func(lo, hi int) {
		for i := lo; i < hi; i++ {
			current[i] = umbrella(previous, i)
			for axis := 0; axis < 3; axis++ {
				sum[i][axis] += coefficients[1] * current[i][axis]
			}
		}
	}); err != nil {
		return nil, err
	}
	// Chebyshev recurrence T(k+1) = 2 W T(k) - T(k-1).
	for k := 2; k <= degree; k++ {
		c := coefficients[k]
		if err := pass(func(lo, hi int) {
			for i := lo; i < hi; i++ {
				w := umbrella(current, i)
				for axis := 0; axis < 3; axis++ {
					next[i][axis] = 2*w[axis] - previous[i][axis]
					sum[i][axis] += c * next[i][axis]
				}
			}
		}); err != nil {
			return nil, err
		}
		previous, current, next = current, next, previous
	}
	out := &Mesh{Vertices: make([][3]float32, n), Triangles: append([][3]uint32(nil), m.Triangles...)}
	for i, p := range sum {
		out.Vertices[i] = [3]float32{float32(p[0]), float32(p[1]), float32(p[2])}
	}
	if m.Normals != nil {
		out.Normals = make([][3]float32, n)
		faceNormals(out, nil)
	}
	return out, ctx.Err()
}

// sincCoefficients are the filter's Chebyshev coefficients for the pass
// band k_pb, widened by sigma so the response is exactly 1 at k_pb: a
// low-degree series has a slow transition, and without the widening it
// already attenuates the lowest frequencies, which hold the object's size
// and position (VTK documents the same adjustment).
func sincCoefficients(degree int, passBand float64, window WindowFunction) []float64 {
	response := func(c []float64, k float64) float64 {
		// T_n(x) for x = 1 - k by the recurrence.
		x := 1 - k
		previous, current := 1.0, x
		value := c[0] + c[1]*x
		for n := 2; n < len(c); n++ {
			previous, current = current, 2*x*current-previous
			value += c[n] * current
		}
		return value
	}
	gap := func(sigma float64) float64 {
		return response(windowedSinc(degree, math.Min(passBand+sigma, 2), window), passBand) - 1
	}
	sigma := 0.0
	if degree > 1 && gap(0) < 0 {
		// The first widening that reaches unity, refined by bisection.
		const steps = 400
		low, high := 0.0, -1.0
		for i := 1; i <= steps; i++ {
			s := (2 - passBand) * float64(i) / steps
			if gap(s) >= 0 {
				high = s
				break
			}
			low = s
		}
		if high > 0 {
			for i := 0; i < 60; i++ {
				mid := (low + high) / 2
				if gap(mid) >= 0 {
					high = mid
				} else {
					low = mid
				}
			}
			sigma = high
		}
	}
	return windowedSinc(degree, math.Min(passBand+sigma, 2), window)
}

// windowedSinc are the windowed, normalised Chebyshev coefficients of the
// ideal low-pass response h(theta) = 1 for theta < theta_pb: its cosine
// series theta_pb/pi + sum 2 sin(n theta_pb)/(n pi) cos(n theta), where
// cos(n theta) = T_n(1 - k) = T_n(W), normalised so a constant passes
// unchanged.
func windowedSinc(degree int, passBand float64, window WindowFunction) []float64 {
	theta := math.Acos(1 - passBand)
	coefficients := make([]float64, degree+1)
	var total float64
	for n := range coefficients {
		c := theta / math.Pi
		if n > 0 {
			c = 2 * math.Sin(float64(n)*theta) / (float64(n) * math.Pi)
		}
		x := math.Pi * float64(n) / float64(degree+1)
		var w float64
		switch window {
		case WindowBlackman:
			w = 0.42 + 0.5*math.Cos(x) + 0.08*math.Cos(2*x)
		case WindowHanning:
			w = 0.5 + 0.5*math.Cos(x)
		case WindowHamming:
			w = 0.54 + 0.46*math.Cos(x)
		default:
			w = 0.355768 + 0.487396*math.Cos(x) + 0.144232*math.Cos(2*x) + 0.012604*math.Cos(3*x)
		}
		coefficients[n] = c * w
		total += coefficients[n]
	}
	for n := range coefficients {
		coefficients[n] /= total
	}
	return coefficients
}

// smoothingNeighbours lists, per vertex, the neighbours its umbrella
// averages (CSR: neighbours[start[i]:start[i+1]]), without feature edges;
// fixed vertices have none.
func smoothingNeighbours(m *Mesh, fixBoundary bool) (neighbours []uint32, start []int32) {
	neighbours, start, _ = smoothingAdjacency(context.Background(), m, fixBoundary, -1, 1)
	return neighbours, start
}

// meshEdge is an undirected edge (a < b) with the number of triangles on
// it.
type meshEdge struct {
	a, b uint32
	uses int32
	// feature marks an interior edge whose triangles meet at more than the
	// feature angle.
	feature bool
}

// edgeUse is one triangle side, bucketed under its lower vertex.
type edgeUse struct {
	b, tri uint32
}

// sortedEdges lists the undirected edges of m in ascending (a, b) order.
// With featureAngle > 0 (degrees) the interior edges of two triangles
// whose normals differ by more than it are marked as features. The sides
// are bucketed by their lower vertex (a counting sort), and the small
// buckets are sorted on up to workers goroutines with the context checked
// between slabs.
func sortedEdges(ctx context.Context, m *Mesh, featureAngle float64, workers int) ([]meshEdge, error) {
	n := len(m.Vertices)
	offsets := make([]int32, n+1)
	for _, t := range m.Triangles {
		for k := 0; k < 3; k++ {
			offsets[min(t[k], t[(k+1)%3])+1]++
		}
	}
	for v := 0; v < n; v++ {
		offsets[v+1] += offsets[v]
	}
	uses := make([]edgeUse, 3*len(m.Triangles))
	fill := append([]int32(nil), offsets[:n]...)
	for i, t := range m.Triangles {
		for k := 0; k < 3; k++ {
			a, b := t[k], t[(k+1)%3]
			if a > b {
				a, b = b, a
			}
			uses[fill[a]] = edgeUse{b: b, tri: uint32(i)}
			fill[a]++
		}
	}
	sortBuckets := func(lo, hi int) {
		for v := lo; v < hi; v++ {
			bucket := uses[offsets[v]:offsets[v+1]]
			// Buckets hold a handful of sides: insertion sort by (b, tri).
			for i := 1; i < len(bucket); i++ {
				for j := i; j > 0 && (bucket[j].b < bucket[j-1].b || bucket[j].b == bucket[j-1].b && bucket[j].tri < bucket[j-1].tri); j-- {
					bucket[j], bucket[j-1] = bucket[j-1], bucket[j]
				}
			}
		}
	}
	if workers <= 1 || n < smoothParallelMinimum {
		sortBuckets(0, n)
	} else if err := forEachSlab(ctx, n, workers, func() slabFunc {
		return func(_ context.Context, lo, hi int) error { sortBuckets(lo, hi); return nil }
	}); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cosine := math.Cos(featureAngle * math.Pi / 180)
	edges := make([]meshEdge, 0, len(uses)/2+1)
	for a := 0; a < n; a++ {
		bucket := uses[offsets[a]:offsets[a+1]]
		for i := 0; i < len(bucket); {
			j := i + 1
			for j < len(bucket) && bucket[j].b == bucket[i].b {
				j++
			}
			e := meshEdge{a: uint32(a), b: bucket[i].b, uses: int32(j - i)}
			if featureAngle > 0 && e.uses == 2 {
				n0, n1 := m.triangleNormal(m.Triangles[bucket[i].tri]), m.triangleNormal(m.Triangles[bucket[i+1].tri])
				if n0 != ([3]float32{}) && n1 != ([3]float32{}) {
					d := float64(n0[0])*float64(n1[0]) + float64(n0[1])*float64(n1[1]) + float64(n0[2])*float64(n1[2])
					e.feature = d < cosine
				}
			}
			edges = append(edges, e)
			i = j
		}
	}
	return edges, nil
}

// smoothingAdjacency lists, per vertex, the neighbours its umbrella
// averages (CSR: neighbours[start[i]:start[i+1]]), in ascending index
// order; fixed vertices have none. Boundary vertices average their two
// boundary neighbours, and with featureAngle > 0 interior vertices on
// feature edges their two feature neighbours; other counts fix them, as do
// non-manifold edges.
func smoothingAdjacency(ctx context.Context, m *Mesh, fixBoundary bool, featureAngle float64, workers int) (neighbours []uint32, start []int32, err error) {
	edges, err := sortedEdges(ctx, m, featureAngle, workers)
	if err != nil {
		return nil, nil, err
	}
	n := len(m.Vertices)
	boundary := make([]bool, n)
	fixed := make([]bool, n)
	var feature []bool
	if featureAngle > 0 {
		feature = make([]bool, n)
	}
	for _, e := range edges {
		switch {
		case e.uses == 1:
			boundary[e.a], boundary[e.b] = true, true
		case e.uses > 2:
			fixed[e.a], fixed[e.b] = true, true
		case e.feature:
			feature[e.a], feature[e.b] = true, true
		}
	}
	// keep reports whether vertex v averages its neighbour across e.
	keep := func(v uint32, e meshEdge) bool {
		switch {
		case fixed[v]:
			return false
		case boundary[v]:
			return e.uses == 1
		case feature != nil && feature[v]:
			return e.feature
		}
		return true
	}
	counts := make([]int32, n)
	for _, e := range edges {
		if keep(e.a, e) {
			counts[e.a]++
		}
		if keep(e.b, e) {
			counts[e.b]++
		}
	}
	// A boundary or feature vertex off a simple curve (or a fixed
	// boundary) stays in place.
	for v := range counts {
		if (boundary[v] && (fixBoundary || counts[v] != 2)) || (!boundary[v] && feature != nil && feature[v] && counts[v] != 2) {
			fixed[v] = true
			counts[v] = 0
		}
	}
	start = make([]int32, n+1)
	for v, c := range counts {
		start[v+1] = start[v] + c
	}
	neighbours = make([]uint32, start[n])
	fill := append([]int32(nil), start[:n]...)
	// Edges come in ascending (a, b) order, so each list ascends: the
	// lower neighbours (edges keyed by them) precede the higher ones.
	for _, e := range edges {
		if keep(e.a, e) {
			neighbours[fill[e.a]] = e.b
			fill[e.a]++
		}
		if keep(e.b, e) {
			neighbours[fill[e.b]] = e.a
			fill[e.b]++
		}
	}
	return neighbours, start, nil
}

// edgeUses counts the triangles on each undirected edge (lower index
// first).
func edgeUses(m *Mesh) map[[2]uint32]int {
	edges := make(map[[2]uint32]int, 3*len(m.Triangles)/2)
	for _, t := range m.Triangles {
		for i := 0; i < 3; i++ {
			a, b := t[i], t[(i+1)%3]
			edges[[2]uint32{min(a, b), max(a, b)}]++
		}
	}
	return edges
}
