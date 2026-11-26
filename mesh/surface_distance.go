package mesh

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"sort"
)

// Continuous surface-to-surface distances (#1254), for the quality control
// of smoothing and decimation and to compare segmentations: every vertex of
// one mesh is measured against the other's triangles (not its vertices), in
// both directions, after vtkDistancePolyDataFilter's per-point distances and
// the Hausdorff distance and average symmetric surface distance of
// segmentation evaluation. Written from the definitions; no third-party code
// is copied.

// SurfaceDistanceOptions configures SurfaceDistances.
type SurfaceDistanceOptions struct {
	// Signed makes the per-vertex distances negative inside the other
	// surface (see Locator.SignedDistance). The summaries are of the
	// absolute distances either way.
	Signed bool
	// Workers bounds the goroutines; zero uses min(GOMAXPROCS,
	// DefaultMaxWorkers). The result does not depend on it.
	Workers int
}

// DistanceSummary summarises the distances of one mesh's vertices to the
// other surface. Every vertex weighs a third of the area of the triangles
// using it, so the statistics estimate the surface integrals whatever the
// vertex density; a mesh without area weighs its vertices equally.
type DistanceSummary struct {
	// Mean, RMS and Max of the absolute distance, and Percentile95, the
	// distance within which 95% of the surface area lies.
	Mean, RMS, Max, Percentile95 float64
	// Vertices is the number of measured vertices.
	Vertices int
}

// SurfaceDistanceReport compares two surfaces A and B.
type SurfaceDistanceReport struct {
	// AToB holds, per vertex of A, its distance to the surface of B, and
	// BToA per vertex of B its distance to A (signed on request).
	AToB, BToA               []float64
	AToBSummary, BToASummary DistanceSummary
	// Hausdorff is the larger of the two directed maxima, the symmetric
	// Hausdorff distance sampled at the vertices.
	Hausdorff float64
	// MeanSurfaceDistance is the average symmetric surface distance: the
	// area-weighted mean of the absolute distances of both directions.
	MeanSurfaceDistance float64
}

// SurfaceDistances measures each mesh's vertices against the other's
// triangles. Distances are exact to the triangles; the maxima are sampled
// at the vertices, so they are within half an edge of the continuous
// Hausdorff distance. The context is checked every 256 vertices.
func SurfaceDistances(ctx context.Context, a, b *Mesh, opts SurfaceDistanceOptions) (*SurfaceDistanceReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for _, m := range []*Mesh{a, b} {
		if err := m.Validate(); err != nil {
			return nil, err
		}
		if len(m.Triangles) == 0 {
			return nil, fmt.Errorf("%w: surface distance of a mesh without triangles", ErrInvalidField)
		}
	}
	la, err := NewLocator(a)
	if err != nil {
		return nil, err
	}
	lb, err := NewLocator(b)
	if err != nil {
		return nil, err
	}
	report := &SurfaceDistanceReport{}
	if report.AToB, err = lb.VertexDistances(ctx, a, opts); err != nil {
		return nil, err
	}
	if report.BToA, err = la.VertexDistances(ctx, b, opts); err != nil {
		return nil, err
	}
	wa, wb := vertexAreaWeights(a), vertexAreaWeights(b)
	report.AToBSummary = summarizeDistances(report.AToB, wa)
	report.BToASummary = summarizeDistances(report.BToA, wb)
	report.Hausdorff = math.Max(report.AToBSummary.Max, report.BToASummary.Max)
	var sum, weight float64
	for i, d := range report.AToB {
		sum += wa[i] * math.Abs(d)
		weight += wa[i]
	}
	for i, d := range report.BToA {
		sum += wb[i] * math.Abs(d)
		weight += wb[i]
	}
	if weight > 0 {
		report.MeanSurfaceDistance = sum / weight
	}
	return report, nil
}

// VertexDistances returns the distance from every vertex of m to the
// located surface, signed when opts.Signed. Vertices no triangle of m uses
// are measured too.
func (l *Locator) VertexDistances(ctx context.Context, m *Mesh, opts SurfaceDistanceOptions) ([]float64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if l == nil || l.tree == nil {
		return nil, fmt.Errorf("%w: surface distance to a mesh without triangles", ErrInvalidField)
	}
	out := make([]float64, len(m.Vertices))
	workers := opts.Workers
	if workers <= 0 {
		workers = min(runtime.GOMAXPROCS(0), DefaultMaxWorkers)
	}
	err := forEachChunk(ctx, len(out), occlusionChunk, workers, func(lo, hi int) {
		stack := make([]int32, 0, 64)
		bound := math.Inf(1)
		for v := lo; v < hi; v++ {
			p := m.point(uint32(v))
			// The previous vertex's distance plus the step bounds this
			// one, which prunes most of the hierarchy at once.
			if v > lo && !math.IsInf(bound, 1) {
				step := sub3(p, m.point(uint32(v-1)))
				bound += math.Sqrt(dot3(step, step))
			}
			s, ok := l.closest(p, bound*(1+1e-12)+1e-9, &stack)
			if !ok {
				s, _ = l.closest(p, math.Inf(1), &stack)
			}
			bound = s.Distance
			if opts.Signed {
				out[v] = l.sign(p, s) * s.Distance
			} else {
				out[v] = s.Distance
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// vertexAreaWeights gives each vertex a third of the area of its
// triangles; equal weights when the mesh has no area.
func vertexAreaWeights(m *Mesh) []float64 {
	w := make([]float64, len(m.Vertices))
	var total float64
	for _, t := range m.Triangles {
		a, b, c := m.point(t[0]), m.point(t[1]), m.point(t[2])
		n := cross3(sub3(b, a), sub3(c, a))
		third := math.Sqrt(dot3(n, n)) / 6
		for _, v := range t {
			w[v] += third
		}
		total += 3 * third
	}
	if !(total > 0) {
		for i := range w {
			w[i] = 1
		}
	}
	return w
}

// summarizeDistances is the weighted summary of the absolute distances.
func summarizeDistances(distances, weights []float64) DistanceSummary {
	s := DistanceSummary{Vertices: len(distances)}
	if len(distances) == 0 {
		return s
	}
	type sample struct{ d, w float64 }
	samples := make([]sample, len(distances))
	var sum, sum2, total float64
	for i, d := range distances {
		d = math.Abs(d)
		samples[i] = sample{d, weights[i]}
		sum += weights[i] * d
		sum2 += weights[i] * d * d
		total += weights[i]
		s.Max = math.Max(s.Max, d)
	}
	if total > 0 {
		s.Mean, s.RMS = sum/total, math.Sqrt(sum2/total)
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i].d < samples[j].d })
	var acc float64
	for _, x := range samples {
		acc += x.w
		s.Percentile95 = x.d
		if acc >= 0.95*total {
			break
		}
	}
	return s
}
