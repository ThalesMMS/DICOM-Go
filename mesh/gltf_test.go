package mesh

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// glbFile is a GLB parsed and checked against the glTF 2.0 rules a model
// writer can break (the Khronos validator's structural checks): container
// header and chunk layout, required JSON fields, index ranges, buffer-view
// and accessor bounds and alignment, exact POSITION bounds, unit normals,
// in-range triangle indices and a valid scene graph.
type glbFile struct {
	doc struct {
		Asset struct {
			Version   string `json:"version"`
			Generator string `json:"generator"`
			Extras    struct {
				Units   string `json:"units"`
				Comment string `json:"comment"`
			} `json:"extras"`
		} `json:"asset"`
		Scene  *int `json:"scene"`
		Scenes []struct {
			Nodes []int `json:"nodes"`
		} `json:"scenes"`
		Nodes []struct {
			Name     string    `json:"name"`
			Mesh     *int      `json:"mesh"`
			Children []int     `json:"children"`
			Rotation []float64 `json:"rotation"`
			Scale    []float64 `json:"scale"`
			Matrix   []float64 `json:"matrix"`
		} `json:"nodes"`
		Meshes []struct {
			Name       string `json:"name"`
			Primitives []struct {
				Attributes map[string]int `json:"attributes"`
				Indices    *int           `json:"indices"`
				Material   *int           `json:"material"`
				Mode       *int           `json:"mode"`
			} `json:"primitives"`
		} `json:"meshes"`
		Materials []struct {
			Name string `json:"name"`
			PBR  struct {
				BaseColorFactor []float64 `json:"baseColorFactor"`
				MetallicFactor  *float64  `json:"metallicFactor"`
				RoughnessFactor *float64  `json:"roughnessFactor"`
			} `json:"pbrMetallicRoughness"`
		} `json:"materials"`
		Accessors []struct {
			BufferView    *int      `json:"bufferView"`
			ByteOffset    int64     `json:"byteOffset"`
			ComponentType int       `json:"componentType"`
			Count         int       `json:"count"`
			Type          string    `json:"type"`
			Min           []float64 `json:"min"`
			Max           []float64 `json:"max"`
		} `json:"accessors"`
		BufferViews []struct {
			Buffer     int   `json:"buffer"`
			ByteOffset int64 `json:"byteOffset"`
			ByteLength int64 `json:"byteLength"`
			ByteStride int64 `json:"byteStride"`
			Target     int   `json:"target"`
		} `json:"bufferViews"`
		Buffers []struct {
			ByteLength int64  `json:"byteLength"`
			URI        string `json:"uri"`
		} `json:"buffers"`
	}
	bin []byte
}

func componentSize(t int) int64 {
	switch t {
	case 5120, 5121:
		return 1
	case 5122, 5123:
		return 2
	case 5125, 5126:
		return 4
	}
	return 0
}

func typeComponents(t string) int64 {
	return map[string]int64{"SCALAR": 1, "VEC2": 2, "VEC3": 3, "VEC4": 4, "MAT2": 4, "MAT3": 9, "MAT4": 16}[t]
}

