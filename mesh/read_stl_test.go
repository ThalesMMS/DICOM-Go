package mesh

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"testing"
)

// binarySTLFixture writes two facets of a unit tetrahedron corner as binary
// STL with the given header.
func binarySTLFixture(t *testing.T, header string) []byte {
	t.Helper()
	var encoded bytes.Buffer
	source := &Mesh{Vertices: [][3]float32{{0, 0, 0}, {2, 0, 0}, {0, 3, 0}, {0, 0, 4}}, Triangles: [][3]uint32{{0, 2, 1}, {0, 1, 3}}}
	if err := WriteSTL(&encoded, source, header); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

// A facet normal is not used, so a NaN or infinite one no longer refuses
// the file, binary or ASCII.
func TestReadSTLIgnoresNonFiniteFacetNormals(t *testing.T) {
	o := ReadOptions{MillimetresPerUnit: 1}
	data := binarySTLFixture(t, "binary")
	for _, bad := range []float32{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))} {
		broken := append([]byte(nil), data...)
		for axis := 0; axis < 3; axis++ {
			binary.LittleEndian.PutUint32(broken[84+axis*4:], math.Float32bits(bad))
		}
		m, err := ReadSTL(nil, bytes.NewReader(broken), o)
		if err != nil {
			t.Fatalf("normal %v: %v", bad, err)
		}
		if len(m.Triangles) != 2 || m.Normals != nil || m.Vertices[1] != [3]float32{0, 3, 0} {
			t.Fatalf("normal %v: %+v", bad, m)
		}
	}
	for _, normal := range []string{"nan nan nan", "NaN 0 inf", "-Inf 1e999 0"} {
		text := strings.Replace(asciiSTLFixture, "facet normal 0 0 1", "facet normal "+normal, 1)
		m, err := ReadSTL(nil, strings.NewReader(text), o)
		if err != nil || len(m.Triangles) != 1 || m.Area() != 3 {
			t.Fatalf("ASCII normal %q: %v", normal, err)
		}
	}
	// The normal must still be three numbers.
	for _, normal := range []string{"x 0 1", "0 1"} {
		text := strings.Replace(asciiSTLFixture, "facet normal 0 0 1", "facet normal "+normal, 1)
		if _, err := ReadSTL(nil, strings.NewReader(text), o); !errors.Is(err, ErrInvalidModel) {
			t.Fatalf("ASCII normal %q accepted: %v", normal, err)
		}
	}
}

// A binary file whose triangle count disagrees with its length is read by
// its length, as VTK does, with the read limits applied to that count.
func TestReadSTLReadsBinaryByLength(t *testing.T) {
	o := ReadOptions{MillimetresPerUnit: 1}
	for _, header := range []string{"binary", "solid but binary"} {
		data := binarySTLFixture(t, header)
		for _, count := range []uint32{0, 1, 3, math.MaxUint32} {
			wrong := append([]byte(nil), data...)
			binary.LittleEndian.PutUint32(wrong[80:84], count)
			m, err := ReadSTL(nil, bytes.NewReader(wrong), o)
			if err != nil {
				t.Fatalf("header %q, count %d: %v", header, count, err)
			}
			if len(m.Triangles) != 2 || len(m.Vertices) != 6 || m.Vertices[5] != [3]float32{0, 0, 4} {
				t.Fatalf("header %q, count %d: %d triangles", header, count, len(m.Triangles))
			}
			small := o
			small.MaxTriangles = 1
			if _, err := ReadSTL(nil, bytes.NewReader(wrong), small); !errors.Is(err, ErrModelLimit) {
				t.Fatalf("header %q, count %d: triangle limit ignored: %v", header, count, err)
			}
			small = o
			small.MaxVertices = 5
			if _, err := ReadSTL(nil, bytes.NewReader(wrong), small); !errors.Is(err, ErrModelLimit) {
				t.Fatalf("header %q, count %d: vertex limit ignored: %v", header, count, err)
			}
		}
		// A length that is not a whole number of facets is not binary.
		if _, err := ReadSTL(nil, bytes.NewReader(data[:len(data)-1]), o); !errors.Is(err, ErrInvalidModel) {
			t.Fatalf("header %q: truncated binary accepted: %v", header, err)
		}
		if _, err := ReadSTL(nil, bytes.NewReader(append(append([]byte(nil), data...), 0)), o); !errors.Is(err, ErrInvalidModel) {
			t.Fatalf("header %q: padded binary accepted: %v", header, err)
		}
	}
	// The header and count alone: no facets.
	if _, err := ReadSTL(nil, bytes.NewReader(binarySTLFixture(t, "binary")[:84]), o); !errors.Is(err, ErrInvalidModel) {
		t.Fatalf("empty binary accepted: %v", err)
	}
}

// An ASCII file whose length happens to be 84 plus a multiple of 50 is
// still ASCII, valid or not: text never holds a NUL, binary facets do.
func TestReadSTLKeepsSizedASCIIAsText(t *testing.T) {
	o := ReadOptions{MillimetresPerUnit: 1}
	pad := func(text string) string {
		for (len(text)-84)%50 != 0 || len(text) < 84 {
			text += "\n"
		}
		return text
	}
	text := pad(asciiSTLFixture)
	m, err := ReadSTL(nil, strings.NewReader(text), o)
	if err != nil || len(m.Triangles) != 1 || m.Area() != 3 {
		t.Fatalf("sized ASCII: %v", err)
	}
	// Two facets, so that the length allows more than one binary facet.
	two := strings.Replace(asciiSTLFixture, "endsolid", "facet normal 0 0 1\nouter loop\nvertex 0 0 1\nvertex 2 0 1\nvertex 0 3 1\nendloop\nendfacet\nendsolid", 1)
	m, err = ReadSTL(nil, strings.NewReader(pad(two)), o)
	if err != nil || len(m.Triangles) != 2 {
		t.Fatalf("sized two-facet ASCII: %v", err)
	}
	// Malformed ASCII of a binary-looking length is refused, not read as
	// binary garbage.
	for _, broken := range []string{
		pad(strings.Replace(asciiSTLFixture, "endloop", "endlop", 1)),
		pad(strings.Replace(asciiSTLFixture, "solid synthetic", "synthetic", 1)),
		pad(strings.Replace(asciiSTLFixture, "endsolid synthetic", "", 1)),
	} {
		if (len(broken)-84)%50 != 0 {
			t.Fatal("fixture not sized")
		}
		if m, err := ReadSTL(nil, strings.NewReader(broken), o); !errors.Is(err, ErrInvalidModel) || m != nil {
			t.Fatalf("malformed sized ASCII read: %v", err)
		}
	}
}
