package mesh

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"
)

func qualityFixture(tris ...[3][3]float32) *Mesh {
	m := &Mesh{}
	for _, t := range tris {
		base := uint32(len(m.Vertices))
		m.Vertices = append(m.Vertices, t[:]...)
		m.Triangles = append(m.Triangles, [3]uint32{base, base + 1, base + 2})
	}
	return m
}
func qualityCheck(t *testing.T, m *Mesh, o QualityOptions) *QualityReport {
	t.Helper()
	r, e := AnalyzeQuality(context.Background(), m, o)
	if e != nil || !r.Complete {
		t.Fatalf("report=%+v error=%v", r, e)
	}
	return r
}
func TestQualityAnalyticMetricsAndSourcePreservation(t *testing.T) {
	m := qualityFixture([3][3]float32{{0, 0, 0}, {3, 0, 0}, {0, 4, 0}})
	m.Normals = [][3]float32{{0, 0, 1}, {0, 0, 1}, {0, 0, 1}}
	before := &Mesh{Vertices: append([][3]float32(nil), m.Vertices...), Normals: append([][3]float32(nil), m.Normals...), Triangles: append([][3]uint32(nil), m.Triangles...)}
	r := qualityCheck(t, m, QualityOptions{MinAreaMM2: 7, MinAngleDegrees: 40, MaxAspectRatio: 1.5})
	if r.AreaMM2.Mean != 6 || math.Abs(r.MinAngleDegrees.Mean-36.86989764584402) > 1e-10 || math.Abs(r.AspectRatio.Mean-math.Sqrt(3)*25/24) > 1e-12 {
		t.Fatalf("analytic 3-4-5 metrics: %+v", r)
	}
	if r.SmallArea.Count != 1 || r.SmallAngle.Count != 1 || r.HighAspect.Count != 1 || !reflect.DeepEqual(before, m) {
		t.Fatalf("classification/mutation: %+v", r)
	}
}
func TestQualityDegenerateAndOppositeWindingDuplicates(t *testing.T) {
	m := &Mesh{Vertices: [][3]float32{{0, 0, 0}, {1, 0, 0}, {2, 0, 0}, {0, 1, 0}}, Triangles: [][3]uint32{{0, 1, 2}, {0, 1, 3}, {3, 1, 0}, {0, 0, 3}}}
	r := qualityCheck(t, m, QualityOptions{})
	if r.Degenerate.Count != 2 || r.DuplicateFaces.Count != 1 || r.IntersectionsConclusive || r.State != "inconclusive" || r.AreaMM2.Samples != 4 || r.AspectRatio.Samples != 2 {
		t.Fatalf("degenerate/duplicate scope: %+v", r)
	}
	if len(r.Pairs) != 1 || r.Pairs[0].Kind != QualityDuplicate {
		t.Fatalf("opposite winding duplicate: %+v", r.Pairs)
	}
}
func TestQualityIntersectionCategories(t *testing.T) {
	a := [3][3]float32{{0, 0, 0}, {4, 0, 0}, {0, 4, 0}}
	cases := []struct {
		name string
		b    [3][3]float32
		kind QualityPairKind
	}{
		{"crossing", [3][3]float32{{1, 1, -1}, {1, 1, 1}, {3, 1, 0}}, QualityCrossing},
		{"coplanar-overlap", [3][3]float32{{1, 1, 0}, {2, 1, 0}, {1, 2, 0}}, QualityCoplanarOverlap},
		{"coplanar-edge-contact", [3][3]float32{{0, 0, 0}, {4, 0, 0}, {0, -4, 0}}, QualityContact},
		{"noncoplanar-point-contact", [3][3]float32{{1, 1, 0}, {1, 1, 2}, {2, 1, 2}}, QualityContact},
		{"disjoint", [3][3]float32{{8, 8, 0}, {9, 8, 0}, {8, 9, 0}}, ""},
		{"overlapping-boxes-coplanar-disjoint", [3][3]float32{{4, 4, 0}, {4, 1.5, 0}, {1.5, 4, 0}}, ""},
		{"tolerance-plane-uncertainty", [3][3]float32{{1, 1, 0.001}, {2, 1, 0.001}, {1, 2, 0.001}}, QualityUncertain},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := qualityCheck(t, qualityFixture(a, c.b), QualityOptions{DistanceToleranceMM: 0.01})
			if c.kind == "" {
				if len(r.Pairs) != 0 {
					t.Fatal(r.Pairs)
				}
			} else if len(r.Pairs) != 1 || r.Pairs[0].Kind != c.kind {
				t.Fatalf("want %s got %+v", c.kind, r.Pairs)
			}
			if c.kind == QualityUncertain && r.IntersectionsConclusive {
				t.Fatal("uncertain reported conclusive")
			}
		})
	}
}
func TestQualityAdjacentAndNestedShells(t *testing.T) {
	m := &Mesh{Vertices: [][3]float32{{0, 0, 0}, {1, 0, 0}, {0, 1, 0}, {1, 1, 0}}, Triangles: [][3]uint32{{0, 1, 2}, {1, 3, 2}}}
	r := qualityCheck(t, m, QualityOptions{})
	if r.AdjacentPairsSkipped != 1 || len(r.Pairs) != 0 {
		t.Fatalf("legitimate adjacency %+v", r)
	}
	m = &Mesh{Vertices: [][3]float32{{0, 0, 0}, {10, 0, 0}, {0, 10, 0}, {0, 0, 10}, {1, 1, 1}, {2, 1, 1}, {1, 2, 1}, {1, 1, 2}}}
	for _, base := range []uint32{0, 4} {
		for _, tri := range [][3]uint32{{0, 2, 1}, {0, 1, 3}, {0, 3, 2}, {1, 2, 3}} {
			m.Triangles = append(m.Triangles, [3]uint32{tri[0] + base, tri[1] + base, tri[2] + base})
		}
	}
	r = qualityCheck(t, m, QualityOptions{})
	if len(r.Pairs) != 0 || !r.IntersectionsConclusive {
		t.Fatalf("nested disjoint shells %+v", r)
	}
}
func TestQualityPhysicalScale(t *testing.T) {
	a := [3][3]float32{{0, 0, 0}, {4, 0, 0}, {0, 4, 0}}
	b := [3][3]float32{{1, 1, 0.001}, {2, 1, 0.001}, {1, 2, 0.001}}
	m := qualityFixture(a, b)
	r := qualityCheck(t, m, QualityOptions{DistanceToleranceMM: 0.01, MinAreaMM2: 1, MinAngleDegrees: 40, MaxAspectRatio: 2})
	for i := range m.Vertices {
		for j := range m.Vertices[i] {
			m.Vertices[i][j] *= 100
		}
	}
	s := qualityCheck(t, m, QualityOptions{DistanceToleranceMM: 1, MinAreaMM2: 10000, MinAngleDegrees: 40, MaxAspectRatio: 2})
	if r.SmallArea.Count != s.SmallArea.Count || r.SmallAngle.Count != s.SmallAngle.Count || r.HighAspect.Count != s.HighAspect.Count || r.UncertainPairs != s.UncertainPairs || math.Abs(s.AreaMM2.Mean/r.AreaMM2.Mean-10000) > 1e-9 {
		t.Fatalf("physical scaling %v %v", r, s)
	}
}
func TestQualityInvalidBeforeGeometry(t *testing.T) {
	cases := []*Mesh{{Vertices: [][3]float32{{float32(math.NaN()), 0, 0}}, Triangles: [][3]uint32{{0, 1, 2}}}, {Vertices: [][3]float32{{0, 0, 0}}, Triangles: [][3]uint32{{0, 0, 4}}}, {Vertices: [][3]float32{{0, 0, 0}}, Normals: [][3]float32{{0, float32(math.Inf(1)), 0}}}}
	for _, m := range cases {
		r, e := AnalyzeQuality(context.Background(), m, QualityOptions{})
		if !errors.Is(e, ErrInvalidField) || r.Complete || r.IntersectionsConclusive || r.TrianglesChecked != 0 || r.Candidates != 0 {
			t.Fatalf("invalid geometry analyzed %+v %v", r, e)
		}
	}
	for _, o := range []QualityOptions{{MaxBytes: -1}, {DistanceToleranceMM: math.NaN()}, {MaxAspectRatio: 0.5}, {MinAngleDegrees: 61}} {
		if _, e := AnalyzeQuality(context.Background(), &Mesh{}, o); !errors.Is(e, ErrInvalidField) {
			t.Fatal(e)
		}
	}
}
func TestQualityLimitsAndBoundedResults(t *testing.T) {
	a := [3][3]float32{{0, 0, 0}, {4, 0, 0}, {0, 4, 0}}
	m := qualityFixture(a, a, a)
	r := qualityCheck(t, m, QualityOptions{MaxResults: 1})
	if r.CoplanarOverlaps != 3 || len(r.Pairs) != 1 || !r.ResultsTruncated {
		t.Fatalf("bounded complete counts %+v", r)
	}
	for _, o := range []QualityOptions{{MaxBytes: 1}, {MaxVertices: 1}, {MaxTriangles: 1}, {MaxWork: 1}, {MaxCandidates: 1}} {
		r, e := AnalyzeQuality(context.Background(), m, o)
		if !errors.Is(e, ErrBudgetExceeded) || r.Complete || r.IntersectionsConclusive || r.State != "limited" {
			t.Fatalf("quota %v: %+v %v", o, r, e)
		}
	}
}