// parseGLB validates data and returns it parsed.
func parseGLB(t *testing.T, data []byte) *glbFile {
	t.Helper()
	le := binary.LittleEndian
	if len(data) < 28 || le.Uint32(data) != glbMagic || le.Uint32(data[4:]) != 2 || int(le.Uint32(data[8:])) != len(data) {
		t.Fatalf("GLB header % x for %d bytes", data[:min(12, len(data))], len(data))
	}
	jsonLength := int(le.Uint32(data[12:]))
	if le.Uint32(data[16:]) != glbChunkJSON || jsonLength%4 != 0 || 20+jsonLength+8 > len(data) {
		t.Fatalf("GLB JSON chunk: type %x, %d bytes", le.Uint32(data[16:]), jsonLength)
	}
	text := data[20 : 20+jsonLength]
	if trimmed := bytes.TrimRight(text, " "); len(text)-len(trimmed) > 3 || bytes.ContainsAny(text[len(trimmed):], "\x00") {
		t.Fatalf("JSON chunk padding %q", text[len(trimmed):])
	}
	f := &glbFile{}
	decoder := json.NewDecoder(bytes.NewReader(text))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&f.doc); err != nil {
		t.Fatalf("glTF JSON: %v\n%s", err, text)
	}
	binStart := 20 + jsonLength
	binLength := int(le.Uint32(data[binStart:]))
	if le.Uint32(data[binStart+4:]) != glbChunkBIN || binLength%4 != 0 || binStart+8+binLength != len(data) {
		t.Fatalf("GLB BIN chunk: type %x, %d bytes, file %d", le.Uint32(data[binStart+4:]), binLength, len(data))
	}
	f.bin = data[binStart+8:]
	d := &f.doc

	if d.Asset.Version != "2.0" {
		t.Fatalf("asset.version %q", d.Asset.Version)
	}
	if len(d.Buffers) != 1 || d.Buffers[0].URI != "" || d.Buffers[0].ByteLength > int64(len(f.bin)) || int64(len(f.bin))-d.Buffers[0].ByteLength > 3 {
		t.Fatalf("buffers %+v for a %d-byte BIN chunk", d.Buffers, len(f.bin))
	}
	for i, v := range d.BufferViews {
		if v.Buffer != 0 || v.ByteOffset < 0 || v.ByteLength < 1 || v.ByteOffset+v.ByteLength > d.Buffers[0].ByteLength {
			t.Fatalf("bufferView %d %+v outside the buffer", i, v)
		}
		if v.ByteOffset%4 != 0 || (v.Target != gltfArrayBuffer && v.Target != gltfElementBuffer) {
			t.Fatalf("bufferView %d offset %d, target %d", i, v.ByteOffset, v.Target)
		}
	}
	for i, a := range d.Accessors {
		size, n := componentSize(a.ComponentType), typeComponents(a.Type)
		if a.BufferView == nil || *a.BufferView < 0 || *a.BufferView >= len(d.BufferViews) || size == 0 || n == 0 || a.Count < 1 {
			t.Fatalf("accessor %d %+v", i, a)
		}
		view := d.BufferViews[*a.BufferView]
		element := size * n
		stride := element
		if view.ByteStride != 0 {
			stride = view.ByteStride
		}
		if a.ByteOffset%size != 0 || (view.ByteOffset+a.ByteOffset)%size != 0 {
			t.Fatalf("accessor %d misaligned", i)
		}
		if a.ByteOffset+stride*int64(a.Count-1)+element > view.ByteLength {
			t.Fatalf("accessor %d (%d × %d bytes) overruns its %d-byte view", i, a.Count, element, view.ByteLength)
		}
		if (a.Min == nil) != (a.Max == nil) || (a.Min != nil && (int64(len(a.Min)) != n || int64(len(a.Max)) != n)) {
			t.Fatalf("accessor %d bounds %v %v", i, a.Min, a.Max)
		}
	}
	for i, m := range d.Materials {
		c := m.PBR.BaseColorFactor
		if len(c) != 4 || m.PBR.MetallicFactor == nil || m.PBR.RoughnessFactor == nil {
			t.Fatalf("material %d %+v", i, m)
		}
		for _, v := range c {
			if !(v >= 0 && v <= 1) {
				t.Fatalf("material %d colour %v", i, c)
			}
		}
	}
	for i, m := range d.Meshes {
		if len(m.Primitives) == 0 {
			t.Fatalf("mesh %d has no primitive", i)
		}
		for _, p := range m.Primitives {
			position, ok := p.Attributes["POSITION"]
			if !ok || p.Indices == nil || p.Material == nil || *p.Material >= len(d.Materials) || p.Mode == nil || *p.Mode != gltfTriangles {
				t.Fatalf("mesh %d primitive %+v", i, p)
			}
			vertices := f.vec3(t, position)
			pos := d.Accessors[position]
			if pos.ComponentType != gltfFloat || pos.Type != "VEC3" || d.BufferViews[*pos.BufferView].Target != gltfArrayBuffer || pos.Min == nil {
				t.Fatalf("mesh %d POSITION accessor %+v", i, pos)
			}
			lower, upper := vertexBounds(vertices)
			for k := 0; k < 3; k++ {
				if pos.Min[k] != lower[k] || pos.Max[k] != upper[k] {
					t.Fatalf("mesh %d POSITION bounds %v..%v, data %v..%v", i, pos.Min, pos.Max, lower, upper)
				}
			}
			for name, index := range p.Attributes {
				if d.Accessors[index].Count != pos.Count {
					t.Fatalf("mesh %d attribute %s count %d, POSITION %d", i, name, d.Accessors[index].Count, pos.Count)
				}
			}
			if normal, ok := p.Attributes["NORMAL"]; ok {
				for j, n := range f.vec3(t, normal) {
					if l := math.Sqrt(float64(n[0])*float64(n[0]) + float64(n[1])*float64(n[1]) + float64(n[2])*float64(n[2])); math.Abs(l-1) > 5e-4 {
						t.Fatalf("mesh %d normal %d has length %v", i, j, l)
					}
				}
			}
			ind := d.Accessors[*p.Indices]
			view := d.BufferViews[*ind.BufferView]
			if ind.Type != "SCALAR" || (ind.ComponentType != gltfUnsignedShort && ind.ComponentType != gltfUnsignedInt) || view.Target != gltfElementBuffer || view.ByteStride != 0 || ind.Count%3 != 0 {
				t.Fatalf("mesh %d indices %+v view %+v", i, ind, view)
			}
			restart := uint32(math.MaxUint32)
			if ind.ComponentType == gltfUnsignedShort {
				restart = math.MaxUint16
			}
			for _, index := range f.indices(*p.Indices) {
				if index >= uint32(pos.Count) || index == restart {
					t.Fatalf("mesh %d index %d of %d vertices", i, index, pos.Count)
				}
			}
		}
	}
	if d.Scene == nil || *d.Scene >= len(d.Scenes) {
		t.Fatalf("scene %v of %d", d.Scene, len(d.Scenes))
	}
	parent := make([]int, len(d.Nodes))
	for i := range parent {
		parent[i] = -1
	}
	for i, n := range d.Nodes {
		if n.Mesh != nil && *n.Mesh >= len(d.Meshes) {
			t.Fatalf("node %d mesh %d", i, *n.Mesh)
		}
		if n.Rotation != nil {
			q := n.Rotation
			if len(q) != 4 || math.Abs(q[0]*q[0]+q[1]*q[1]+q[2]*q[2]+q[3]*q[3]-1) > 5e-4 {
				t.Fatalf("node %d rotation %v is not a unit quaternion", i, q)
			}
		}
		if n.Matrix != nil && (n.Rotation != nil || n.Scale != nil) {
			t.Fatalf("node %d has both a matrix and TRS", i)
		}
		for _, c := range n.Children {
			if c <= 0 || c >= len(d.Nodes) || parent[c] >= 0 || c == i {
				t.Fatalf("node %d child %d", i, c)
			}
			parent[c] = i
		}
	}
	for _, root := range d.Scenes[*d.Scene].Nodes {
		if root >= len(d.Nodes) || parent[root] >= 0 {
			t.Fatalf("scene root %d", root)
		}
	}
	return f
}

