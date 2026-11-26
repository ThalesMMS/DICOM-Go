package mesh

import (
	"context"
	"errors"
	"fmt"
	"math"
)

// SubdivisionInterpolation declares a vertex scalar's midpoint policy.
type SubdivisionInterpolation uint8

const (
	SubdivisionLinear SubdivisionInterpolation = iota
	// SubdivisionEqualOnly preserves noninterpolable values, refusing a split
	// across unequal endpoint values. No implicit category interpolation.
	SubdivisionEqualOnly
)

type SubdivisionVertexAttribute struct {
	Attribute VertexScalarAttribute
	Policy    SubdivisionInterpolation
}

// SubdivisionOptions requires explicit finite positive limits. MaxBytes bounds
// cumulative logical array allocation, including retired passes and scratch,
// excluding caller inputs and Go allocator/runtime overhead, not process RSS.
type SubdivisionOptions struct {
	MaxEdgeLengthMM                     float64
	MaxVertices, MaxTriangles, MaxBytes int64
	MaxPasses                           int
	VertexAttributes                    []SubdivisionVertexAttribute
	// FaceAttributes retain parent values exactly, even for category-like
	// numeric values. Names/units follow VertexScalarAttribute's contract.
	FaceAttributes []VertexScalarAttribute
}
type SubdivisionReport struct {
	CriterionMM                                        float64
	InputVertices, InputTriangles, Vertices, Triangles int64
	Passes                                             int
	AllocatedBytes                                     int64
	MaxEdgeLengthMM                                    float64
	Stop                                               string
}
type SubdivisionResult struct {
	Mesh             *Mesh
	VertexAttributes []SubdivisionVertexAttribute
	FaceAttributes   []VertexScalarAttribute
	// ParentFaces is the original source triangle of each output triangle.
	ParentFaces []uint32
	Report      SubdivisionReport
}

// SubdivisionError reports refusal/cancellation and last complete counts.
// No partial mesh is exposed, including when earlier passes were conformant.
type SubdivisionError struct {
	Report SubdivisionReport
	Cause  error
}

func (e *SubdivisionError) Error() string {
	return fmt.Sprintf("mesh: subdivision %s after %d passes: %v", e.Report.Stop, e.Report.Passes, e.Cause)
}
func (e *SubdivisionError) Unwrap() error { return e.Cause }

var ErrSubdivisionPrecision = errors.New("mesh: subdivision midpoint/normal is not representable")

