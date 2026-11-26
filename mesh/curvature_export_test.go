package mesh

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func curvatureExportAttributes(c *CurvatureResult) []VertexScalarAttribute {
	flags := make([]float64, len(c.Flags))
	for i, v := range c.Flags {
		flags[i] = float64(v)
	}
	return []VertexScalarAttribute{{"CURVATURE_MEAN", c.Mean, "mm^-1"}, {"CURVATURE_GAUSSIAN", c.Gaussian, "mm^-2"}, {"CURVATURE_MIN", c.PrincipalMin, "mm^-1"}, {"CURVATURE_MAX", c.PrincipalMax, "mm^-1"}, {"MIXED_AREA", c.MixedArea, "mm^2"}, {"CURVATURE_FLAGS", flags, ""}}
}

// Parse the PLY header and records independently of the writer layout.
func readCurvaturePLY(t *testing.T, data []byte) map[string][]float64 {
	t.Helper()
	marker := []byte("end_header\n")
	at := bytes.Index(data, marker)
	if at < 0 {
		t.Fatal("no PLY header end")
	}
	header := string(data[:at+len(marker)])
	data = data[at+len(marker):]
	if !strings.HasPrefix(header, "ply\nformat binary_little_endian 1.0\n") {
		t.Fatal("PLY format")
	}
	type property struct {
		name string
		size int
	}
	var properties []property
	vertices, faces := 0, 0
	vertexElement := false
	for _, line := range strings.Split(header, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "element" {
			vertexElement = fields[1] == "vertex"
			count, e := strconv.Atoi(fields[2])
			if e != nil {
				t.Fatal(e)
			}
			if vertexElement {
				vertices = count
			} else if fields[1] == "face" {
				faces = count
			}
		}
		if fields[0] == "property" && vertexElement {
			size := 0
			switch fields[1] {
			case "float":
				size = 4
			case "double":
				size = 8
			default:
				t.Fatalf("unexpected PLY scalar %s", line)
			}
			properties = append(properties, property{fields[2], size})
		}
	}
	got := make(map[string][]float64)
	for _, p := range properties {
		got[p.name] = make([]float64, vertices)
	}
	for i := 0; i < vertices; i++ {
		for _, p := range properties {
			if len(data) < p.size {
				t.Fatal("truncated PLY vertex")
			}
			if p.size == 4 {
				got[p.name][i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(data)))
			} else {
				got[p.name][i] = math.Float64frombits(binary.LittleEndian.Uint64(data))
			}
			data = data[p.size:]
		}
	}
	for f := 0; f < faces; f++ {
		if len(data) < 13 || data[0] != 3 {
			t.Fatal("invalid PLY face record")
		}
		for k := 0; k < 3; k++ {
			if binary.LittleEndian.Uint32(data[1+4*k:]) >= uint32(vertices) {
				t.Fatal("PLY face index")
			}
		}
		data = data[13:]
	}
	if len(data) != 0 {
		t.Fatal("PLY unexpected trailing data")
	}
	return got
}

type curvatureGLBDoc struct {
	Accessors []struct {
		BufferView, ComponentType, Count int
		Type                             string
		Extras                           map[string]string
	}
	BufferViews []struct{ ByteOffset, ByteLength int64 }
	Buffers     []struct{ ByteLength int64 }
	Meshes      []struct {
		Primitives []struct{ Attributes map[string]int }
	}
}

