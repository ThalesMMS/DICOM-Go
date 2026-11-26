package mesh

import (
	"context"
	"fmt"
	"math"
	"unsafe"
)

// QualityOptions describes a nondestructive analysis in patient millimetres.
// Zero limits select finite defaults. Negative limits/options are invalid.
// Result indices are bounded independently of complete counts.
type QualityOptions struct {
	DistanceToleranceMM, MinAreaMM2, MinAngleDegrees, MaxAspectRatio        float64
	MaxVertices, MaxTriangles, MaxBytes, MaxWork, MaxCandidates, MaxResults int64
}

// QualityStatistics excludes undefined angles/aspect ratios of zero-area faces.
type QualityStatistics struct {
	Samples        int64
	Min, Max, Mean float64
}

// QualityFinding counts all findings, retaining at most MaxResults indices.
type QualityFinding struct {
	Count   int64
	Indices []uint32
}

// QualityPair identifies two nonadjacent faces, or an indexed duplicate.
type QualityPair struct {
	First, Second uint32
	Kind          QualityPairKind
}
type QualityPairKind string

const (
	QualityCrossing        QualityPairKind = "crossing"
	QualityCoplanarOverlap QualityPairKind = "coplanar-overlap"
	QualityContact         QualityPairKind = "contact"
	QualityUncertain       QualityPairKind = "uncertain"
	QualityDuplicate       QualityPairKind = "duplicate-face"
)

// QualityReport.Complete means the stated scope was scanned, not that the mesh
// is suitable for clinical use. Degenerate faces and uncertain pairs make
// intersection conclusions inconclusive even when Complete is true.
type QualityReport struct {
	Options                                                                                          QualityOptions
	Complete, ResultsTruncated, IntersectionsConclusive                                              bool
	State                                                                                            string
	VerticesChecked, TrianglesChecked, Work, Candidates, AdjacentPairsSkipped                        int64
	ReservedBytes                                                                                    int64
	AreaMM2, MinAngleDegrees, AspectRatio                                                            QualityStatistics
	InvalidVertices, InvalidTriangles, Degenerate, SmallArea, SmallAngle, HighAspect, DuplicateFaces QualityFinding
	Crossings, CoplanarOverlaps, Contacts, UncertainPairs                                            int64
	Pairs                                                                                            []QualityPair
}

// QualityLimitError distinguishes partial work from a successful empty report.
type QualityLimitError struct {
	Resource    string
	Used, Limit int64
}

func (e *QualityLimitError) Error() string {
	return fmt.Sprintf("mesh quality: %s limit (%d > %d)", e.Resource, e.Used, e.Limit)
}
func (e *QualityLimitError) Unwrap() error { return ErrBudgetExceeded }

// Exact coplanarity uses bounded <=840-bit integers. Reserve conservative
// logical scratch payload (limbs and headers); allocator/runtime overhead is
// excluded consistently with the array quota.
const qualityExactScratchBytes int64 = 16 << 10

type qualityBox struct {
	Lo, Hi     [3]float64
	ID         uint32
	Degenerate bool
}
type qualityKey struct {
	V  [3]uint32
	ID uint32
}
type qualityRun struct {
	ctx context.Context
	r   *QualityReport
}

