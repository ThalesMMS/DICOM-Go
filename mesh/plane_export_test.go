package mesh

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// Independent PLY decoder for this fixture: verify header counts/properties,
// exact coordinate/scalar/color/index payload, winding/volume and EOF. It does
// not call the clipping or triangulation implementation to reconstruct faces.
func plane1497ReadPLY(t *testing.T, data []byte, r *PlaneClipResult) *Mesh {
	t.Helper()
	at := bytes.Index(data, []byte("end_header\n"))
	if at < 0 {
		t.Fatal("header")
	}
	header := string(data[:at])
	data = data[at+len("end_header\n"):]
	if !strings.Contains(header, "format binary_little_endian 1.0") || !strings.Contains(header, "element vertex "+strconv.Itoa(len(r.Mesh.Vertices))) || !strings.Contains(header, "element face "+strconv.Itoa(len(r.Mesh.Triangles))) || !strings.Contains(header, "property double LINEAR") || !strings.Contains(header, "derived source synthetic:cube") {
		t.Fatal(header)
	}
	take := func(n int) []byte {
		if len(data) < n {
			t.Fatal("truncated")
		}
		b := data[:n]
		data = data[n:]
		return b
	}
	out := &Mesh{}
	for i := range r.Mesh.Vertices {
		var p [3]float32
		for a := range p {
			p[a] = math.Float32frombits(binary.LittleEndian.Uint32(take(4)))
		}
		out.Vertices = append(out.Vertices, p)
		s := math.Float64frombits(binary.LittleEndian.Uint64(take(8)))
		if s != r.VertexScalars[0].Values[i] {
			t.Fatal("scalar payload")
		}
	}
	for i := range r.Mesh.Triangles {
		if take(1)[0] != 3 {
			t.Fatal("face arity")
		}
		var face [3]uint32
		for j := range face {
			face[j] = binary.LittleEndian.Uint32(take(4))
			if int(face[j]) >= len(out.Vertices) {
				t.Fatal("index")
			}
		}
		color := take(3)
		if !bytes.Equal(color, r.FaceColors[i][:]) {
			t.Fatal("face color")
		}
		out.Triangles = append(out.Triangles, face)
	}
	if len(data) != 0 {
		t.Fatal("extra payload")
	}
	return out
}
func TestPlaneClipExportRoundTripAndAtomicFailure(t *testing.T) {
	m := plane1497Box([3]float32{-1, -1, -1}, [3]float32{1, 1, 1})
	v := make([]float64, 8)
	colors := make([][3]uint8, 12)
	for i, p := range m.Vertices {
		v[i] = float64(p[0]) + float64(p[1]) + float64(p[2])
	}
	for i := range colors {
		colors[i] = [3]uint8{uint8(i), 44, 55}
	}
	r, e := ClipClosedByPlane(context.Background(), m, Plane{Normal: [3]float64{0, 0, 1}}, PlaneOptions{KeepPositive: true, SourceReference: "synthetic:cube", VertexScalars: []VertexScalarAttribute{{"LINEAR", v, "mm"}}, FaceColors: colors, CapColor: [3]uint8{255, 0, 0}})
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "cut.ply")
	if e = r.WritePLYFile(context.Background(), path, 0); e != nil {
		t.Fatal(e)
	}
	data, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	out := plane1497ReadPLY(t, data, r)
	if !reflect.DeepEqual(out, r.Mesh) {
		t.Fatal("geometry roundtrip")
	}
	requireClosedManifold(t, out)
	if out.Volume() != r.Mesh.Volume() || out.Area() != r.Mesh.Area() {
		t.Fatal("mass roundtrip")
	}
	// An independent OBJ reader checks indexed geometry too.
	var obj bytes.Buffer
	if e = WriteOBJ(&obj, r.Mesh, "plane test"); e != nil {
		t.Fatal(e)
	}
	om := &Mesh{}
	for _, line := range strings.Split(obj.String(), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "v":
			var p [3]float32
			for a := range p {
				x, err := strconv.ParseFloat(f[a+1], 32)
				if err != nil {
					t.Fatal(err)
				}
				p[a] = float32(x)
			}
			om.Vertices = append(om.Vertices, p)
		case "f":
			var face [3]uint32
			for a := range face {
				id, err := strconv.Atoi(strings.Split(f[a+1], "/")[0])
				if err != nil || id <= 0 {
					t.Fatal("OBJ index")
				}
				face[a] = uint32(id - 1)
			}
			om.Triangles = append(om.Triangles, face)
		}
	}
	if !reflect.DeepEqual(om, r.Mesh) {
		t.Fatal("OBJ geometry")
	}
	for _, tc := range []struct {
		name  string
		ctx   context.Context
		limit int64
		want  error
	}{{"quota", context.Background(), 16, ErrBudgetExceeded}, {"cancel", &plane1497Cancel{Context: context.Background(), after: 3}, 0, context.Canceled}} {
		t.Run(tc.name, func(t *testing.T) {
			e := r.WritePLYFile(tc.ctx, path, tc.limit)
			if !errors.Is(e, tc.want) {
				t.Fatalf("%v", e)
			}
			now, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(now, data) {
				t.Fatal("destination changed on failure")
			}
			files, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".mesh-plane-*"))
			if err != nil || len(files) != 0 {
				t.Fatal("temporary leaked")
			}
		})
	}
	if e = r.WritePLYFile(context.Background(), filepath.Join(path, "invalid"), 0); e == nil {
		t.Fatal("disk error")
	}
}
