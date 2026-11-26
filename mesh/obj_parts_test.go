package mesh

import (
	"bytes"
	"math"
	"strconv"
	"strings"
	"testing"
)

// TestWriteOBJPartsAndMTL: each part reads back from its group with its own
// vertices, normals and volume, and uses a material the library defines in
// its colour.
func TestWriteOBJPartsAndMTL(t *testing.T) {
	sphere := analyticSphere(t, 4)
	moved := &Mesh{Triangles: sphere.Triangles}
	for _, v := range sphere.Vertices {
		moved.Vertices = append(moved.Vertices, [3]float32{v[0] + 30, v[1], v[2]})
	}
	parts := []Part{{Name: "Left kidney", Mesh: sphere, Color: [3]uint8{255, 0, 51}}, {Name: "Tumour/2", Mesh: moved, Color: [3]uint8{0, 128, 255}}}
	var obj, mtl bytes.Buffer
	if err := WriteOBJParts(&obj, parts, "segments.mtl", "two segments"); err != nil {
		t.Fatal(err)
	}
	if err := WriteMTL(&mtl, parts); err != nil {
		t.Fatal(err)
	}
	if OBJMaterialName(0, "Left kidney") != "part0_Left_kidney" || OBJMaterialName(1, "Tumour/2") != "part1_Tumour_2" || OBJMaterialName(2, "") != "part2" {
		t.Fatal("material names")
	}
	materials := map[string][3]float64{}
	var current string
	for _, l := range strings.Split(mtl.String(), "\n") {
		fields := strings.Fields(l)
		switch {
		case len(fields) == 2 && fields[0] == "newmtl":
			current = fields[1]
		case len(fields) == 4 && fields[0] == "Kd":
			var c [3]float64
			for k := range c {
				c[k], _ = strconv.ParseFloat(fields[k+1], 64)
			}
			materials[current] = c
		}
	}
	if len(materials) != 2 || materials["part0_Left_kidney"] != [3]float64{1, 0, 0.2} || math.Abs(materials["part1_Tumour_2"][1]-0.502) > 1e-3 {
		t.Fatalf("materials %v", materials)
	}
	var vertices, normals [][3]float64
	groups := map[string][][3]int{}
	used := map[string]string{}
	group := ""
	normalCorners := 0
	for _, l := range strings.Split(obj.String(), "\n") {
		fields := strings.Fields(l)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "mtllib":
			if fields[1] != "segments.mtl" {
				t.Fatalf("mtllib %q", fields[1])
			}
		case "g":
			group = fields[1]
		case "usemtl":
			used[group] = fields[1]
		case "v", "vn":
			var p [3]float64
			for k := range p {
				p[k], _ = strconv.ParseFloat(fields[k+1], 64)
			}
			if fields[0] == "v" {
				vertices = append(vertices, p)
			} else {
				normals = append(normals, p)
			}
		case "f":
			var f [3]int
			for k := range f {
				pair := strings.Split(fields[k+1], "//")
				f[k], _ = strconv.Atoi(pair[0])
				if len(pair) == 2 {
					normalCorners++
					if n, _ := strconv.Atoi(pair[1]); n < 1 || n > len(normals) {
						t.Fatalf("face normal %d of %d", n, len(normals))
					}
				}
			}
			groups[group] = append(groups[group], f)
		}
	}
	if len(normals) != len(sphere.Vertices) || normalCorners != 3*len(sphere.Triangles) {
		t.Fatalf("%d normals, %d face corners with normals", len(normals), normalCorners)
	}
	for i, part := range parts {
		name := OBJMaterialName(i, part.Name)
		if used[name] != name {
			t.Fatalf("group %s uses %q", name, used[name])
		}
		var volume float64
		for _, f := range groups[name] {
			volume += dot3(vertices[f[0]-1], cross3(vertices[f[1]-1], vertices[f[2]-1])) / 6
		}
		if len(groups[name]) != len(part.Mesh.Triangles) || !relClose(volume, part.Mesh.Volume(), 1e-5) {
			t.Fatalf("group %s: %d faces, volume %.3f, want %.3f", name, len(groups[name]), volume, part.Mesh.Volume())
		}
	}
	if err := WriteOBJParts(&obj, nil, "", ""); err == nil {
		t.Fatal("OBJ without parts accepted")
	}
}