func (f *glbFile) vec3(t *testing.T, accessor int) [][3]float32 {
	t.Helper()
	a := f.doc.Accessors[accessor]
	view := f.doc.BufferViews[*a.BufferView]
	data := f.bin[view.ByteOffset+a.ByteOffset:]
	out := make([][3]float32, a.Count)
	for i := range out {
		for k := 0; k < 3; k++ {
			out[i][k] = math.Float32frombits(binary.LittleEndian.Uint32(data[12*i+4*k:]))
		}
	}
	return out
}

func (f *glbFile) indices(accessor int) []uint32 {
	a := f.doc.Accessors[accessor]
	view := f.doc.BufferViews[*a.BufferView]
	data := f.bin[view.ByteOffset+a.ByteOffset:]
	out := make([]uint32, a.Count)
	for i := range out {
		if a.ComponentType == gltfUnsignedShort {
			out[i] = uint32(binary.LittleEndian.Uint16(data[2*i:]))
		} else {
			out[i] = binary.LittleEndian.Uint32(data[4*i:])
		}
	}
	return out
}

// part reads mesh i back.
func (f *glbFile) part(t *testing.T, i int) *Mesh {
	t.Helper()
	p := f.doc.Meshes[i].Primitives[0]
	m := &Mesh{Vertices: f.vec3(t, p.Attributes["POSITION"])}
	if n, ok := p.Attributes["NORMAL"]; ok {
		m.Normals = f.vec3(t, n)
	}
	flat := f.indices(*p.Indices)
	for k := 0; k+2 < len(flat); k += 3 {
		m.Triangles = append(m.Triangles, [3]uint32{flat[k], flat[k+1], flat[k+2]})
	}
	return m
}

// gridSheet is an open w×h vertex sheet, enough vertices for 32-bit
// indices when w·h exceeds 65535.
func gridSheet(w, h int) *Mesh {
	m := &Mesh{}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			m.Vertices = append(m.Vertices, [3]float32{float32(x) * 0.5, float32(y) * 0.5, float32(math.Sin(float64(x+y) / 7))})
		}
	}
	for y := 0; y+1 < h; y++ {
		for x := 0; x+1 < w; x++ {
			a := uint32(y*w + x)
			m.Triangles = append(m.Triangles, [3]uint32{a, a + 1, a + uint32(w) + 1}, [3]uint32{a, a + uint32(w) + 1, a + uint32(w)})
		}
	}
	return m
}

