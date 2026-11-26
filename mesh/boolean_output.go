package mesh

import (
	"fmt"
	"math"
	"unsafe"
)

// Every slice growth is admitted before allocation, with explicitly doubled
// capacity. Cumulative charges include old/new overlap and discarded scratch.
func booleanGrow[T any](r *booleanRun, values *[]T, maximum int64, output bool) error {
	if int64(len(*values)) >= maximum {
		return &QualityLimitError{"boolean array elements", int64(len(*values)) + 1, maximum}
	}
	if len(*values) < cap(*values) {
		return r.ctx.Err()
	}
	n := min(maximum, max(int64(8), int64(cap(*values))*2))
	if n > int64(^uint(0)>>1) {
		return &QualityLimitError{"boolean addressable capacity", n, int64(^uint(0) >> 1)}
	}
	var item T
	size := int64(unsafe.Sizeof(item))
	if size != 0 && n > math.MaxInt64/size {
		return &QualityLimitError{"boolean array bytes", math.MaxInt64, 0}
	}
	var err error
	if output {
		err = r.charge(&r.output, n*size, r.o.MaxOutputBytes, "boolean output bytes")
	} else {
		err = r.reserve(n * size)
	}
	if err != nil {
		return err
	}
	next := make([]T, len(*values), int(n))
	copy(next, *values)
	*values = next
	return nil
}

type booleanVertices struct {
	r      *booleanRun
	points [][3]float64
	mesh   *Mesh
}

func (v *booleanVertices) add(p [3]float64) (uint32, error) {
	f := [3]float32{float32(p[0]), float32(p[1]), float32(p[2])}
	converted := [3]float64{float64(f[0]), float64(f[1]), float64(f[2])}
	d := sub3(p, converted)
	if !planeVecFinite(converted) || math.Sqrt(dot3(d, d)) > v.r.o.ToleranceMM {
		return 0, fmt.Errorf("%w: float32 position exceeds tolerance", ErrBooleanGeometry)
	}
	for i, q := range v.points {
		if err := v.r.step(); err != nil {
			return 0, err
		}
		d := sub3(p, q)
		if dot3(d, d) <= v.r.eps*v.r.eps {
			return uint32(i), nil
		}
		if f == v.mesh.Vertices[i] {
			return 0, fmt.Errorf("%w: float32 merges distinct positions (separation=%g roundoff=%g)", ErrBooleanGeometry, math.Sqrt(dot3(d, d)), v.r.eps)
		}
	}
	if err := booleanGrow(v.r, &v.points, v.r.o.MaxResultVertices, false); err != nil {
		return 0, err
	}
	if err := booleanGrow(v.r, &v.mesh.Vertices, v.r.o.MaxResultVertices, true); err != nil {
		return 0, err
	}
	id := uint32(len(v.points))
	v.points = append(v.points, p)
	v.mesh.Vertices = append(v.mesh.Vertices, f)
	return id, nil
}

type booleanEdgePoint struct {
	id uint32
	t  float64
}

func (r *booleanRun) finish(pieces []*booleanPolygon, operation BooleanOperation) (*BooleanResult, error) {
	if err := r.charge(&r.output, 512+int64(len(r.inputs[0].Reference)+len(r.inputs[1].Reference)), r.o.MaxOutputBytes, "boolean output bytes"); err != nil {
		return nil, err
	}
	result := &BooleanResult{Mesh: &Mesh{}, Operation: operation, References: [2]string{r.inputs[0].Reference, r.inputs[1].Reference}, ToleranceMM: r.o.ToleranceMM}
	schema := r.inputs[0].VertexScalars
	if len(r.inputs[0].Mesh.Triangles) == 0 {
		schema = r.inputs[1].VertexScalars
	}
	if err := r.charge(&r.output, int64(len(schema))*64, r.o.MaxOutputBytes, "boolean attribute headers"); err != nil {
		return nil, err
	}
	result.CornerScalars = make([]BooleanCornerScalar, len(schema))
	for i, s := range schema {
		result.CornerScalars[i] = BooleanCornerScalar{Name: s.Name, Units: s.Units}
	}
	vertices := &booleanVertices{r: r, mesh: result.Mesh}
	for _, p := range pieces {
		if len(p.p) != 3 {
			return nil, fmt.Errorf("%w: unfinished triangulation", ErrBooleanGeometry)
		}
		var ids [3]uint32
		for i, point := range p.p {
			var err error
			ids[i], err = vertices.add(point)
			if err != nil {
				return nil, err
			}
		}
		if err := r.emitFace(result, p, ids); err != nil {
			return nil, err
		}
	}
	if len(result.Mesh.Triangles) > 0 {
		// Output topology is new; reserve its validation scratch independently.
		if err := r.reserve(int64(len(result.Mesh.Vertices))*64 + int64(len(result.Mesh.Triangles))*1024 + 32768); err != nil {
			return nil, err
		}
		if err := r.validateSolid(result.Mesh, true); err != nil {
			return nil, err
		}
		if err := r.reserve(int64(len(result.Mesh.Vertices))*16 + int64(len(result.Mesh.Triangles))*64); err != nil {
			return nil, err
		}
		if err := r.charge(&r.work, int64(len(result.Mesh.Vertices))+24*int64(len(result.Mesh.Triangles)), r.o.MaxWork, "boolean output components"); err != nil {
			return nil, err
		}
		labels, count := ConnectedComponents(result.Mesh)
		if err := r.orientation(result.Mesh, labels, count); err != nil {
			return nil, err
		}
	}
	result.Diagnostics = []string{"shared geometric vertices; shading normals and scalars preserved per face corner"}
	if err := r.ctx.Err(); err != nil {
		return nil, err
	}
	result.Work, result.Candidates, result.ScratchBytes, result.OutputBytes, result.MaxProjectionErrorMM = r.work, r.candidates, r.scratch, r.output, r.projectionError
	return result, nil
}

