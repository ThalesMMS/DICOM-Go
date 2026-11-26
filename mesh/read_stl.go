package mesh

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ReadSTL accepts binary or ASCII STL and scales its coordinates into mm.
//
// A binary file is recognised by its length: 84 bytes of header and
// triangle count, then 50 bytes per facet. When the length matches the
// stored count the file is binary, even if its header starts with "solid"
// (no text can match: its count bytes would ask for gigabytes). When the
// count disagrees but the length is a whole number of facets, the file is
// read as binary with the count its length gives, unless it could be text
// (valid UTF-8 without NUL bytes), which is read as ASCII: writers that
// leave a wrong count, or a "solid" header, are common, as VTK's reader
// also assumes, while an ASCII file never contains a NUL and real binary
// facets do. The read limits apply to the count the length gives.
//
// Each facet's stored normal is ignored, in both encodings, and may be
// non-finite: the Mesh keeps no facet normals, the winding of the vertices
// defines the face, and Normals is left nil. Attribute bytes are accepted
// as opaque metadata; colour extensions are not guessed.
func ReadSTL(ctx context.Context, r io.Reader, options ReadOptions) (*Mesh, error) {
	o, err := options.normalized(true)
	if err != nil {
		return nil, err
	}
	data, err := modelInput(ctx, r, o)
	if err != nil {
		return nil, err
	}
	if len(data) >= 84 && (len(data)-84)%50 == 0 {
		stored := uint64(binary.LittleEndian.Uint32(data[80:84]))
		if stored == uint64((len(data)-84)/50) || !stlText(data) {
			return readBinarySTL(ctx, data, o)
		}
	}
	return readASCIISTL(ctx, data, o)
}

// stlText reports whether data could be an ASCII STL: valid UTF-8 without
// NUL bytes.
func stlText(data []byte) bool {
	return utf8.Valid(data) && bytes.IndexByte(data, 0) < 0
}

// readBinarySTL reads the facets that fill data after its 84-byte header;
// the stored triangle count is not used.
func readBinarySTL(ctx context.Context, data []byte, o ReadOptions) (*Mesh, error) {
	count := (len(data) - 84) / 50
	if count < 1 {
		return nil, fmt.Errorf("%w: empty STL", ErrInvalidModel)
	}
	if count > o.MaxTriangles || int64(count)*3 > int64(o.MaxVertices) || int64(count)*52 > o.MaxOutputBytes {
		return nil, ErrModelLimit
	}
	m := &Mesh{Vertices: make([][3]float32, 0, count*3), Triangles: make([][3]uint32, 0, count)}
	for i := 0; i < count; i++ {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		// The facet normal (bytes 0-11) is not used; see ReadSTL.
		facet := data[84+i*50 : 84+(i+1)*50]
		base := uint32(len(m.Vertices))
		for vertex := 0; vertex < 3; vertex++ {
			var point [3]float32
			for axis := 0; axis < 3; axis++ {
				v := float64(math.Float32frombits(binary.LittleEndian.Uint32(facet[12+vertex*12+axis*4:]))) * o.MillimetresPerUnit
				if !finiteModel(v) || math.Abs(v) > math.MaxFloat32 {
					return nil, fmt.Errorf("%w: non-finite STL coordinate", ErrInvalidModel)
				}
				point[axis] = float32(v)
			}
			if err := modelVertex(m, point, o); err != nil {
				return nil, err
			}
		}
		if err := modelTriangle(m, [3]uint32{base, base + 1, base + 2}, o); err != nil {
			return nil, err
		}
	}
	return m, nil
}
func readASCIISTL(ctx context.Context, data []byte, o ReadOptions) (*Mesh, error) {
	if !stlText(data) {
		return nil, fmt.Errorf("%w: invalid STL bytes/length", ErrInvalidModel)
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, min(1024, o.MaxLineBytes+2)), o.MaxLineBytes+2)
	m := &Mesh{}
	state := 0
	ended := false
	var corners [3][3]float32
	vertex := 0
	for scanner.Scan() {
		if len(scanner.Bytes()) > o.MaxLineBytes {
			return nil, ErrModelLimit
		}
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		switch state {
		case 0:
			if f[0] != "solid" {
				return nil, fmt.Errorf("%w: missing STL solid", ErrInvalidModel)
			}
			if len(line) > o.MaxNameBytes+6 {
				return nil, ErrModelLimit
			}
			state = 1
		case 1:
			if f[0] == "endsolid" {
				ended = true
				state = 6
				continue
			}
			if len(f) != 5 || f[0] != "facet" || f[1] != "normal" {
				return nil, fmt.Errorf("%w: expected facet normal", ErrInvalidModel)
			}
			// The normal must be three numbers, but is not used and may be
			// non-finite ("nan", "inf" or out of range); see ReadSTL.
			for _, raw := range f[2:] {
				if _, err := strconv.ParseFloat(raw, 64); err != nil && !errors.Is(err, strconv.ErrRange) {
					return nil, fmt.Errorf("%w: invalid STL normal %q", ErrInvalidModel, raw)
				}
			}
			state = 2
		case 2:
			if len(f) != 2 || f[0] != "outer" || f[1] != "loop" {
				return nil, fmt.Errorf("%w: expected outer loop", ErrInvalidModel)
			}
			vertex = 0
			state = 3
		case 3:
			if len(f) != 4 || f[0] != "vertex" {
				return nil, fmt.Errorf("%w: expected vertex", ErrInvalidModel)
			}
			point, err := modelPoint(f[1:], o.MillimetresPerUnit)
			if err != nil {
				return nil, err
			}
			corners[vertex] = point
			vertex++
			if vertex == 3 {
				state = 4
			}
		case 4:
			if len(f) != 1 || f[0] != "endloop" {
				return nil, fmt.Errorf("%w: expected endloop", ErrInvalidModel)
			}
			state = 5
		case 5:
			if len(f) != 1 || f[0] != "endfacet" {
				return nil, fmt.Errorf("%w: expected endfacet", ErrInvalidModel)
			}
			base := uint32(len(m.Vertices))
			for _, point := range corners {
				if err := modelVertex(m, point, o); err != nil {
					return nil, err
				}
			}
			if err := modelTriangle(m, [3]uint32{base, base + 1, base + 2}, o); err != nil {
				return nil, err
			}
			state = 1
		default:
			return nil, fmt.Errorf("%w: trailing STL content", ErrInvalidModel)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: STL line exceeds capacity", ErrModelLimit)
	}
	if !ended || len(m.Triangles) == 0 {
		return nil, fmt.Errorf("%w: truncated/empty STL", ErrInvalidModel)
	}
	return m, nil
}