func TestWriteGLBIsAValidGLTF(t *testing.T) {
	sphere := analyticSphere(t, 4)
	// A part without normals and an odd triangle count (16-bit index
	// padding), and one past the 16-bit index range.
	small := &Mesh{Vertices: sphere.Vertices, Triangles: sphere.Triangles[:len(sphere.Triangles)-1]}
	if len(small.Triangles)%2 == 0 {
		small.Triangles = small.Triangles[:len(small.Triangles)-1]
	}
	sheet := gridSheet(300, 240)
	parts := []Part{
		{Name: "Liver", Mesh: sphere, Color: [3]uint8{221, 130, 101}},
		{Name: "Open part", Mesh: small, Color: [3]uint8{0, 255, 0}},
		{Name: "Sheet", Mesh: sheet, Color: [3]uint8{255, 255, 255}},
	}
	var out bytes.Buffer
	if err := WriteGLB(&out, parts, GLBOptions{Comment: "segment model\nlabel 1"}); err != nil {
		t.Fatal(err)
	}
	f := parseGLB(t, out.Bytes())
	d := &f.doc
	if d.Asset.Generator != "dicom-go mesh" || d.Asset.Extras.Comment != "segment model label 1" || !strings.Contains(d.Asset.Extras.Units, "millimetres") {
		t.Fatalf("asset %+v", d.Asset)
	}
	if len(d.Meshes) != 3 || len(d.Materials) != 3 || len(d.Nodes) != 4 || len(d.Nodes[0].Children) != 3 {
		t.Fatalf("%d meshes, %d materials, %d nodes", len(d.Meshes), len(d.Materials), len(d.Nodes))
	}
	root := d.Nodes[0]
	if root.Scale[0] != 0.001 || root.Rotation[0] >= 0 || root.Rotation[3] <= 0 {
		t.Fatalf("root node %+v", root)
	}
	wantTypes := []int{gltfUnsignedShort, gltfUnsignedShort, gltfUnsignedInt}
	for i, part := range parts {
		got := f.part(t, i)
		if d.Meshes[i].Name != part.Name || d.Materials[i].Name != part.Name || d.Nodes[i+1].Name != part.Name || *d.Nodes[i+1].Mesh != i {
			t.Fatalf("part %d names %q %q %q", i, d.Meshes[i].Name, d.Materials[i].Name, d.Nodes[i+1].Name)
		}
		if ct := d.Accessors[*d.Meshes[i].Primitives[0].Indices].ComponentType; ct != wantTypes[i] {
			t.Fatalf("part %d index type %d, want %d", i, ct, wantTypes[i])
		}
		if (got.Normals != nil) != (part.Mesh.Normals != nil) || len(got.Triangles) != len(part.Mesh.Triangles) {
			t.Fatalf("part %d: normals %v, %d triangles", i, got.Normals != nil, len(got.Triangles))
		}
		for k, v := range part.Mesh.Vertices {
			if got.Vertices[k] != v {
				t.Fatalf("part %d vertex %d %v, want the exact millimetres %v", i, k, got.Vertices[k], v)
			}
		}
		if got.Volume() != part.Mesh.Volume() || got.Area() != part.Mesh.Area() {
			t.Fatalf("part %d volume %v area %v, mesh %v %v", i, got.Volume(), got.Area(), part.Mesh.Volume(), part.Mesh.Area())
		}
	}
	if c := d.Materials[0].PBR.BaseColorFactor; math.Abs(c[0]-0.7230551) > 1e-6 || c[3] != 1 || *d.Materials[0].PBR.MetallicFactor != 0 {
		t.Fatalf("material 0 colour %v (sRGB 221 is linear 0.7231)", c)
	}
	// Superior goes to glTF +Y and anterior (-P) to +Z.
	q := root.Rotation
	rotate := func(v [3]float64) [3]float64 {
		u := [3]float64{q[0], q[1], q[2]}
		w := cross3(u, v)
		w = [3]float64{2 * w[0], 2 * w[1], 2 * w[2]}
		c := cross3(u, w)
		return [3]float64{v[0] + q[3]*w[0] + c[0], v[1] + q[3]*w[1] + c[1], v[2] + q[3]*w[2] + c[2]}
	}
	for _, axis := range []struct{ in, want [3]float64 }{{[3]float64{0, 0, 1}, [3]float64{0, 1, 0}}, {[3]float64{0, -1, 0}, [3]float64{0, 0, 1}}, {[3]float64{1, 0, 0}, [3]float64{1, 0, 0}}} {
		got := rotate(axis.in)
		for k := range got {
			if math.Abs(got[k]-axis.want[k]) > 1e-12 {
				t.Fatalf("root rotation takes %v to %v, want %v", axis.in, got, axis.want)
			}
		}
	}

	// Deterministic output.
	var again bytes.Buffer
	if err := WriteGLB(&again, parts, GLBOptions{Comment: "segment model\nlabel 1"}); err != nil || !bytes.Equal(again.Bytes(), out.Bytes()) {
		t.Fatalf("second write differs (%v)", err)
	}
	// Patient axes keep only the scale.
	again.Reset()
	if err := WriteGLB(&again, parts[:1], GLBOptions{PatientAxes: true, Generator: "Twin-Viewer"}); err != nil {
		t.Fatal(err)
	}
	patient := parseGLB(t, again.Bytes())
	if patient.doc.Nodes[0].Rotation != nil || patient.doc.Asset.Generator != "Twin-Viewer" {
		t.Fatalf("patient-axes root %+v", patient.doc.Nodes[0])
	}
}

