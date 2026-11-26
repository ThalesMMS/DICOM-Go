package mesh

import (
	"errors"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/render"
)

// ErrInvalidField reports a field, geometry or option the extractor cannot
// use.
var ErrInvalidField = errors.New("mesh: invalid field")

// ErrBudgetExceeded reports a surface larger than the triangle or byte
// budget of Options. It is returned before the output is allocated; the
// wrapping *BudgetError carries the exact counts.
var ErrBudgetExceeded = errors.New("mesh: surface exceeds budget")

// BudgetError is the error returned when a surface exceeds its budget.
type BudgetError struct {
	Vertices, Triangles int64
	// Bytes is the output size, including label information for multi-label surfaces.
	Bytes int64
	// MaxTriangles and MaxBytes are the budgets in force.
	MaxTriangles int64
	MaxBytes     int64
}

func (e *BudgetError) Error() string {
	return fmt.Sprintf("mesh: surface of %d triangles and %d vertices (%d bytes) exceeds the budget of %d triangles and %d bytes",
		e.Triangles, e.Vertices, e.Bytes, e.MaxTriangles, e.MaxBytes)
}

func (e *BudgetError) Unwrap() error { return ErrBudgetExceeded }

// Mesh is an indexed triangle mesh in DICOM patient LPS millimetres.
//
// Vertices are shared: every triangle corner is an index into Vertices, so a
// closed surface has each edge used by exactly two triangles, in opposite
// directions. Triangles wind counter-clockwise seen from outside, so the
// right-hand normal (B-A)x(C-A) of triangle (A, B, C) points out of the
// inside region (the samples at or above the iso value). Normals, when
// present, holds one unit outward normal per vertex.
//
// Coordinates are float32, as in VTK's default points and in STL: a
// 1 m field of view keeps a resolution better than 0.1 µm.
type Mesh struct {
	Vertices  [][3]float32
	Normals   [][3]float32
	Triangles [][3]uint32
}

// Bytes is the memory held by the mesh arrays.
func (m *Mesh) Bytes() int64 {
	if m == nil {
		return 0
	}
	return meshBytes(int64(len(m.Vertices)), int64(len(m.Triangles)), m.Normals != nil)
}

func meshBytes(vertices, triangles int64, normals bool) int64 {
	perVertex := int64(12)
	if normals {
		perVertex = 24
	}
	return vertices*perVertex + triangles*12
}

// Vertex returns vertex i as a render.Vec3.
func (m *Mesh) Vertex(i int) render.Vec3 {
	v := m.Vertices[i]
	return render.Vec3{X: float64(v[0]), Y: float64(v[1]), Z: float64(v[2])}
}

// Validate checks that every triangle indexes existing, distinct vertices,
// that the normals (when present) match the vertices one to one, and that
// every coordinate is finite.
func (m *Mesh) Validate() error {
	if m == nil {
		return fmt.Errorf("%w: nil mesh", ErrInvalidField)
	}
	if m.Normals != nil && len(m.Normals) != len(m.Vertices) {
		return fmt.Errorf("mesh: %d normals for %d vertices", len(m.Normals), len(m.Vertices))
	}
	for i, v := range m.Vertices {
		for _, c := range v {
			if math.IsNaN(float64(c)) || math.IsInf(float64(c), 0) {
				return fmt.Errorf("mesh: vertex %d is not finite", i)
			}
		}
	}
	n := uint32(len(m.Vertices))
	for i, t := range m.Triangles {
		if t[0] >= n || t[1] >= n || t[2] >= n {
			return fmt.Errorf("mesh: triangle %d indexes a missing vertex", i)
		}
		if t[0] == t[1] || t[1] == t[2] || t[0] == t[2] {
			return fmt.Errorf("mesh: triangle %d repeats a vertex", i)
		}
	}
	return nil
}
