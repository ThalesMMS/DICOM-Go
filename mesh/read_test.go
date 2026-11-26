package mesh

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"testing"
)

const asciiSTLFixture = `solid synthetic
facet normal 0 0 1
outer loop
vertex 0 0 0
vertex 2 0 0
vertex 0 3 0
endloop
endfacet
endsolid synthetic
`

func TestReadSTLASCIIAndBinaryPreserveExplicitPhysicalScale(t *testing.T) {
	ascii, err := ReadSTL(context.Background(), strings.NewReader(asciiSTLFixture), ReadOptions{MillimetresPerUnit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(ascii.Triangles) != 1 || ascii.Vertices[1][0] != 20 || ascii.Vertices[2][1] != 30 || ascii.Area() != 300 {
		t.Fatal("physical scale lost", ascii)
	}
	var encoded bytes.Buffer
	source := &Mesh{Vertices: [][3]float32{{0, 0, 0}, {2, 0, 0}, {0, 3, 0}}, Triangles: [][3]uint32{{0, 1, 2}}}
	if err := WriteSTL(&encoded, source, "solid misleading binary header"); err != nil {
		t.Fatal(err)
	}
	parsed, err := ReadSTL(context.Background(), bytes.NewReader(encoded.Bytes()), ReadOptions{MillimetresPerUnit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Area() != ascii.Area() || parsed.Vertices[1] != ascii.Vertices[1] {
		t.Fatal("binary/ASCII geometry differ")
	}
}
func TestReadSTLRefusesAbsentUnitsTruncationNonfiniteAndLimits(t *testing.T) {
	valid := ReadOptions{MillimetresPerUnit: 1}
	if _, err := ReadSTL(nil, strings.NewReader(asciiSTLFixture), ReadOptions{}); !errors.Is(err, ErrInvalidModel) {
		t.Fatal("units guessed", err)
	}
	for _, text := range []string{strings.ReplaceAll(asciiSTLFixture, "endsolid synthetic", ""), strings.ReplaceAll(asciiSTLFixture, "vertex 2 0 0", "vertex NaN 0 0"), strings.ReplaceAll(asciiSTLFixture, "vertex 2 0 0", "vertex 0 0 0"), asciiSTLFixture + "junk"} {
		if model, err := ReadSTL(nil, strings.NewReader(text), valid); err == nil || model != nil {
			t.Fatal("invalid STL published", err)
		}
	}
	small := valid
	small.MaxInputBytes = 10
	if _, err := ReadSTL(nil, strings.NewReader(asciiSTLFixture), small); !errors.Is(err, ErrModelLimit) {
		t.Fatal("input not bounded", err)
	}
	small = valid
	small.MaxVertices = 2
	if _, err := ReadSTL(nil, strings.NewReader(asciiSTLFixture), small); !errors.Is(err, ErrModelLimit) {
		t.Fatal("vertices not bounded", err)
	}
	small = valid
	small.MaxOutputBytes = 20
	if _, err := ReadSTL(nil, strings.NewReader(asciiSTLFixture), small); !errors.Is(err, ErrModelLimit) {
		t.Fatal("output not bounded", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadSTL(ctx, strings.NewReader(asciiSTLFixture), valid); !errors.Is(err, context.Canceled) {
		t.Fatal("cancel ignored", err)
	}
	var encoded bytes.Buffer
	_ = WriteSTL(&encoded, &Mesh{Vertices: [][3]float32{{0, 0, 0}, {1, 0, 0}, {0, 1, 0}}, Triangles: [][3]uint32{{0, 1, 2}}}, "binary")
	broken := append([]byte(nil), encoded.Bytes()...)
	binary.LittleEndian.PutUint32(broken[96:100], math.Float32bits(float32(math.Inf(1))))
	if _, err := ReadSTL(nil, bytes.NewReader(broken), valid); !errors.Is(err, ErrInvalidModel) {
		t.Fatal("infinite binary coordinate accepted", err)
	}
	if _, err := ReadSTL(nil, bytes.NewReader(encoded.Bytes()[:100]), valid); !errors.Is(err, ErrInvalidModel) {
		t.Fatal("truncated binary accepted", err)
	}
}

func TestReadMTLRejectsConflictingOpacityDeclarations(t *testing.T) {
	_, err := ReadMTL(context.Background(), strings.NewReader("newmtl red\nd 1\nTr 0.5\n"), ReadOptions{})
	if err == nil {
		t.Fatal("conflicting opacity declarations accepted")
	}
}