func (q *qualityRun) step() error {
	if err := q.ctx.Err(); err != nil {
		q.r.State = "canceled"
		return err
	}
	if q.r.Work >= q.r.Options.MaxWork {
		q.r.State = "limited"
		return &QualityLimitError{"work", q.r.Work + 1, q.r.Options.MaxWork}
	}
	q.r.Work++
	return nil
}
func qualityOptions(o QualityOptions) (QualityOptions, error) {
	vals := []float64{o.DistanceToleranceMM, o.MinAreaMM2, o.MinAngleDegrees, o.MaxAspectRatio}
	for _, v := range vals {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return o, fmt.Errorf("%w: invalid quality tolerance", ErrInvalidField)
		}
	}
	if o.MinAngleDegrees > 60 || (o.MaxAspectRatio > 0 && o.MaxAspectRatio < 1) {
		return o, fmt.Errorf("%w: quality angle/aspect bound", ErrInvalidField)
	}
	limits := []*int64{&o.MaxVertices, &o.MaxTriangles, &o.MaxBytes, &o.MaxWork, &o.MaxCandidates, &o.MaxResults}
	defaults := []int64{10_000_000, 10_000_000, 256 << 20, 20_000_000, 1_000_000, 1024}
	for i, p := range limits {
		if *p < 0 {
			return o, fmt.Errorf("%w: negative quality limit", ErrInvalidField)
		}
		if *p == 0 {
			*p = defaults[i]
		}
	}
	return o, nil
}
func (r *QualityReport) finding(f *QualityFinding, id uint32) {
	f.Count++
	if int64(len(f.Indices)) < r.Options.MaxResults {
		f.Indices = append(f.Indices, id)
	} else {
		r.ResultsTruncated = true
	}
}
func (r *QualityReport) pair(a, b uint32, k QualityPairKind) {
	if a > b {
		a, b = b, a
	}
	switch k {
	case QualityCrossing:
		r.Crossings++
	case QualityCoplanarOverlap:
		r.CoplanarOverlaps++
	case QualityContact:
		r.Contacts++
	case QualityUncertain:
		r.UncertainPairs++
		r.IntersectionsConclusive = false
	}
	if int64(len(r.Pairs)) < r.Options.MaxResults {
		r.Pairs = append(r.Pairs, QualityPair{a, b, k})
	} else {
		r.ResultsTruncated = true
	}
}
func qualityStat(s *QualityStatistics, x float64) {
	s.Samples++
	if s.Samples == 1 {
		s.Min = x
		s.Max = x
	}
	s.Min = math.Min(s.Min, x)
	s.Max = math.Max(s.Max, x)
	s.Mean += (x - s.Mean) / float64(s.Samples)
}
func qualityPoint(m *Mesh, id uint32) [3]float64 {
	v := m.Vertices[id]
	return [3]float64{float64(v[0]), float64(v[1]), float64(v[2])}
}
func qualityTriangle(m *Mesh, id uint32) [3][3]float64 {
	t := m.Triangles[id]
	return [3][3]float64{qualityPoint(m, t[0]), qualityPoint(m, t[1]), qualityPoint(m, t[2])}
}

