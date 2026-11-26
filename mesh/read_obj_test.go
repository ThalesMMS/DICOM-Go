package mesh

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const objConcaveFixture = `mtllib ../opaque-name-only.mtl
v 0 0 0
v 2 0 0
v 2 2 0
v 1 1 0
v 0 2 0
vt 0 0
vn 0 0 1
usemtl Red
f -5/1/1 -4/1/1 -3/1/1 -2/1/1 -1/1/1
`

func TestReadOBJConcaveRelativeBindingsAndPhysicalScale(t *testing.T) {
	model, err := ReadOBJ(context.Background(), strings.NewReader(objConcaveFixture), ReadOptions{MillimetresPerUnit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(model.Mesh.Triangles) != 3 || model.Mesh.Area() != 300 || model.Mesh.Vertices[2][0] != 20 {
		t.Fatal("polygon/scale wrong", model.Mesh)
	}
	if len(model.MaterialNames) != 1 || model.MaterialNames[0] != "Red" || model.MaterialLibraries[0] != "../opaque-name-only.mtl" || len(model.TriangleNormals) != 3 || model.TriangleNormals[0] != [3]int32{0, 0, 0} {
		t.Fatal("declared bindings lost", model)
	}
}
func TestReadOBJRejectsInvalidTopologyIndicesAndUnboundedInput(t *testing.T) {
	o := ReadOptions{MillimetresPerUnit: 1}
	for _, text := range []string{
		"v 0 0 0\nv 1 0 0\nv 0 1 0\nf 0 2 3\n",
		"v 0 0 0\nv 1 0 0\nv 0 1 0\nf 1 2 4\n",
		"v NaN 0 0\nv 1 0 0\nv 0 1 0\nf 1 2 3\n",
		"v 0 0 0\nv 1 0 0\nv 1 1 1\nv 0 1 0\nf 1 2 3 4\n",
		"v 0 0 0\nv 2 2 0\nv 0 2 0\nv 2 0 0\nf 1 2 3 4\n",
		"v 0 0 0\nv 1 0 0\nv 0 1 0\nf 1/9 2/9 3/9\n",
	} {
		if model, err := ReadOBJ(nil, strings.NewReader(text), o); err == nil || model != nil {
			t.Fatal("bad geometry published", text, err)
		}
	}
	small := o
	small.MaxTriangles = 2
	if _, err := ReadOBJ(nil, strings.NewReader(objConcaveFixture), small); !errors.Is(err, ErrModelLimit) {
		t.Fatal("face output not bounded", err)
	}
	if _, err := ReadOBJ(nil, strings.NewReader("call remote.obj\n"), o); !errors.Is(err, ErrUnsupportedModel) {
		t.Fatal("external call accepted", err)
	}
}
func TestReadMTLInspectsDeclaredMaterialsWithoutLoadingResources(t *testing.T) {
	materials, err := ReadMTL(nil, strings.NewReader("newmtl Red\nKd 1 0 0\nKa 0.1 0.2 0.3\nNs 50\nNi 1.5\nTr 0.25\nillum 2\nmap_Kd https://invalid.example/never-opened.png\n"), ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(materials) != 1 || materials[0].Diffuse != [3]float32{1, 0, 0} || materials[0].Opacity != 0.75 || materials[0].TextureReferences[0].Arguments != "https://invalid.example/never-opened.png" {
		t.Fatal("materials changed", materials)
	}
	for _, text := range []string{"newmtl X\nKd NaN 0 0\n", "newmtl X\nd 2\n", "newmtl X\nnewmtl X\n", "Kd 1 0 0\n"} {
		if _, err := ReadMTL(nil, strings.NewReader(text), ReadOptions{}); err == nil {
			t.Fatal("invalid material accepted", text)
		}
	}
	if _, err := ReadMTL(nil, strings.NewReader("newmtl X\nnewmtl Y\n"), ReadOptions{MaxMaterials: 1}); !errors.Is(err, ErrModelLimit) {
		t.Fatal("material capacity not enforced", err)
	}
}

func TestModelReadersEnforceSmallLineAndOutputBudgets(t *testing.T) {
	o := ReadOptions{MillimetresPerUnit: 1, MaxLineBytes: 8}
	if _, err := ReadSTL(nil, strings.NewReader(asciiSTLFixture), o); !errors.Is(err, ErrModelLimit) {
		t.Fatal("STL line cap ignored", err)
	}
	if _, err := ReadOBJ(nil, strings.NewReader(objConcaveFixture), o); !errors.Is(err, ErrModelLimit) {
		t.Fatal("OBJ line cap ignored", err)
	}
	if _, err := ReadMTL(nil, strings.NewReader("newmtl material-name-too-long\n"), o); !errors.Is(err, ErrModelLimit) {
		t.Fatal("MTL line cap ignored", err)
	}
	if _, err := ReadOBJ(nil, strings.NewReader(objConcaveFixture), ReadOptions{MillimetresPerUnit: 1, MaxOutputBytes: 64}); !errors.Is(err, ErrModelLimit) {
		t.Fatal("OBJ output cap ignored", err)
	}
}

func TestReadOBJRejectsSelfTouchingBoundary(t *testing.T) {
	_, err := ReadOBJ(context.Background(), strings.NewReader("v 0 0 0\nv 4 0 0\nv 4 4 0\nv 2 0 0\nv 0 4 0\nf 1 2 3 4 5\n"), ReadOptions{MillimetresPerUnit: 1})
	if err == nil {
		t.Fatal("self-touching nonadjacent polygon edges accepted")
	}
}