func readCurvatureGLB(t *testing.T, data []byte) (curvatureGLBDoc, []byte) {
	t.Helper()
	var doc curvatureGLBDoc
	if len(data) < 28 || binary.LittleEndian.Uint32(data) != 0x46546c67 || binary.LittleEndian.Uint32(data[4:]) != 2 || int(binary.LittleEndian.Uint32(data[8:])) != len(data) {
		t.Fatal("GLB container")
	}
	jsonLength := int(binary.LittleEndian.Uint32(data[12:]))
	end := 20 + jsonLength
	if jsonLength%4 != 0 || end+8 > len(data) || binary.LittleEndian.Uint32(data[16:]) != 0x4e4f534a {
		t.Fatal("GLB JSON chunk")
	}
	if e := json.Unmarshal(data[20:end], &doc); e != nil {
		t.Fatal(e)
	}
	if binary.LittleEndian.Uint32(data[end+4:]) != 0x004e4942 || int(binary.LittleEndian.Uint32(data[end:])) != len(data)-end-8 {
		t.Fatal("GLB BIN chunk")
	}
	bin := data[end+8:]
	if len(doc.Buffers) != 1 || doc.Buffers[0].ByteLength != int64(len(bin)) {
		t.Fatal("GLB buffer length")
	}
	for _, view := range doc.BufferViews {
		if view.ByteOffset%4 != 0 || view.ByteOffset < 0 || view.ByteLength < 0 || view.ByteOffset+view.ByteLength > int64(len(bin)) {
			t.Fatal("GLB view alignment/range")
		}
	}
	return doc, bin
}

func TestGeneratedCurvaturePLYGLB(t *testing.T) {
	meshes := []*Mesh{curvatureSphere(20, 1), curvatureCylinder(10, 32, 5)}
	var parts []Part
	var all [][]VertexScalarAttribute
	for i, m := range meshes {
		c, e := VertexCurvatures(nil, m, CurvatureOptions{})
		if e != nil {
			t.Fatal(e)
		}
		attributes := curvatureExportAttributes(c)
		var out bytes.Buffer
		if e := WritePLY(&out, m, PLYOptions{VertexScalars: attributes}); e != nil {
			t.Fatal(e)
		}
		got := readCurvaturePLY(t, out.Bytes())
		for _, a := range attributes {
			if !reflect.DeepEqual(got[a.Name], a.Values) {
				t.Fatalf("PLY %s precision/order", a.Name)
			}
			if a.Units != "" && !bytes.Contains(out.Bytes(), []byte("comment vertex_scalar "+a.Name+" units "+a.Units+"\n")) {
				t.Fatal("PLY units")
			}
		}
		parts = append(parts, Part{Name: fmt.Sprintf("part%d", i), Mesh: m})
		all = append(all, attributes)
	}
	var out bytes.Buffer
	if e := WriteGLB(&out, parts, GLBOptions{VertexScalars: all}); e != nil {
		t.Fatal(e)
	}
	doc, bin := readCurvatureGLB(t, out.Bytes())
	if len(doc.Meshes) != 2 {
		t.Fatal("GLB part mapping")
	}
	for i, attributes := range all {
		for _, a := range attributes {
			accessorIndex, ok := doc.Meshes[i].Primitives[0].Attributes["_"+a.Name]
			if !ok {
				t.Fatal("GLB custom semantic")
			}
			accessor := doc.Accessors[accessorIndex]
			if accessor.ComponentType != 5126 || accessor.Type != "SCALAR" || accessor.Count != len(a.Values) || accessor.Extras["units"] != a.Units {
				t.Fatal("GLB accessor")
			}
			view := doc.BufferViews[accessor.BufferView]
			if view.ByteLength != 4*int64(len(a.Values)) {
				t.Fatal("GLB scalar view length")
			}
			for v, want := range a.Values {
				got := math.Float32frombits(binary.LittleEndian.Uint32(bin[view.ByteOffset+4*int64(v):]))
				if math.Float32bits(got) != math.Float32bits(float32(want)) {
					t.Fatalf("GLB %s vertex %d", a.Name, v)
				}
			}
		}
	}
}

