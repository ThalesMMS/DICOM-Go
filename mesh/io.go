package mesh

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"strconv"
)

// Mesh measurements and file export (#1015), after VTK's vtkMassProperties,
// vtkSTLWriter, vtkPLYWriter and vtkOBJWriter. Coordinates are written in
// patient millimetres, the unit 3D-printing and modelling tools assume for
// STL. Written from the file formats' specifications; no VTK code is copied.

// Area is the total triangle area in mm².
func (m *Mesh) Area() float64 {
	if m == nil {
		return 0
	}
	var area float64
	for _, t := range m.Triangles {
		a, b, c := m.point(t[0]), m.point(t[1]), m.point(t[2])
		n := cross3(sub3(b, a), sub3(c, a))
		area += math.Sqrt(dot3(n, n)) / 2
	}
	return area
}

// Volume is the enclosed volume in mm³ by the divergence theorem: exact for
// a closed, consistently wound mesh, positive when its triangles wind
// counter-clockwise seen from outside.
func (m *Mesh) Volume() float64 {
	if m == nil {
		return 0
	}
	var volume float64
	for _, t := range m.Triangles {
		a, b, c := m.point(t[0]), m.point(t[1]), m.point(t[2])
		volume += dot3(a, cross3(b, c)) / 6
	}
	return volume
}

func (m *Mesh) point(i uint32) [3]float64 {
	v := m.Vertices[i]
	return [3]float64{float64(v[0]), float64(v[1]), float64(v[2])}
}

// Closure describes whether a mesh bounds a solid.
type Closure struct {
	// BoundaryEdges are used by one triangle, NonManifoldEdges by more than
	// two, and InconsistentEdges by two triangles in the same direction.
	BoundaryEdges, NonManifoldEdges, InconsistentEdges int
}

// Closed reports a watertight, consistently wound mesh.
func (c Closure) Closed() bool {
	return c.BoundaryEdges == 0 && c.NonManifoldEdges == 0 && c.InconsistentEdges == 0
}

// CheckClosure counts the edges that keep m from bounding a solid.
func (m *Mesh) CheckClosure() Closure {
	var c Closure
	if m == nil {
		return c
	}
	type uses struct{ forward, backward int }
	edges := make(map[[2]uint32]*uses, 3*len(m.Triangles)/2)
	for _, t := range m.Triangles {
		for i := 0; i < 3; i++ {
			a, b := t[i], t[(i+1)%3]
			key := [2]uint32{min(a, b), max(a, b)}
			u := edges[key]
			if u == nil {
				u = &uses{}
				edges[key] = u
			}
			if a < b {
				u.forward++
			} else {
				u.backward++
			}
		}
	}
	for _, u := range edges {
		switch total := u.forward + u.backward; {
		case total == 1:
			c.BoundaryEdges++
		case total > 2:
			c.NonManifoldEdges++
		case u.forward != 1:
			c.InconsistentEdges++
		}
	}
	return c
}

// triangleNormal is the unit right-hand normal of triangle t (zero when
// degenerate).
func (m *Mesh) triangleNormal(t [3]uint32) [3]float32 {
	a, b, c := m.point(t[0]), m.point(t[1]), m.point(t[2])
	return unit32(cross3(sub3(b, a), sub3(c, a)))
}

// WriteSTL writes m as binary STL: an 80-byte header, the triangle count and
// per triangle its unit normal, three corners (float32, mm) and a zero
// attribute word.
func WriteSTL(w io.Writer, m *Mesh, header string) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if uint64(len(m.Triangles)) > math.MaxUint32 {
		return fmt.Errorf("%w: %d triangles exceed STL's count", ErrInvalidField, len(m.Triangles))
	}
	out := bufio.NewWriterSize(w, 1<<16)
	var head [84]byte
	copy(head[:80], header)
	binary.LittleEndian.PutUint32(head[80:], uint32(len(m.Triangles)))
	if _, err := out.Write(head[:]); err != nil {
		return err
	}
	var record [50]byte
	for _, t := range m.Triangles {
		n := m.triangleNormal(t)
		values := [12]float32{n[0], n[1], n[2]}
		for k := 0; k < 3; k++ {
			v := m.Vertices[t[k]]
			values[3+3*k], values[4+3*k], values[5+3*k] = v[0], v[1], v[2]
		}
		for i, value := range values {
			binary.LittleEndian.PutUint32(record[4*i:], math.Float32bits(value))
		}
		if _, err := out.Write(record[:]); err != nil {
			return err
		}
	}
	return out.Flush()
}