// SubdivideEdges linearly refines only physical edges exceeding the threshold.
// A global edge midpoint is shared by all incident faces; conformant templates
// preserve winding, components and boundary loops of supported manifold input.
// Source arrays/attributes must stay stable for the call and are never changed.
// Invalid topology/geometry, exhausted limits and cancellation return nil result.
// Normals, if supplied, keep original entries and normalize midpoint averages;
// antipodal/zero midpoint normals refuse. No smoothing or surface projection.
func SubdivideEdges(ctx context.Context, m *Mesh, o SubdivisionOptions) (out *SubdivisionResult, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	report := SubdivisionReport{CriterionMM: o.MaxEdgeLengthMM}
	defer func() {
		if err != nil {
			out = nil
			var e *SubdivisionError
			if !errors.As(err, &e) {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					report.Stop = "cancelled"
				} else if report.Stop == "" {
					report.Stop = "refused"
				}
				err = &SubdivisionError{Report: report, Cause: err}
			}
		}
	}()
	if err = ctx.Err(); err != nil {
		report.Stop = "cancelled"
		return nil, err
	}
	if m == nil || !finiteCurvature(o.MaxEdgeLengthMM) || o.MaxEdgeLengthMM <= 0 || o.MaxVertices <= 0 || o.MaxTriangles <= 0 || o.MaxBytes <= 0 || o.MaxPasses <= 0 || o.MaxPasses > 64 || len(o.VertexAttributes) > MaxVertexScalarAttributes || len(o.FaceAttributes) > MaxVertexScalarAttributes {
		return nil, fmt.Errorf("%w: subdivision options/mesh", ErrInvalidField)
	}
	n, t := int64(len(m.Vertices)), int64(len(m.Triangles))
	report.InputVertices = n
	report.InputTriangles = t
	report.Vertices = n
	report.Triangles = t
	if err = subdivisionCounts(n, t, o); err != nil {
		report.Stop = "counts"
		return nil, err
	}
	// Fixed metadata, conservative at256 bytes per attribute, and topology's
	// two16-byte edge arrays, parent/rank corners, first/flag vertices.
	if err = subdivisionCharge(&report, 4096+256*int64(len(o.VertexAttributes)+len(o.FaceAttributes))+111*t+5*n, o); err != nil {
		report.Stop = "bytes"
		return nil, err
	}
	if err = validateCurvatureInput(ctx, m); err != nil {
		return nil, err
	}
	for i, v := range m.Normals {
		if i&1023 == 0 {
			if err = ctx.Err(); err != nil {
				return nil, err
			}
		}
		for _, c := range v {
			if !finiteCurvature(float64(c)) {
				return nil, fmt.Errorf("%w: subdivision normal", ErrInvalidField)
			}
		}
	}
	if err = subdivisionGeometry(ctx, m); err != nil {
		return nil, err
	}
	if err = curvatureTopology(ctx, m, make([]CurvatureFlags, len(m.Vertices))); err != nil {
		return nil, err
	}
	if err = subdivisionAttributes(ctx, m, o); err != nil {
		return nil, err
	}
	if err = subdivisionCharge(&report, subdivisionOutputBytes(n, t, m.Normals != nil, len(o.VertexAttributes), len(o.FaceAttributes)), o); err != nil {
		report.Stop = "bytes"
		return nil, err
	}
	current := subdivisionAllocate(m, n, t, o.VertexAttributes, o.FaceAttributes)
	for i := range current.Mesh.Vertices {
		if i&1023 == 0 {
			if err = ctx.Err(); err != nil {
				return nil, err
			}
		}
		current.Mesh.Vertices[i] = m.Vertices[i]
		if m.Normals != nil {
			current.Mesh.Normals[i] = m.Normals[i]
		}
		for a := range current.VertexAttributes {
			current.VertexAttributes[a].Attribute.Values[i] = o.VertexAttributes[a].Attribute.Values[i]
		}
	}
	for i := range current.Mesh.Triangles {
		if i&1023 == 0 {
			if err = ctx.Err(); err != nil {
				return nil, err
			}
		}
		current.Mesh.Triangles[i] = m.Triangles[i]
		current.ParentFaces[i] = uint32(i)
		for a := range current.FaceAttributes {
			current.FaceAttributes[a].Values[i] = o.FaceAttributes[a].Values[i]
		}
	}
	for {
		if err = ctx.Err(); err != nil {
			report.Stop = "cancelled"
			return nil, err
		}
		n, t = int64(len(current.Mesh.Vertices)), int64(len(current.Mesh.Triangles))
		if err = subdivisionCharge(&report, 108*t, o); err != nil {
			report.Stop = "bytes"
			return nil, err
		}
		edges := make([]curvatureEdge, 3*int(t))
		scratch := make([]curvatureEdge, len(edges))
		mids := make([]uint32, len(edges))
		for i := range mids {
			if i&1023 == 0 {
				if err = ctx.Err(); err != nil {
					return nil, err
				}
			}
			mids[i] = math.MaxUint32
		}
		for f, face := range current.Mesh.Triangles {
			if f&1023 == 0 {
				if err = ctx.Err(); err != nil {
					return nil, err
				}
			}
			for i := 0; i < 3; i++ {
				edges[3*f+i] = curvatureEdge{key: edgeID(face[i], face[(i+1)%3]), corner: uint32(3*f + i)}
			}
		}
		edges, err = sortCurvatureEdges(ctx, edges, scratch)
		if err != nil {
			return nil, err
		}
		split := int64(0)
		report.MaxEdgeLengthMM = 0
		for start := 0; start < len(edges); {
			// Group multiplicity changes start parity; check every group rather
			// than masking the corner index, so boundary/interior mixtures
			// cannot skip all subsequent checkpoints.
			if err = ctx.Err(); err != nil {
				return nil, err
			}
			end := start + 1
			for end < len(edges) && edges[end].key == edges[start].key {
				end++
			}
			a, b := uint32(edges[start].key>>32), uint32(edges[start].key)
			length := subdivisionEdgeLength(current.Mesh, a, b)
			report.MaxEdgeLengthMM = math.Max(report.MaxEdgeLengthMM, length)
			if length > o.MaxEdgeLengthMM {
				mid := uint32(n + split)
				split++
				for i := start; i < end; i++ {
					mids[edges[i].corner] = mid
				}
			}
			start = end
		}
		if split == 0 {
			report.Stop = "complete"
			report.Vertices = n
			report.Triangles = t
			current.Report = report
			if err = ctx.Err(); err != nil {
				report.Stop = "cancelled"
				return nil, err
			}
			return current, nil
		}
		if report.Passes == o.MaxPasses {
			report.Stop = "passes"
			return nil, ErrBudgetExceeded
		}
		nextT := t
		for f := range current.Mesh.Triangles {
			if f&1023 == 0 {
				if err = ctx.Err(); err != nil {
					return nil, err
				}
			}
			for i := 0; i < 3; i++ {
				if mids[3*f+i] != math.MaxUint32 {
					nextT++
				}
			}
		}
		if err = subdivisionCounts(n+split, nextT, o); err != nil {
			report.Stop = "counts"
			return nil, err
		}
		if err = subdivisionCharge(&report, subdivisionOutputBytes(n+split, nextT, current.Mesh.Normals != nil, len(current.VertexAttributes), len(current.FaceAttributes)), o); err != nil {
			report.Stop = "bytes"
			return nil, err
		}
		next := subdivisionAllocate(current.Mesh, n+split, nextT, current.VertexAttributes, current.FaceAttributes)
		for i := 0; i < int(n); i++ {
			if i&1023 == 0 {
				if err = ctx.Err(); err != nil {
					return nil, err
				}
			}
			next.Mesh.Vertices[i] = current.Mesh.Vertices[i]
			if current.Mesh.Normals != nil {
				next.Mesh.Normals[i] = current.Mesh.Normals[i]
			}
			for a := range next.VertexAttributes {
				next.VertexAttributes[a].Attribute.Values[i] = current.VertexAttributes[a].Attribute.Values[i]
			}
		}
		for start := 0; start < len(edges); {
			// Group multiplicity changes start parity; check every group rather
			// than masking the corner index, so boundary/interior mixtures
			// cannot skip all subsequent checkpoints.
			if err = ctx.Err(); err != nil {
				return nil, err
			}
			end := start + 1
			for end < len(edges) && edges[end].key == edges[start].key {
				end++
			}
			mid := mids[edges[start].corner]
			if mid != math.MaxUint32 {
				a, b := uint32(edges[start].key>>32), uint32(edges[start].key)
				if err = subdivisionMidpoint(current, next, a, b, mid); err != nil {
					report.Stop = "precision-or-attribute"
					return nil, err
				}
			}
			start = end
		}
		faceAt := 0
		for f, face := range current.Mesh.Triangles {
			if f&1023 == 0 {
				if err = ctx.Err(); err != nil {
					return nil, err
				}
			}
			children, count := subdivisionChildren(face, [3]uint32{mids[3*f], mids[3*f+1], mids[3*f+2]})
			for c := 0; c < count; c++ {
				child := children[c]
				pa, pb, pc := subdivisionPoint(current.Mesh, face[0]), subdivisionPoint(current.Mesh, face[1]), subdivisionPoint(current.Mesh, face[2])
				ca, cb, cc := subdivisionPoint(next.Mesh, child[0]), subdivisionPoint(next.Mesh, child[1]), subdivisionPoint(next.Mesh, child[2])
				if dot3(cross3(sub3(pb, pa), sub3(pc, pa)), cross3(sub3(cb, ca), sub3(cc, ca))) <= 0 {
					report.Stop = "precision"
					return nil, ErrSubdivisionPrecision
				}
				next.Mesh.Triangles[faceAt] = child
				next.ParentFaces[faceAt] = current.ParentFaces[f]
				for a := range next.FaceAttributes {
					next.FaceAttributes[a].Values[faceAt] = current.FaceAttributes[a].Values[f]
				}
				faceAt++
			}
		}
		if err = subdivisionGeometry(ctx, next.Mesh); err != nil {
			report.Stop = "precision"
			return nil, err
		}
		current = next
		report.Passes++
		report.Vertices = n + split
		report.Triangles = nextT
	}
}
