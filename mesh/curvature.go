package mesh

import (
	"context"
	"fmt"
	"math"
)

const (
	DefaultCurvatureMaxVertices  = 2_000_000
	DefaultCurvatureMaxTriangles = 4_000_000
	// Scale-relative guard for singular triangles and oriented normal sums.
	curvatureRelativeEpsilon = 64 * 2.220446049250313e-16
)

// CurvatureOptions bounds the arrays used by VertexCurvatures. Zero selects
// 2 million vertices, 4 million triangles and DefaultMaxBytes (1 GiB).
// Negative limits are invalid; positive limits are explicit finite bounds.
type CurvatureOptions struct {
	MaxVertices, MaxTriangles, MaxBytes int64
}

// CurvatureFlags describes vertices without an interior estimate, and a
// projection of incompatible discrete mean/Gaussian estimates.
type CurvatureFlags uint8

const (
	// CurvatureBoundary masks all four curvatures at a boundary vertex.
	CurvatureBoundary CurvatureFlags = 1 << iota
	// CurvatureUnused masks vertices not used by any triangle.
	CurvatureUnused
	// CurvaturePrincipalClamped means H²-K was negative: the two principal
	// estimates equal H. H and K themselves have not been altered.
	CurvaturePrincipalClamped
)

// CurvatureResult owns one value per input vertex. Mean and principal
// curvatures are in mm⁻¹, Gaussian in mm⁻² and MixedArea in mm². All numeric
// values are finite. Boundary/unused vertices have zero curvature; Flags
// distinguishes them from flat interior vertices. MixedArea is defined at
// boundary vertices, and is zero at unused vertices.
type CurvatureResult struct {
	Mean, Gaussian             []float64
	PrincipalMin, PrincipalMax []float64
	MixedArea                  []float64
	Flags                      []CurvatureFlags
}

// CurvatureBudgetError reports a conservative reservation for result and
// scratch arrays, excluding input, allocator/runtime overhead and RSS.
type CurvatureBudgetError struct {
	Vertices, Triangles, Bytes          int64
	MaxVertices, MaxTriangles, MaxBytes int64
}

func (e *CurvatureBudgetError) Error() string {
	return fmt.Sprintf("mesh: curvature of %d vertices and %d triangles reserves %d bytes; limits %d vertices, %d triangles, %d bytes", e.Vertices, e.Triangles, e.Bytes, e.MaxVertices, e.MaxTriangles, e.MaxBytes)
}

func (e *CurvatureBudgetError) Unwrap() error { return ErrBudgetExceeded }