func TestWriteGLBDropsDegenerateNormals(t *testing.T) {
	sphere := analyticSphere(t, 3)
	broken := &Mesh{Vertices: sphere.Vertices, Normals: append([][3]float32(nil), sphere.Normals...), Triangles: sphere.Triangles}
	broken.Normals[0] = [3]float32{}
	var out bytes.Buffer
	if err := WriteGLB(&out, []Part{{Mesh: broken}}, GLBOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := parseGLB(t, out.Bytes()).doc.Meshes[0].Primitives[0].Attributes["NORMAL"]; ok {
		t.Fatal("a zero normal was written as a glTF NORMAL")
	}
}

func TestWriteGLBRejectsInvalidParts(t *testing.T) {
	sphere := analyticSphere(t, 3)
	for name, parts := range map[string][]Part{
		"none":      nil,
		"nil mesh":  {{Name: "x"}},
		"empty":     {{Mesh: &Mesh{Vertices: sphere.Vertices}}},
		"bad index": {{Mesh: &Mesh{Vertices: sphere.Vertices[:2], Triangles: [][3]uint32{{0, 1, 2}}}}},
	} {
		var out bytes.Buffer
		if err := WriteGLB(&out, parts, GLBOptions{}); err == nil {
			t.Errorf("%s: accepted", name)
		} else if name != "bad index" && !errors.Is(err, ErrInvalidField) {
			t.Errorf("%s: %v", name, err)
		}
		if out.Len() != 0 {
			t.Errorf("%s: wrote %d bytes before failing", name, out.Len())
		}
	}
}

// TestWriteGLBPassesTheKhronosValidator runs the Khronos glTF-Validator
// command line (gltf_validator) when DICOMGO_GLTF_VALIDATOR names it; the
// report must hold no error.
func TestWriteGLBPassesTheKhronosValidator(t *testing.T) {
	validator := os.Getenv("DICOMGO_GLTF_VALIDATOR")
	if validator == "" {
		t.Skip("set DICOMGO_GLTF_VALIDATOR to the gltf_validator executable")
	}
	sphere := analyticSphere(t, 4)
	path := filepath.Join(t.TempDir(), "model.glb")
	var out bytes.Buffer
	if err := WriteGLB(&out, []Part{{Name: "sphere", Mesh: sphere, Color: [3]uint8{200, 80, 60}}, {Name: "sheet", Mesh: gridSheet(300, 240)}}, GLBOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := exec.Command(validator, "-o", path).Output()
	if err != nil {
		t.Fatalf("%s: %v\n%s", validator, err, report)
	}
	var parsed struct {
		Issues struct {
			NumErrors   int `json:"numErrors"`
			NumWarnings int `json:"numWarnings"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(report, &parsed); err != nil {
		t.Fatalf("validator report: %v\n%s", err, report)
	}
	if parsed.Issues.NumErrors != 0 {
		t.Fatalf("glTF-Validator found %d errors:\n%s", parsed.Issues.NumErrors, report)
	}
}

func BenchmarkWriteGLB(b *testing.B) {
	sheet := gridSheet(1000, 1000)
	var out bytes.Buffer
	out.Grow(40 << 20)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out.Reset()
		if err := WriteGLB(&out, []Part{{Name: "sheet", Mesh: sheet}}, GLBOptions{}); err != nil {
			b.Fatal(err)
		}
	}
	b.SetBytes(int64(out.Len()))
}