// PLYOptions configures WritePLY.
type PLYOptions struct {
	// FaceColors, when set, holds an sRGB colour per triangle (the colour of
	// its label for segment surfaces).
	FaceColors [][3]uint8
	// Comments are written as header comments.
	Comments []string
	// VertexScalars adds up to MaxVertexScalarAttributes double properties
	// per vertex. Units, when set, are written as header comments. All names,
	// lengths and values are validated before any output is written.
	VertexScalars []VertexScalarAttribute
}

// WritePLY writes m as binary little-endian PLY: float vertices (mm) with
// their normals when m has them, and triangles as vertex-index lists with an
// optional per-face colour.
func WritePLY(w io.Writer, m *Mesh, opts PLYOptions) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if opts.FaceColors != nil && len(opts.FaceColors) != len(m.Triangles) {
		return fmt.Errorf("%w: %d face colours for %d triangles", ErrInvalidField, len(opts.FaceColors), len(m.Triangles))
	}
	if err := validateVertexScalars(opts.VertexScalars, len(m.Vertices), false); err != nil {
		return err
	}
	out := bufio.NewWriterSize(w, 1<<16)
	header := "ply\nformat binary_little_endian 1.0\ncomment units mm\n"
	for _, comment := range opts.Comments {
		header += "comment " + sanitizeHeaderLine(comment) + "\n"
	}
	for _, attribute := range opts.VertexScalars {
		if attribute.Units != "" {
			header += "comment vertex_scalar " + attribute.Name + " units " + attribute.Units + "\n"
		}
	}
	header += "element vertex " + strconv.Itoa(len(m.Vertices)) + "\nproperty float x\nproperty float y\nproperty float z\n"
	normals := m.Normals != nil
	if normals {
		header += "property float nx\nproperty float ny\nproperty float nz\n"
	}
	for _, attribute := range opts.VertexScalars {
		header += "property double " + attribute.Name + "\n"
	}
	header += "element face " + strconv.Itoa(len(m.Triangles)) + "\nproperty list uchar int vertex_indices\n"
	if opts.FaceColors != nil {
		header += "property uchar red\nproperty uchar green\nproperty uchar blue\n"
	}
	header += "end_header\n"
	if _, err := out.WriteString(header); err != nil {
		return err
	}
	var buf [24]byte
	for i, v := range m.Vertices {
		values := []float32{v[0], v[1], v[2]}
		if normals {
			n := m.Normals[i]
			values = append(values, n[0], n[1], n[2])
		}
		for k, value := range values {
			binary.LittleEndian.PutUint32(buf[4*k:], math.Float32bits(value))
		}
		if _, err := out.Write(buf[:4*len(values)]); err != nil {
			return err
		}
		var scalar [8]byte
		for _, attribute := range opts.VertexScalars {
			binary.LittleEndian.PutUint64(scalar[:], math.Float64bits(attribute.Values[i]))
			if _, err := out.Write(scalar[:]); err != nil {
				return err
			}
		}
	}
	var face [16]byte
	for i, t := range m.Triangles {
		face[0] = 3
		for k := 0; k < 3; k++ {
			binary.LittleEndian.PutUint32(face[1+4*k:], t[k])
		}
		size := 13
		if opts.FaceColors != nil {
			c := opts.FaceColors[i]
			face[13], face[14], face[15] = c[0], c[1], c[2]
			size = 16
		}
		if _, err := out.Write(face[:size]); err != nil {
			return err
		}
	}
	return out.Flush()
}

// WriteOBJ writes m as Wavefront OBJ text: vertices (mm), their normals when
// m has them, and 1-based triangle faces.
func WriteOBJ(w io.Writer, m *Mesh, comment string) error {
	if err := m.Validate(); err != nil {
		return err
	}
	out := bufio.NewWriterSize(w, 1<<16)
	line := func(prefix string, v [3]float32) {
		out.WriteString(prefix)
		for _, c := range v {
			out.WriteByte(' ')
			out.WriteString(strconv.FormatFloat(float64(c), 'g', -1, 32))
		}
		out.WriteByte('\n')
	}
	out.WriteString("# units mm\n")
	if comment != "" {
		out.WriteString("# " + sanitizeHeaderLine(comment) + "\n")
	}
	for _, v := range m.Vertices {
		line("v", v)
	}
	for _, n := range m.Normals {
		line("vn", n)
	}
	for _, t := range m.Triangles {
		if m.Normals != nil {
			fmt.Fprintf(out, "f %d//%d %d//%d %d//%d\n", t[0]+1, t[0]+1, t[1]+1, t[1]+1, t[2]+1, t[2]+1)
		} else {
			fmt.Fprintf(out, "f %d %d %d\n", t[0]+1, t[1]+1, t[2]+1)
		}
	}
	return out.Flush()
}

