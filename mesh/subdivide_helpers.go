package mesh

import (
	"context"
	"fmt"
	"math"
	"strings"
)

func subdivisionCounts(n, t int64, o SubdivisionOptions) error {
	maxInt := int64(^uint(0) >> 1)
	if n < 0 || t < 0 || n >= math.MaxUint32 || t > (math.MaxUint32-1)/3 || n > maxInt || t > maxInt/3 || n > o.MaxVertices || t > o.MaxTriangles {
		return fmt.Errorf("%w: subdivision needs %d vertices/%d triangles, limits %d/%d", ErrBudgetExceeded, n, t, o.MaxVertices, o.MaxTriangles)
	}
	return nil
}
func subdivisionCharge(r *SubdivisionReport, b int64, o SubdivisionOptions) error {
	if b < 0 || r.AllocatedBytes > o.MaxBytes || b > o.MaxBytes-r.AllocatedBytes {
		return fmt.Errorf("%w: subdivision logical bytes charged %d + %d exceed %d", ErrBudgetExceeded, r.AllocatedBytes, b, o.MaxBytes)
	}
	r.AllocatedBytes += b
	return nil
}
func subdivisionOutputBytes(n, t int64, normals bool, va, fa int) int64 {
	return meshBytes(n, t, normals) + 4*t + 8*n*int64(va) + 8*t*int64(fa) + 256*int64(va+fa) + 512
}
func subdivisionAllocate(m *Mesh, n, t int64, va []SubdivisionVertexAttribute, fa []VertexScalarAttribute) *SubdivisionResult {
	r := &SubdivisionResult{Mesh: &Mesh{Vertices: make([][3]float32, int(n)), Triangles: make([][3]uint32, int(t))}, ParentFaces: make([]uint32, int(t)), VertexAttributes: make([]SubdivisionVertexAttribute, len(va)), FaceAttributes: make([]VertexScalarAttribute, len(fa))}
	if m.Normals != nil {
		r.Mesh.Normals = make([][3]float32, int(n))
	}
	for i, a := range va {
		r.VertexAttributes[i] = a
		r.VertexAttributes[i].Attribute.Values = make([]float64, int(n))
	}
	for i, a := range fa {
		r.FaceAttributes[i] = a
		r.FaceAttributes[i].Values = make([]float64, int(t))
	}
	return r
}
func subdivisionAttributes(ctx context.Context, m *Mesh, o SubdivisionOptions) error {
	if len(o.VertexAttributes) > MaxVertexScalarAttributes || len(o.FaceAttributes) > MaxVertexScalarAttributes {
		return fmt.Errorf("%w: subdivision attribute count", ErrInvalidField)
	}
	validate := func(a VertexScalarAttribute, n int) error {
		switch strings.ToLower(a.Name) {
		case "x", "y", "z", "nx", "ny", "nz":
			return fmt.Errorf("%w: reserved attribute name", ErrInvalidField)
		}
		if !scalarName(a.Name) || !scalarUnits(a.Units) || len(a.Values) != n {
			return fmt.Errorf("%w: subdivision attribute contract %q", ErrInvalidField, a.Name)
		}
		for i, v := range a.Values {
			if i&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			if !finiteCurvature(v) {
				return fmt.Errorf("%w: subdivision nonfinite attribute %q", ErrInvalidField, a.Name)
			}
		}
		return nil
	}
	for i, a := range o.VertexAttributes {
		if a.Policy > SubdivisionEqualOnly {
			return fmt.Errorf("%w: subdivision interpolation policy", ErrInvalidField)
		}
		if err := validate(a.Attribute, len(m.Vertices)); err != nil {
			return err
		}
		for _, b := range o.VertexAttributes[:i] {
			if a.Attribute.Name == b.Attribute.Name {
				return fmt.Errorf("%w: duplicate vertex attribute", ErrInvalidField)
			}
		}
	}
	for i, a := range o.FaceAttributes {
		if err := validate(a, len(m.Triangles)); err != nil {
			return err
		}
		for _, b := range o.FaceAttributes[:i] {
			if a.Name == b.Name {
				return fmt.Errorf("%w: duplicate face attribute", ErrInvalidField)
			}
		}
	}
	return ctx.Err()
}
func subdivisionPoint(m *Mesh, i uint32) [3]float64 {
	p := m.Vertices[i]
	return [3]float64{float64(p[0]), float64(p[1]), float64(p[2])}
}
func subdivisionEdgeLength(m *Mesh, a, b uint32) float64 {
	d := sub3(subdivisionPoint(m, a), subdivisionPoint(m, b))
	return math.Hypot(math.Hypot(d[0], d[1]), d[2])
}
func subdivisionGeometry(ctx context.Context, m *Mesh) error {
	for i, f := range m.Triangles {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		a, b, c := subdivisionPoint(m, f[0]), subdivisionPoint(m, f[1]), subdivisionPoint(m, f[2])
		normal := cross3(sub3(b, a), sub3(c, a))
		if dot3(normal, normal) == 0 {
			return fmt.Errorf("%w: subdivision degenerate triangle %d", ErrInvalidField, i)
		}
	}
	return ctx.Err()
}
func subdivisionMidpoint(current, next *SubdivisionResult, a, b, mid uint32) error {
	pa, pb := current.Mesh.Vertices[a], current.Mesh.Vertices[b]
	p := [3]float32{}
	for k := 0; k < 3; k++ {
		p[k] = float32(float64(pa[k])*.5 + float64(pb[k])*.5)
	}
	if p == pa || p == pb {
		return ErrSubdivisionPrecision
	}
	next.Mesh.Vertices[mid] = p
	if current.Mesh.Normals != nil {
		na, nb := current.Mesh.Normals[a], current.Mesh.Normals[b]
		v := [3]float64{}
		for k := 0; k < 3; k++ {
			v[k] = float64(na[k])*.5 + float64(nb[k])*.5
		}
		length := math.Hypot(math.Hypot(v[0], v[1]), v[2])
		if length == 0 {
			return ErrSubdivisionPrecision
		}
		for k := 0; k < 3; k++ {
			next.Mesh.Normals[mid][k] = float32(v[k] / length)
		}
	}
	for i, aAttr := range current.VertexAttributes {
		x, y := aAttr.Attribute.Values[a], aAttr.Attribute.Values[b]
		if aAttr.Policy == SubdivisionEqualOnly && x != y {
			return fmt.Errorf("%w: noninterpolable vertex attribute %q crosses unequal endpoints", ErrInvalidField, aAttr.Attribute.Name)
		}
		sum := x + y
		value := sum * .5
		if math.IsInf(sum, 0) {
			value = x*.5 + y*.5
		}
		next.VertexAttributes[i].Attribute.Values[mid] = value
	}
	return nil
}