// AnalyzeQuality checks indexed duplicates (all windings), triangle metrics,
// and intersections of nonadjacent, nondegenerate triangles. Shared-index
// adjacency is excluded from intersection testing; containment/nested shells
// and closure are outside this geometric intersection scope. It never repairs
// or mutates the source. The returned partial report is meaningful on error.
func AnalyzeQuality(ctx context.Context, m *Mesh, o QualityOptions) (*QualityReport, error) {
	o, err := qualityOptions(o)
	r := &QualityReport{Options: o, State: "invalid", IntersectionsConclusive: true}
	defer func() {
		if !r.Complete {
			r.IntersectionsConclusive = false
		}
	}()
	if err != nil {
		return r, err
	}
	if ctx == nil || m == nil {
		return r, fmt.Errorf("%w: nil quality context/mesh", ErrInvalidField)
	}
	q := qualityRun{ctx, r}
	if err = ctx.Err(); err != nil {
		r.State = "canceled"
		return r, err
	}
	if m.Normals != nil && len(m.Normals) != len(m.Vertices) {
		return r, fmt.Errorf("%w: normal count", ErrInvalidField)
	}
	n, nt := int64(len(m.Vertices)), int64(len(m.Triangles))
	for _, v := range []struct {
		name        string
		used, limit int64
	}{{"vertices", n, o.MaxVertices}, {"triangles", nt, o.MaxTriangles}} {
		if v.used > v.limit {
			r.State = "limited"
			return r, &QualityLimitError{v.name, v.used, v.limit}
		}
	}
	if uint64(len(m.Vertices)) > uint64(math.MaxUint32) || uint64(len(m.Triangles)) > uint64(math.MaxUint32) {
		return r, fmt.Errorf("%w: quality index range", ErrInvalidField)
	}
	// Two merge arrays per box/key; seven bounded index lists plus pairs. This
	// is a logical payload reservation, excluding the caller's input and Go's
	// allocator/runtime overhead. Capacity is allocated explicitly, not grown.
	perTri := int64(2*unsafe.Sizeof(qualityBox{}) + 2*unsafe.Sizeof(qualityKey{}))
	perResult := int64(7*unsafe.Sizeof(uint32(0)) + unsafe.Sizeof(QualityPair{}))
	if nt > math.MaxInt64/perTri || o.MaxResults > (math.MaxInt64-qualityExactScratchBytes-nt*perTri)/perResult {
		r.State = "limited"
		return r, &QualityLimitError{"bytes", math.MaxInt64, o.MaxBytes}
	}
	r.ReservedBytes = nt*perTri + o.MaxResults*perResult + qualityExactScratchBytes
	if r.ReservedBytes > o.MaxBytes {
		r.State = "limited"
		return r, &QualityLimitError{"bytes", r.ReservedBytes, o.MaxBytes}
	}
	fs := []*QualityFinding{&r.InvalidVertices, &r.InvalidTriangles, &r.Degenerate, &r.SmallArea, &r.SmallAngle, &r.HighAspect, &r.DuplicateFaces}
	for _, f := range fs {
		f.Indices = make([]uint32, 0, int(o.MaxResults))
	}
	r.Pairs = make([]QualityPair, 0, int(o.MaxResults))
	for i, v := range m.Vertices {
		if err = q.step(); err != nil {
			return r, err
		}
		bad := false
		for _, x := range v {
			bad = bad || math.IsNaN(float64(x)) || math.IsInf(float64(x), 0)
		}
		if m.Normals != nil {
			for _, x := range m.Normals[i] {
				bad = bad || math.IsNaN(float64(x)) || math.IsInf(float64(x), 0)
			}
		}
		r.VerticesChecked++
		if bad {
			r.finding(&r.InvalidVertices, uint32(i))
		}
	}
	for i, t := range m.Triangles {
		if err = q.step(); err != nil {
			return r, err
		}
		if uint64(t[0]) >= uint64(n) || uint64(t[1]) >= uint64(n) || uint64(t[2]) >= uint64(n) {
			r.finding(&r.InvalidTriangles, uint32(i))
		}
	}
	if r.InvalidVertices.Count+r.InvalidTriangles.Count > 0 {
		return r, fmt.Errorf("%w: nonfinite coordinates/normals or missing indices", ErrInvalidField)
	}
	r.State = "partial"
	boxes := make([]qualityBox, int(nt))
	scratch := make([]qualityBox, int(nt))
	keys := make([]qualityKey, int(nt))
	ks := make([]qualityKey, int(nt))
	for i, t := range m.Triangles {
		if err = q.step(); err != nil {
			return r, err
		}
		p := qualityTriangle(m, uint32(i))
		b := qualityBox{ID: uint32(i), Lo: p[0], Hi: p[0]}
		for _, v := range p[1:] {
			for a := 0; a < 3; a++ {
				b.Lo[a] = math.Min(b.Lo[a], v[a])
				b.Hi[a] = math.Max(b.Hi[a], v[a])
			}
		}
		c := cross3(sub3(p[1], p[0]), sub3(p[2], p[0]))
		area := math.Sqrt(dot3(c, c)) / 2
		qualityStat(&r.AreaMM2, area)
		if area == 0 {
			b.Degenerate = true
			r.finding(&r.Degenerate, uint32(i))
			r.IntersectionsConclusive = false
		} else {
			minAngle := 180.0
			maxL2 := 0.0
			for j := 0; j < 3; j++ {
				u := sub3(p[(j+1)%3], p[j])
				v := sub3(p[(j+2)%3], p[j])
				cr := cross3(u, v)
				angle := math.Atan2(math.Sqrt(dot3(cr, cr)), dot3(u, v)) * 180 / math.Pi
				minAngle = math.Min(minAngle, angle)
				maxL2 = math.Max(maxL2, dot3(u, u))
			}
			aspect := math.Sqrt(3) * maxL2 / (4 * area)
			qualityStat(&r.MinAngleDegrees, minAngle)
			qualityStat(&r.AspectRatio, aspect)
			if o.MinAngleDegrees > 0 && minAngle < o.MinAngleDegrees {
				r.finding(&r.SmallAngle, uint32(i))
			}
			if o.MaxAspectRatio > 0 && aspect > o.MaxAspectRatio {
				r.finding(&r.HighAspect, uint32(i))
			}
		}
		if area < o.MinAreaMM2 {
			r.finding(&r.SmallArea, uint32(i))
		}
		boxes[i] = b
		v := t
		if v[0] > v[1] {
			v[0], v[1] = v[1], v[0]
		}
		if v[1] > v[2] {
			v[1], v[2] = v[2], v[1]
		}
		if v[0] > v[1] {
			v[0], v[1] = v[1], v[0]
		}
		keys[i] = qualityKey{v, uint32(i)}
		r.TrianglesChecked++
	}
	if err = qualityMerge(&q, keys, ks, func(a, b qualityKey) bool {
		for j := 0; j < 3; j++ {
			if a.V[j] != b.V[j] {
				return a.V[j] < b.V[j]
			}
		}
		return a.ID < b.ID
	}); err != nil {
		return r, err
	}
	// Representative links per duplicate group avoid quadratic duplicate pairs.
	for i := 1; i < len(keys); i++ {
		if err = q.step(); err != nil {
			return r, err
		}
		if keys[i].V == keys[i-1].V {
			r.finding(&r.DuplicateFaces, keys[i].ID)
			r.pair(keys[i-1].ID, keys[i].ID, QualityDuplicate)
		}
	}
	if err = qualityMerge(&q, boxes, scratch, func(a, b qualityBox) bool {
		if a.Lo[0] != b.Lo[0] {
			return a.Lo[0] < b.Lo[0]
		}
		return a.ID < b.ID
	}); err != nil {
		return r, err
	}
	for i, a := range boxes {
		for j := i + 1; j < len(boxes); j++ {
			if err = q.step(); err != nil {
				return r, err
			}
			b := boxes[j]
			if b.Lo[0] > a.Hi[0]+o.DistanceToleranceMM {
				break
			}
			if a.Degenerate || b.Degenerate {
				continue
			}
			overlap := true
			for k := 1; k < 3; k++ {
				overlap = overlap && a.Lo[k] <= b.Hi[k]+o.DistanceToleranceMM && b.Lo[k] <= a.Hi[k]+o.DistanceToleranceMM
			}
			if !overlap {
				continue
			}
			ta, tb := m.Triangles[a.ID], m.Triangles[b.ID]
			adj := false
			for _, x := range ta {
				for _, y := range tb {
					adj = adj || x == y
				}
			}
			if adj {
				r.AdjacentPairsSkipped++
				continue
			}
			if r.Candidates >= o.MaxCandidates {
				r.State = "limited"
				return r, &QualityLimitError{"candidates", r.Candidates + 1, o.MaxCandidates}
			}
			r.Candidates++
			kind := qualityIntersect(qualityTriangle(m, a.ID), qualityTriangle(m, b.ID), o.DistanceToleranceMM)
			if kind != "" {
				r.pair(a.ID, b.ID, kind)
			}
		}
	}
	r.Complete = true
	r.State = "complete"
	if !r.IntersectionsConclusive {
		r.State = "inconclusive"
	}
	return r, nil
}