// WriteOBJParts writes parts as one Wavefront OBJ: per part a group ("g")
// and a material ("usemtl", see OBJMaterialName) from the library mtlName
// (written by WriteMTL), its vertices (mm), its normals when it has them,
// and its 1-based triangle faces.
func WriteOBJParts(w io.Writer, parts []Part, mtlName, comment string) error {
	if len(parts) == 0 {
		return fmt.Errorf("%w: OBJ export without parts", ErrInvalidField)
	}
	for i, part := range parts {
		if err := part.Mesh.Validate(); err != nil {
			return fmt.Errorf("OBJ part %d: %w", i, err)
		}
	}
	out := bufio.NewWriterSize(w, 1<<16)
	line := func(prefix string, v [3]float32) {
		out.WriteString(prefix)
		for _, c := range v {
			out.WriteByte(' ')
			out.WriteString(strconv.FormatFloat(float64(c), 'g', -1, 32))
		}
		out.WriteByte('\n')
	}
	out.WriteString("# units mm\n")
	if comment != "" {
		out.WriteString("# " + sanitizeHeaderLine(comment) + "\n")
	}
	if mtlName != "" {
		out.WriteString("mtllib " + sanitizeHeaderLine(mtlName) + "\n")
	}
	var vertexBase, normalBase uint32
	for i, part := range parts {
		m := part.Mesh
		name := OBJMaterialName(i, part.Name)
		out.WriteString("g " + name + "\n")
		out.WriteString("usemtl " + name + "\n")
		for _, v := range m.Vertices {
			line("v", v)
		}
		for _, n := range m.Normals {
			line("vn", n)
		}
		for _, t := range m.Triangles {
			a, b, c := vertexBase+t[0]+1, vertexBase+t[1]+1, vertexBase+t[2]+1
			if m.Normals != nil {
				na, nb, nc := normalBase+t[0]+1, normalBase+t[1]+1, normalBase+t[2]+1
				fmt.Fprintf(out, "f %d//%d %d//%d %d//%d\n", a, na, b, nb, c, nc)
			} else {
				fmt.Fprintf(out, "f %d %d %d\n", a, b, c)
			}
		}
		vertexBase += uint32(len(m.Vertices))
		normalBase += uint32(len(m.Normals))
	}
	return out.Flush()
}

// WriteMTL writes the Wavefront material library of parts: one opaque
// diffuse material per part in its sRGB colour, named as WriteOBJParts
// uses them.
func WriteMTL(w io.Writer, parts []Part) error {
	out := bufio.NewWriter(w)
	out.WriteString("# dicom-go mesh materials\n")
	channel := func(c uint8) string { return strconv.FormatFloat(float64(c)/255, 'f', 4, 64) }
	for i, part := range parts {
		c := part.Color
		fmt.Fprintf(out, "\nnewmtl %s\nKa 0 0 0\nKd %s %s %s\nKs 0 0 0\nd 1\nillum 1\n",
			OBJMaterialName(i, part.Name), channel(c[0]), channel(c[1]), channel(c[2]))
	}
	return out.Flush()
}

// OBJMaterialName is the group and material name of part i: "part<i>",
// then the part's name with every character other than an ASCII letter,
// digit, '-' or '_' replaced by '_' (OBJ names end at white space).
func OBJMaterialName(i int, name string) string {
	out := []rune("part" + strconv.Itoa(i))
	if name != "" {
		out = append(out, '_')
	}
	for _, r := range name {
		if !(r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			r = '_'
		}
		out = append(out, r)
	}
	return string(out)
}

func sanitizeHeaderLine(s string) string {
	out := []rune{}
	for _, r := range s {
		if r == '\n' || r == '\r' {
			r = ' '
		}
		out = append(out, r)
	}
	return string(out)
}

// Merge concatenates meshes into one, with face colours when colors holds
// one sRGB colour per mesh (the PLY export of segment surfaces).
func Merge(meshes []*Mesh, colors [][3]uint8) (*Mesh, [][3]uint8) {
	out := &Mesh{}
	var faces [][3]uint8
	normals := len(meshes) > 0
	for _, m := range meshes {
		if m != nil && m.Normals == nil && len(m.Vertices) > 0 {
			normals = false
		}
	}
	for i, m := range meshes {
		if m == nil {
			continue
		}
		base := uint32(len(out.Vertices))
		out.Vertices = append(out.Vertices, m.Vertices...)
		if normals {
			out.Normals = append(out.Normals, m.Normals...)
		}
		for _, t := range m.Triangles {
			out.Triangles = append(out.Triangles, [3]uint32{t[0] + base, t[1] + base, t[2] + base})
			if colors != nil {
				faces = append(faces, colors[i])
			}
		}
	}
	return out, faces
}