func (r *booleanRun) emitFace(result *BooleanResult, p *booleanPolygon, ids [3]uint32) error {
	if err := r.step(); err != nil {
		return err
	}
	if ids[0] == ids[1] || ids[1] == ids[2] || ids[2] == ids[0] {
		return fmt.Errorf("%w: collapsed triangle", ErrBooleanGeometry)
	}
	out := result.Mesh
	a, b, c := out.point(ids[0]), out.point(ids[1]), out.point(ids[2])
	geometric := cross3(sub3(b, a), sub3(c, a))
	length := math.Sqrt(dot3(geometric, geometric))
	if length == 0 || dot3(geometric, p.n) <= 0 {
		return fmt.Errorf("%w: rounded face orientation", ErrBooleanGeometry)
	}
	geometric = scale3(geometric, 1/length)
	in := r.inputs[p.input]
	t := in.Mesh.Triangles[p.face]
	original := [3][3]float64{in.Mesh.point(t[0]), in.Mesh.point(t[1]), in.Mesh.point(t[2])}
	source := BooleanFaceSource{Input: p.input, Triangle: p.face, Reversed: p.reversed}
	var normals [3][3]float32
	for corner, id := range ids {
		point := out.point(id)
		closest, bary := closestOnTriangle(point, original[0], original[1], original[2])
		d := sub3(point, closest)
		distance := math.Sqrt(dot3(d, d))
		if distance > r.o.ToleranceMM {
			return fmt.Errorf("%w: source interpolation exceeds tolerance", ErrBooleanGeometry)
		}
		r.projectionError = math.Max(r.projectionError, distance)
		source.Barycentric[corner] = bary
		n := geometric
		if in.Mesh.Normals != nil {
			n = [3]float64{}
			for j, vertex := range t {
				for axis := 0; axis < 3; axis++ {
					n[axis] += bary[j] * float64(in.Mesh.Normals[vertex][axis])
				}
			}
			l := math.Sqrt(dot3(n, n))
			if l == 0 {
				return fmt.Errorf("%w: unsupported cancelling source normals", ErrBooleanGeometry)
			}
			n = scale3(n, 1/l)
			if p.reversed {
				n = scale3(n, -1)
			}
		}
		normals[corner] = [3]float32{float32(n[0]), float32(n[1]), float32(n[2])}
	}
	limit := r.o.MaxResultTriangles
	if err := booleanGrow(r, &out.Triangles, limit, true); err != nil {
		return err
	}
	if err := booleanGrow(r, &result.Faces, limit, true); err != nil {
		return err
	}
	if err := booleanGrow(r, &result.CornerNormals, limit, true); err != nil {
		return err
	}
	if err := booleanGrow(r, &result.SourceNormals, limit, true); err != nil {
		return err
	}
	if err := booleanGrow(r, &result.ColorsPresent, limit, true); err != nil {
		return err
	}
	if err := booleanGrow(r, &result.FaceColors, limit, true); err != nil {
		return err
	}
	for i := range result.CornerScalars {
		s := &result.CornerScalars[i]
		if err := booleanGrow(r, &s.Values, limit, true); err != nil {
			return err
		}
		var values [3]float64
		for corner, bary := range source.Barycentric {
			for j, vertex := range t {
				values[corner] += bary[j] * in.VertexScalars[i].Values[vertex]
			}
		}
		if !planeVecFinite(values) {
			return fmt.Errorf("%w: scalar interpolation overflow", ErrBooleanGeometry)
		}
		s.Values = append(s.Values, values)
	}
	var color [3]uint8
	if in.FaceColors != nil {
		color = in.FaceColors[p.face]
	}
	out.Triangles = append(out.Triangles, ids)
	result.Faces = append(result.Faces, source)
	result.CornerNormals = append(result.CornerNormals, normals)
	result.SourceNormals = append(result.SourceNormals, in.Mesh.Normals != nil)
	result.ColorsPresent = append(result.ColorsPresent, in.FaceColors != nil)
	result.FaceColors = append(result.FaceColors, color)
	return nil
}
