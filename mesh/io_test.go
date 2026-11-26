package mesh

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

// analyticSphere is the Marching Cubes surface of a sphere of radius r mm on
// a 0.5 mm grid.
func analyticSphere(t *testing.T, r float64) *Mesh {
	t.Helper()
	n := int(2*r/0.5) + 5
	centre := float64(n-1) / 2 * 0.5
	field := &FuncField{Size: [3]int{n, n, n}, Affine: GridAffine(render.Vec3{X: 10, Y: -20, Z: 5}, IdentityAxes, [3]float64{0.5, 0.5, 0.5}),
		Value: func(x, y, z int) float64 {
			dx, dy, dz := float64(x)*0.5-centre, float64(y)*0.5-centre, float64(z)*0.5-centre
			return r - math.Sqrt(dx*dx+dy*dy+dz*dz)
		}}
	m, err := Extract(context.Background(), field, Options{Iso: 0})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMeshAreaVolumeAndClosure(t *testing.T) {
	const r = 8.0
	sphere := analyticSphere(t, r)
	if v, want := sphere.Volume(), 4.0/3*math.Pi*r*r*r; !relClose(v, want, 0.01) {
		t.Fatalf("sphere volume %.2f mm³, analytic %.2f", v, want)
	}
	if a, want := sphere.Area(), 4*math.Pi*r*r; !relClose(a, want, 0.01) {
		t.Fatalf("sphere area %.2f mm², analytic %.2f", a, want)
	}
	if c := sphere.CheckClosure(); !c.Closed() {
		t.Fatalf("sphere closure %+v", c)
	}
	open := &Mesh{Vertices: sphere.Vertices, Triangles: sphere.Triangles[1:]}
	if c := open.CheckClosure(); c.Closed() || c.BoundaryEdges != 3 {
		t.Fatalf("open sphere closure %+v", c)
	}
	flipped := &Mesh{Vertices: sphere.Vertices, Triangles: append([][3]uint32{{sphere.Triangles[0][0], sphere.Triangles[0][2], sphere.Triangles[0][1]}}, sphere.Triangles[1:]...)}
	if c := flipped.CheckClosure(); c.InconsistentEdges != 3 {
		t.Fatalf("flipped triangle closure %+v", c)
	}
}

func TestWriteSTLPLYAndOBJ(t *testing.T) {
	sphere := analyticSphere(t, 4)
	var stl bytes.Buffer
	if err := WriteSTL(&stl, sphere, "dicom-go test"); err != nil {
		t.Fatal(err)
	}
	data := stl.Bytes()
	count := int(binary.LittleEndian.Uint32(data[80:]))
	if count != len(sphere.Triangles) || len(data) != 84+50*count || !strings.HasPrefix(string(data[:80]), "dicom-go test") {
		t.Fatalf("STL: %d bytes, %d triangles", len(data), count)
	}
	// Read the STL back: its signed volume is the mesh's, and every stored
	// normal agrees with the winding.
	var volume float64
	for i := 0; i < count; i++ {
		record := data[84+50*i:]
		f := func(k int) float64 { return float64(math.Float32frombits(binary.LittleEndian.Uint32(record[4*k:]))) }
		n := [3]float64{f(0), f(1), f(2)}
		a, b, c := [3]float64{f(3), f(4), f(5)}, [3]float64{f(6), f(7), f(8)}, [3]float64{f(9), f(10), f(11)}
		volume += dot3(a, cross3(b, c)) / 6
		if w := cross3(sub3(b, a), sub3(c, a)); dot3(w, n) <= 0 && dot3(w, w) > 0 {
			t.Fatalf("STL normal %d against the winding", i)
		}
	}
	if !relClose(volume, sphere.Volume(), 1e-5) {
		t.Fatalf("STL volume %.3f, mesh %.3f", volume, sphere.Volume())
	}

	colors := make([][3]uint8, len(sphere.Triangles))
	for i := range colors {
		colors[i] = [3]uint8{200, 10, uint8(i)}
	}
	var ply bytes.Buffer
	if err := WritePLY(&ply, sphere, PLYOptions{FaceColors: colors, Comments: []string{"segment Liver\nlabel 1"}}); err != nil {
		t.Fatal(err)
	}
	end := bytes.Index(ply.Bytes(), []byte("end_header\n")) + len("end_header\n")
	header := string(ply.Bytes()[:end])
	for _, want := range []string{"format binary_little_endian 1.0", "comment units mm", "comment segment Liver label 1",
		"element vertex " + strconv.Itoa(len(sphere.Vertices)), "property float nx", "element face " + strconv.Itoa(len(sphere.Triangles)),
		"property list uchar int vertex_indices", "property uchar red"} {
		if !strings.Contains(header, want) {
			t.Fatalf("PLY header lacks %q:\n%s", want, header)
		}
	}
	body := ply.Bytes()[end:]
	if len(body) != 24*len(sphere.Vertices)+16*len(sphere.Triangles) {
		t.Fatalf("PLY body %d bytes", len(body))
	}
	last := body[24*len(sphere.Vertices)+16*(len(sphere.Triangles)-1):]
	if last[0] != 3 || binary.LittleEndian.Uint32(last[1:]) != sphere.Triangles[len(sphere.Triangles)-1][0] || last[13] != 200 || last[15] != uint8(len(sphere.Triangles)-1) {
		t.Fatalf("last PLY face % x", last[:16])
	}
	if err := WritePLY(&ply, sphere, PLYOptions{FaceColors: colors[1:]}); err == nil {
		t.Fatal("mismatched face colours accepted")
	}

	var obj bytes.Buffer
	if err := WriteOBJ(&obj, sphere, "sphere"); err != nil {
		t.Fatal(err)
	}
	var vertices [][3]float64
	var faces [][3]int
	scanner := bufio.NewScanner(&obj)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		switch {
		case len(fields) == 4 && fields[0] == "v":
			var p [3]float64
			for k := range p {
				p[k], _ = strconv.ParseFloat(fields[k+1], 64)
			}
			vertices = append(vertices, p)
		case len(fields) == 4 && fields[0] == "f":
			var f [3]int
			for k := range f {
				f[k], _ = strconv.Atoi(strings.Split(fields[k+1], "//")[0])
			}
			faces = append(faces, f)
		}
	}
	volume = 0
	for _, f := range faces {
		volume += dot3(vertices[f[0]-1], cross3(vertices[f[1]-1], vertices[f[2]-1])) / 6
	}
	if len(vertices) != len(sphere.Vertices) || len(faces) != len(sphere.Triangles) || !relClose(volume, sphere.Volume(), 1e-5) {
		t.Fatalf("OBJ: %d vertices, %d faces, volume %.3f", len(vertices), len(faces), volume)
	}

	merged, faceColors := Merge([]*Mesh{sphere, sphere}, [][3]uint8{{1, 2, 3}, {4, 5, 6}})
	if len(merged.Triangles) != 2*len(sphere.Triangles) || len(faceColors) != len(merged.Triangles) || faceColors[len(faceColors)-1] != [3]uint8{4, 5, 6} || merged.Validate() != nil {
		t.Fatal("merge lost triangles or colours")
	}
}