func TestCurvatureExportInvalidPreflight(t *testing.T) {
	m := &Mesh{Vertices: [][3]float32{{0, 0, 0}, {1, 0, 0}, {0, 1, 0}}, Triangles: [][3]uint32{{0, 1, 2}}}
	for _, attributes := range [][]VertexScalarAttribute{{{Name: "x", Values: []float64{1, 2, 3}}}, {{Name: "MEAN", Values: []float64{1}}}, {{Name: "MEAN", Values: []float64{0, math.NaN(), 0}}}, {{Name: "MEAN\nproperty", Values: []float64{1, 2, 3}}}, {{Name: "MEAN", Values: []float64{1, 2, 3}, Units: "mm\nend_header"}}, make([]VertexScalarAttribute, 33)} {
		for _, glb := range []bool{false, true} {
			var out bytes.Buffer
			var e error
			if glb {
				e = WriteGLB(&out, []Part{{Mesh: m}}, GLBOptions{VertexScalars: [][]VertexScalarAttribute{attributes}})
			} else {
				e = WritePLY(&out, m, PLYOptions{VertexScalars: attributes})
			}
			if !errors.Is(e, ErrInvalidField) || out.Len() != 0 {
				t.Fatalf("preflight error/output %v %d", e, out.Len())
			}
		}
	}
	var out bytes.Buffer
	large := []VertexScalarAttribute{{Name: "LARGE", Values: []float64{math.MaxFloat64, 0, 0}}}
	if e := WriteGLB(&out, []Part{{Mesh: m}}, GLBOptions{VertexScalars: [][]VertexScalarAttribute{large}}); !errors.Is(e, ErrInvalidField) || out.Len() != 0 {
		t.Fatal("GLB conversion preflight")
	}
	if e := WritePLY(&out, m, PLYOptions{VertexScalars: large}); e != nil {
		t.Fatal(e)
	}
	if readCurvaturePLY(t, out.Bytes())["LARGE"][0] != math.MaxFloat64 {
		t.Fatal("PLY double representation")
	}
	out.Reset()
	if e := WriteGLB(&out, []Part{{Mesh: m}}, GLBOptions{VertexScalars: [][]VertexScalarAttribute{}}); !errors.Is(e, ErrInvalidField) || out.Len() != 0 {
		t.Fatal("GLB outer mapping preflight")
	}
	parts := make([]Part, 129)
	all := make([][]VertexScalarAttribute, 129)
	for i := range parts {
		parts[i].Mesh = m
		for a := 0; a < 32; a++ {
			all[i] = append(all[i], VertexScalarAttribute{Name: fmt.Sprintf("A%d", a), Values: []float64{0, 0, 0}})
		}
	}
	if e := WriteGLB(&out, parts, GLBOptions{VertexScalars: all}); !errors.Is(e, ErrInvalidField) || out.Len() != 0 {
		t.Fatal("GLB global count preflight")
	}
	want := errors.New("controlled writer error")
	if e := WritePLY(curvatureErrorWriter{want}, m, PLYOptions{VertexScalars: large}); !errors.Is(e, want) {
		t.Fatalf("PLY writer error %v", e)
	}
	if e := WriteGLB(curvatureErrorWriter{want}, []Part{{Mesh: m}}, GLBOptions{}); !errors.Is(e, want) {
		t.Fatalf("GLB writer error %v", e)
	}
}

type curvatureErrorWriter struct{ err error }

func (w curvatureErrorWriter) Write(p []byte) (int, error) { return 0, w.err }

