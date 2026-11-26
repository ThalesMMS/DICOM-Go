package mesh

import (
	"context"
	"errors"
	"fmt"
	"math"
)

// ErrBooleanGeometry denotes invalid or numerically ambiguous solid geometry.
// No error from Boolean accompanies a partially published result.
var ErrBooleanGeometry = errors.New("mesh: boolean geometry")

// BooleanOperation selects a solid set operation; difference means A minus B.
type BooleanOperation string

const (
	BooleanUnion        BooleanOperation = "union"
	BooleanIntersection BooleanOperation = "intersection"
	BooleanDifference   BooleanOperation = "difference"
)

// BooleanInput borrows a closed, oriented patient-space boundary and attributes
// until return. Reference is provenance, not an instruction to load a resource.
// Scalar names/units must match between inputs; discontinuities are preserved
// at output face corners, rather than averaged across an intersection seam.
type BooleanInput struct {
	Mesh          *Mesh
	Reference     string
	VertexScalars []VertexScalarAttribute
	FaceColors    [][3]uint8
}

// BooleanOptions selects finite quotas (zero means defaults). ToleranceMM is a
// physical ambiguity/representation bound, not a distance for repairing input
// or collapsing small cavities. ScratchBytes is a conservative logical quota
// reservation: tracked slice growth plus live-scratch envelopes for shared
// topology/triangulation helpers, not measured RSS or total allocator traffic.
type BooleanOptions struct {
	MaxWorkingVertices                                                           int64
	ToleranceMM                                                                  float64
	MaxVertices, MaxTriangles, MaxPolygons, MaxPolygonVertices, MaxPlanesPerFace int64
	MaxResultVertices, MaxResultTriangles, MaxScratchBytes, MaxOutputBytes       int64
	MaxWork, MaxCandidates                                                       int64
}

// BooleanFaceSource identifies the immutable original triangle. Barycentric
// coordinates correspond to each output corner, in the original source winding.
// Reversed is true on B's cavity boundary in A minus B.
type BooleanFaceSource struct {
	Input       uint8
	Triangle    uint32
	Reversed    bool
	Barycentric [3][3]float64
}

// BooleanCornerScalar preserves a scalar per output face corner, including
// discontinuities where faces from different source meshes meet.
type BooleanCornerScalar struct {
	Name, Units string
	Values      [][3]float64
}

// BooleanResult owns all arrays. Mesh has shared geometric vertices and no
// shading normals: a seam can have two different source normals/scalar values.
// CornerNormals interpolates source normals where present, and otherwise uses
// the geometric face normal (SourceNormals marks which faces had input data).
// ColorsPresent distinguishes absent face colors from a supplied black color.
type BooleanResult struct {
	Mesh                         *Mesh
	Operation                    BooleanOperation
	References                   [2]string
	ToleranceMM                  float64
	Faces                        []BooleanFaceSource
	CornerNormals                [][3][3]float32
	SourceNormals, ColorsPresent []bool
	FaceColors                   [][3]uint8
	CornerScalars                []BooleanCornerScalar
	Diagnostics                  []string
	ScratchBytes, OutputBytes    int64
	Work, Candidates             int64
	MaxProjectionErrorMM         float64
}

type booleanRun struct {
	cuts                                        [2][][]booleanCut
	ctx                                         context.Context
	o                                           BooleanOptions
	inputs                                      [2]BooleanInput
	eps                                         float64
	work, candidates, scratch, output, polygons int64
	projectionError                             float64
}

func booleanOptions(o BooleanOptions) (BooleanOptions, error) {
	if !planeFinite(o.ToleranceMM) || o.ToleranceMM < 0 {
		return o, fmt.Errorf("%w: tolerance", ErrBooleanGeometry)
	}
	if o.ToleranceMM == 0 {
		o.ToleranceMM = 1e-6
	}
	limits := []*int64{&o.MaxVertices, &o.MaxTriangles, &o.MaxPolygons, &o.MaxPolygonVertices, &o.MaxPlanesPerFace, &o.MaxResultVertices, &o.MaxResultTriangles, &o.MaxScratchBytes, &o.MaxOutputBytes, &o.MaxWork, &o.MaxCandidates}
	defaults := []int64{200000, 400000, 200000, 128, 2048, 200000, 400000, 256 << 20, 256 << 20, 20000000, 1000000}
	limits = append(limits, &o.MaxWorkingVertices)
	defaults = append(defaults, 400000)
	for i, p := range limits {
		if *p < 0 {
			return o, fmt.Errorf("%w: negative quota", ErrBooleanGeometry)
		}
		if *p == 0 {
			*p = defaults[i]
		}
	}
	if o.MaxPlanesPerFace > 4096 || o.MaxVertices > math.MaxInt32 || o.MaxTriangles > math.MaxUint32 || o.MaxResultVertices > math.MaxUint32 || o.MaxResultTriangles > math.MaxUint32 || o.MaxWorkingVertices > math.MaxUint32 {
		return o, fmt.Errorf("%w: plane/index capacity", ErrBooleanGeometry)
	}
	return o, nil
}

func (r *booleanRun) step() error { return r.charge(&r.work, 1, r.o.MaxWork, "boolean work") }
func (r *booleanRun) charge(counter *int64, n, limit int64, resource string) error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	if n < 0 || n > limit-*counter {
		used := int64(math.MaxInt64)
		if n >= 0 && *counter <= math.MaxInt64-n {
			used = *counter + n
		}
		return &QualityLimitError{resource, used, limit}
	}
	*counter += n
	return nil
}
func (r *booleanRun) reserve(n int64) error {
	return r.charge(&r.scratch, n, r.o.MaxScratchBytes, "boolean scratch bytes")
}

// Boolean evaluates union, intersection or A minus B of closed oriented solids.
// It partitions source faces along actual triangle-intersection segments,
// classifies the pieces by oriented solid angle, retains the requested
// inside/outside pieces and conforms their edges before triangulation. It
// does not voxelize, fill holes, repair sources, or call a rendering backend.
// Coincident/coplanar overlapping boundaries and tangent component pairs are
// explicitly refused. Inputs, output and interpolation must pass validation.
func Boolean(ctx context.Context, a, b BooleanInput, operation BooleanOperation, options BooleanOptions) (*BooleanResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if operation != BooleanUnion && operation != BooleanIntersection && operation != BooleanDifference {
		return nil, fmt.Errorf("%w: operation %q", ErrBooleanGeometry, operation)
	}
	o, err := booleanOptions(options)
	if err != nil {
		return nil, err
	}
	r := &booleanRun{ctx: ctx, o: o, inputs: [2]BooleanInput{a, b}}
	if err = r.preflight(); err != nil {
		return nil, err
	}
	var sources [2][]*booleanPolygon
	for i := range sources {
		sources[i], err = r.sourcePolygons(uint8(i))
		if err != nil {
			return nil, err
		}
	}
	var pieces []*booleanPolygon
	for i := range sources {
		inside := operation == BooleanIntersection || operation == BooleanDifference && i == 1
		for _, p := range sources[i] {
			if err = r.partition(p, inside, operation == BooleanDifference && i == 1, &pieces); err != nil {
				return nil, err
			}
		}
	}
	pieces, err = r.mergePieces(pieces)
	if err != nil {
		return nil, err
	}
	return r.finish(pieces, operation)
}