// Each mask splits only marked original edges. Rotate two-edge masks so the
// marked edges are AB and BC; all templates have the parent's winding.
func subdivisionChildren(f, mid [3]uint32) (out [4][3]uint32, count int) {
	n := 0
	for _, m := range mid {
		if m != math.MaxUint32 {
			n++
		}
	}
	if n == 0 {
		out[0] = f
		return out, 1
	}
	if n == 3 {
		a, b, c := f[0], f[1], f[2]
		x, y, z := mid[0], mid[1], mid[2]
		out = [4][3]uint32{{a, x, z}, {x, b, y}, {z, y, c}, {x, y, z}}
		return out, 4
	}
	for i := 0; i < 3; i++ {
		if n == 1 && mid[i] != math.MaxUint32 {
			a, b, c := f[i], f[(i+1)%3], f[(i+2)%3]
			x := mid[i]
			out[0] = [3]uint32{a, x, c}
			out[1] = [3]uint32{x, b, c}
			return out, 2
		}
		if n == 2 && mid[i] != math.MaxUint32 && mid[(i+1)%3] != math.MaxUint32 {
			a, b, c := f[i], f[(i+1)%3], f[(i+2)%3]
			x, y := mid[i], mid[(i+1)%3]
			out[0] = [3]uint32{b, y, x}
			out[1] = [3]uint32{a, x, c}
			out[2] = [3]uint32{x, y, c}
			return out, 3
		}
	}
	panic("subdivision mask invariant")
}