func TestCurvatureExportDefaultsByteIdentical(t *testing.T) {
	// Captured by default-goldens.go before any source edit at published
	// 8fceb783. Include normals, comments, colours, two parts and both axes.
	want := map[string]string{
		"PLY normals=false extra=false":            "c26fd00c9e9d5ebd9d3f1d6821b852500149aaec9f3f3ce11eca734aae520c76",
		"GLB normals=false extra=false axes=false": "f5002ece41fda802644b0e4214e77b7462491b68b8de3739ef0e2de944e6bc93",
		"GLB normals=false extra=false axes=true":  "d0c70849bb352a2e34c1248abe99449aacc35f4b59195fa0580fc648ab7f5c87",
		"PLY normals=false extra=true":             "a47f4df564b1d47d1335768b7b4660c49b66af501b6859514bcc54295da1773f",
		"GLB normals=false extra=true axes=false":  "43a1e21991e1abc8eb11d92d57ea067fda2815159261fb4c95f3f94848e057fc",
		"GLB normals=false extra=true axes=true":   "3d18a30778aa50091fc9b1098251ca04057318decf3fd158a1bd083172824cbb",
		"PLY normals=true extra=false":             "70a9b3e5577ce18b511b5fa7e077da036580b4efb740d5f8860b9075ab762ff9",
		"GLB normals=true extra=false axes=false":  "e891e0a61a52ec32369abd36d47fe71c5b771ea862e8cf6641780c4f39469f38",
		"GLB normals=true extra=false axes=true":   "c08eef8a5e91592f72cfdd18077c30df085c659664a16b719e0eb8881ecf6fcf",
		"PLY normals=true extra=true":              "93ad9e1b19f6431b79a6de5b4de546a033e9405cc0f78e59c79f7861cef7f415",
		"GLB normals=true extra=true axes=false":   "8d0bfc69751aa03bc581573dc0c1096ea42a7f14cd9fbf3b53bae82286cc4705",
		"GLB normals=true extra=true axes=true":    "6bfcfd39e555f2887016bf6d305d8cca346ed8a3fce788bb722189cac5439139",
	}
	// Same published 8fceb783 renderer, captured on Windows/amd64 Go 1.26.4.
	// The platform math.Pow result changes JSON colour digits, not geometry.
	if runtime.GOOS == "windows" && runtime.GOARCH == "amd64" && runtime.Version() == "go1.26.4" {
		want["GLB normals=false extra=false axes=false"] = "9956d74f734d16f8d862d55aad0796bfac433e4ba5ce00120add5cd136a2b2bf"
		want["GLB normals=false extra=false axes=true"] = "bb248a2887bf0021684d27aa8e07e070b34f8a14aef6738525fffbcac02b67b2"
		want["GLB normals=false extra=true axes=false"] = "4705af48ef04b14863076b3500bd5db08f65dc08551f44c2f2bf7bd0f6da0a7c"
		want["GLB normals=false extra=true axes=true"] = "9914a8e810beeeaa82d2aa58804781125ce3c8ab460397c2fd64523e6385610d"
		want["GLB normals=true extra=false axes=false"] = "176c5c1ce886c67b7c4bd54ff2fddaf6940fe3ae77e58527d9fa32238d7841b9"
		want["GLB normals=true extra=false axes=true"] = "ea7dc484c7b7fa5ab29adecc81884306b116c3c21db8398b42a9632bed560d67"
		want["GLB normals=true extra=true axes=false"] = "2a22770be3c71f686e11812324e91747c00e547c0ea82f7d15b3a90772fe6c68"
		want["GLB normals=true extra=true axes=true"] = "af2445104c98b7a61e5756aefa92a8f59be6838a7c4b5be89ef12501a29499a7"
	}
	m := &Mesh{Vertices: [][3]float32{{1, 2, 3}, {4, 2, 3}, {1, 6, 3}}, Triangles: [][3]uint32{{0, 1, 2}}}
	check := func(key string, data []byte) {
		t.Helper()
		if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != want[key] {
			t.Fatalf("%s changed: %s", key, got)
		}
	}
	for _, normal := range []bool{false, true} {
		if normal {
			m.Normals = [][3]float32{{0, 0, 1}, {0, 0, 1}, {0, 0, 1}}
		}
		for _, extra := range []bool{false, true} {
			var p PLYOptions
			if extra {
				p.FaceColors = [][3]uint8{{4, 5, 6}}
				p.Comments = []string{"curvature default baseline"}
			}
			var b bytes.Buffer
			if e := WritePLY(&b, m, p); e != nil {
				t.Fatal(e)
			}
			check(fmt.Sprintf("PLY normals=%t extra=%t", normal, extra), b.Bytes())
			for _, axes := range []bool{false, true} {
				b.Reset()
				opts := GLBOptions{PatientAxes: axes}
				if extra {
					opts.Generator = "default baseline"
					opts.Comment = "curvature default baseline"
				}
				parts := []Part{{Name: "triangle", Mesh: m, Color: [3]uint8{3, 5, 7}}, {Name: "second", Mesh: m, Color: [3]uint8{11, 13, 17}}}
				if e := WriteGLB(&b, parts, opts); e != nil {
					t.Fatal(e)
				}
				check(fmt.Sprintf("GLB normals=%t extra=%t axes=%t", normal, extra, axes), b.Bytes())
			}
		}
	}
}