type qualityCancelContext struct{ calls, limit int }

func (c *qualityCancelContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *qualityCancelContext) Done() <-chan struct{}       { return nil }
func (c *qualityCancelContext) Value(any) any               { return nil }
func (c *qualityCancelContext) Err() error {
	c.calls++
	if c.calls >= c.limit {
		return context.Canceled
	}
	return nil
}
func TestQualityCancellationDuringSearch(t *testing.T) {
	a := [3][3]float32{{0, 0, 0}, {4, 0, 0}, {0, 4, 0}}
	m := qualityFixture(a, a, a, a)
	full := qualityCheck(t, m, QualityOptions{})
	ctx := &qualityCancelContext{limit: int(full.Work) - 2}
	r, e := AnalyzeQuality(ctx, m, QualityOptions{})
	if !errors.Is(e, context.Canceled) || r.Complete || r.IntersectionsConclusive || r.State != "canceled" || r.TrianglesChecked != 4 || r.Candidates == 0 {
		t.Fatalf("mid-search cancellation %+v %v", r, e)
	}
}

func TestQualityNearParallelIsUncertain(t *testing.T) {
	h := float32(1e-15)
	a := [3][3]float32{{0, 0, 0}, {4, 0, 0}, {0, 4, 0}}
	b := [3][3]float32{{1, 1, -h}, {2, 1, h}, {1, 2, h}}
	// Noncoplanar, with an interior z=0 intersection from (1.5,1) to
	// (1,1.5), i.e. x+y=2.5. A roundoff band cannot certify coplanarity.
	r := qualityCheck(t, qualityFixture(a, b), QualityOptions{})
	if r.CoplanarOverlaps != 0 || r.UncertainPairs != 1 || r.IntersectionsConclusive || r.State != "inconclusive" {
		t.Fatalf("tilted analytic planes incorrectly certified: %+v", r)
	}
	// A genuinely coplanar non-axis-aligned plane x+y+z=4 must remain
	// distinguishable from the tiny tilt above.
	a = [3][3]float32{{4, 0, 0}, {0, 4, 0}, {0, 0, 4}}
	b = [3][3]float32{{2, 1, 1}, {1, 2, 1}, {1, 1, 2}}
	r = qualityCheck(t, qualityFixture(a, b), QualityOptions{})
	if r.CoplanarOverlaps != 1 || !r.IntersectionsConclusive {
		t.Fatalf("exact oblique coplanarity: %+v", r)
	}
}