// VertexCurvatures estimates signed mean, Gaussian and principal curvatures
// using cotangent weights and mixed Voronoi areas (Meyer et al., Discrete
// Differential-Geometry Operators for Triangulated 2-Manifolds, §§3–5).
// Mean is half the projection of the mean-curvature vector onto the
// area-weighted oriented normal: an outward-wound convex sphere is positive.
// Input normals do not determine the sign. PrincipalMin <= PrincipalMax.
//
// Each used vertex must have one manifold face fan. Degenerate/duplicate
// triangles, inconsistent winding, non-manifold edges/vertices and singular
// interior normal sums are errors. Boundary vertices are masked; no repair,
// welding or self-intersection test is performed. Input arrays are borrowed
// for this call only and must remain stable. The function does not mutate
// them, uses no shared scratch, and returns no partial result on error.
func VertexCurvatures(ctx context.Context, m *Mesh, opts CurvatureOptions) (*CurvatureResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("%w: curvature of nil mesh", ErrInvalidField)
	}
	n, t := int64(len(m.Vertices)), int64(len(m.Triangles))
	if err := curvatureBudget(n, t, opts); err != nil {
		return nil, err
	}
	if err := validateCurvatureInput(ctx, m); err != nil {
		return nil, err
	}
	result := &CurvatureResult{}
	if n == 0 {
		return result, nil
	}
	result.Mean = make([]float64, n)
	result.Gaussian = make([]float64, n)
	result.PrincipalMin = make([]float64, n)
	result.PrincipalMax = make([]float64, n)
	result.MixedArea = make([]float64, n)
	result.Flags = make([]CurvatureFlags, n)
	if err := curvatureTopology(ctx, m, result.Flags); err != nil {
		return nil, err
	}
	// Together with the topology arrays, the capacities are at most
	// 109*N + 111*T bytes; curvatureBudget reserves 128*N+128*T+8192.
	normals := make([][3]float64, n)
	laplacian := make([][3]float64, n)
	angles := make([]float64, n)
	normalScale := make([]float64, n)
	for f, face := range m.Triangles {
		if f&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		p := [3][3]float64{m.point(face[0]), m.point(face[1]), m.point(face[2])}
		d := [3][3]float64{sub3(p[1], p[0]), sub3(p[2], p[1]), sub3(p[0], p[2])}
		length2 := [3]float64{dot3(d[0], d[0]), dot3(d[1], d[1]), dot3(d[2], d[2])}
		cross := cross3(d[0], sub3(p[2], p[0]))
		twiceArea := math.Sqrt(dot3(cross, cross))
		if min(length2[0], length2[1], length2[2]) == 0 || twiceArea <= curvatureRelativeEpsilon*max(length2[0], length2[1], length2[2]) {
			return nil, fmt.Errorf("%w: singular curvature triangle %d", ErrInvalidField, f)
		}
		var cot [3]float64
		obtuse := -1
		for i, v := range face {
			a, b := sub3(p[(i+1)%3], p[i]), sub3(p[(i+2)%3], p[i])
			dot := dot3(a, b)
			cot[i] = dot / twiceArea
			if dot < 0 {
				obtuse = i
			}
			angles[v] += math.Atan2(twiceArea, dot)
			normalScale[v] += twiceArea
			for k := 0; k < 3; k++ {
				normals[v][k] += cross[k]
			}
		}
		for i, v := range face {
			if obtuse >= 0 {
				fraction := 0.125 // one quarter of triangle area
				if i == obtuse {
					fraction = 0.25 // one half of triangle area
				}
				result.MixedArea[v] += fraction * twiceArea
			} else {
				result.MixedArea[v] += (cot[(i+1)%3]*length2[(i+2)%3] + cot[(i+2)%3]*length2[i]) / 8
			}
			j := (i + 1) % 3
			weight := cot[(i+2)%3]
			for k := 0; k < 3; k++ {
				value := weight * (p[i][k] - p[j][k])
				laplacian[v][k] += value
				laplacian[face[j]][k] -= value
			}
		}
	}
	for i := range m.Vertices {
		if i&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if result.Flags[i]&(CurvatureBoundary|CurvatureUnused) != 0 {
			continue
		}
		area := result.MixedArea[i]
		normalLength := math.Sqrt(dot3(normals[i], normals[i]))
		if area <= 0 || normalLength <= curvatureRelativeEpsilon*normalScale[i] {
			return nil, fmt.Errorf("%w: singular curvature area/normal at vertex %d", ErrInvalidField, i)
		}
		h := dot3(laplacian[i], normals[i]) / (4 * area * normalLength)
		g := (2*math.Pi - angles[i]) / area
		if !finiteCurvature(h) || !finiteCurvature(g) || !finiteCurvature(area) {
			return nil, fmt.Errorf("%w: non-finite curvature at vertex %d", ErrInvalidField, i)
		}
		delta := h*h - g
		if delta < 0 {
			delta = 0
			result.Flags[i] |= CurvaturePrincipalClamped
		}
		root := math.Sqrt(delta)
		result.Mean[i], result.Gaussian[i] = h, g
		result.PrincipalMin[i], result.PrincipalMax[i] = h-root, h+root
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func finiteCurvature(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// Match Mesh.Validate's representation checks with cancellation in bounded
// chunks, without changing that existing synchronous public API.
func validateCurvatureInput(ctx context.Context, m *Mesh) error {
	if m.Normals != nil && len(m.Normals) != len(m.Vertices) {
		return fmt.Errorf("%w: %d normals for %d vertices", ErrInvalidField, len(m.Normals), len(m.Vertices))
	}
	for i, v := range m.Vertices {
		if i&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		for _, c := range v {
			if !finiteCurvature(float64(c)) {
				return fmt.Errorf("%w: curvature vertex %d is not finite", ErrInvalidField, i)
			}
		}
	}
	n := uint32(len(m.Vertices))
	for i, face := range m.Triangles {
		if i&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if face[0] >= n || face[1] >= n || face[2] >= n || face[0] == face[1] || face[1] == face[2] || face[0] == face[2] {
			return fmt.Errorf("%w: curvature triangle %d has missing/repeated indices", ErrInvalidField, i)
		}
	}
	return ctx.Err()
}

func curvatureBudget(n, t int64, opts CurvatureOptions) error {
	if opts.MaxVertices < 0 || opts.MaxTriangles < 0 || opts.MaxBytes < 0 {
		return fmt.Errorf("%w: negative curvature budget", ErrInvalidField)
	}
	if opts.MaxVertices == 0 {
		opts.MaxVertices = DefaultCurvatureMaxVertices
	}
	if opts.MaxTriangles == 0 {
		opts.MaxTriangles = DefaultCurvatureMaxTriangles
	}
	if opts.MaxBytes == 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	bytes := int64(0)
	if n != 0 || t != 0 {
		// Count arithmetic is checked before multiplication, even for synthetic
		// counts and caller-raised budgets. MaxUint32 is the corner sentinel.
		maxInt := int64(^uint(0) >> 1)
		if n < 0 || t < 0 || n >= math.MaxUint32 || t > (math.MaxUint32-1)/3 || n > maxInt || t > maxInt/3 || n > (math.MaxInt64-8192)/128 || t > (math.MaxInt64-8192-128*n)/128 {
			return fmt.Errorf("%w: curvature index/count capacity", ErrBudgetExceeded)
		}
		bytes = 128*n + 128*t + 8192
	}
	if n > opts.MaxVertices || t > opts.MaxTriangles || bytes > opts.MaxBytes {
		return &CurvatureBudgetError{Vertices: n, Triangles: t, Bytes: bytes, MaxVertices: opts.MaxVertices, MaxTriangles: opts.MaxTriangles, MaxBytes: opts.MaxBytes}
	}
	return nil
}
