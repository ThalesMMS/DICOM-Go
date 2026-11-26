package mesh

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// OBJModel preserves polygon geometry and declared normal/texture/material
// indices. No mtllib or texture name is opened by this reader.
type OBJModel struct {
	Mesh                             *Mesh
	Normals                          [][3]float32
	TextureCoordinates               [][3]float32
	TriangleNormals                  [][3]int32
	TriangleTextures                 [][3]int32
	TriangleMaterials                []int32
	MaterialNames, MaterialLibraries []string
}
type objCorner struct {
	vertex          uint32
	normal, texture int32
}

func objIndex(text string, count int) (int32, error) {
	n, err := strconv.ParseInt(text, 10, 32)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("%w: invalid OBJ index", ErrInvalidModel)
	}
	if n < 0 {
		n = int64(count) + n
	} else {
		n--
	}
	if n < 0 || n >= int64(count) {
		return 0, fmt.Errorf("%w: OBJ index outside preceding vertices", ErrInvalidModel)
	}
	return int32(n), nil
}

// ReadOBJ accepts polygonal OBJ, including relative indices and independent
// v/vt/vn bindings. Planar simple polygons are triangulated by ear clipping;
// unsupported free-form/point/line primitives are refused, not discarded.
func ReadOBJ(ctx context.Context, r io.Reader, options ReadOptions) (*OBJModel, error) {
	o, err := options.normalized(true)
	if err != nil {
		return nil, err
	}
	data, err := modelInput(ctx, r, o)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil, fmt.Errorf("%w: OBJ must be UTF-8 text", ErrInvalidModel)
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, min(1024, o.MaxLineBytes+2)), o.MaxLineBytes+2)
	out := &OBJModel{Mesh: &Mesh{}}
	material := int32(-1)
	names := map[string]int32{}
	lineNumber := 0
	continuation := ""
	memory := func() bool {
		return out.Mesh.Bytes()+int64(len(out.Normals)+len(out.TextureCoordinates))*12+int64(len(out.Mesh.Triangles))*28+int64(len(out.MaterialNames)+len(out.MaterialLibraries))*int64(o.MaxNameBytes+16) <= o.MaxOutputBytes
	}
	for scanner.Scan() {
		if len(scanner.Bytes()) > o.MaxLineBytes {
			return nil, ErrModelLimit
		}
		lineNumber++
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		line := strings.TrimSpace(scanner.Text())
		if pos := strings.IndexByte(line, '#'); pos >= 0 {
			line = strings.TrimSpace(line[:pos])
		}
		if len(continuation)+len(line) > o.MaxLineBytes {
			return nil, ErrModelLimit
		}
		line = continuation + line
		continuation = ""
		if strings.HasSuffix(line, "\\") {
			continuation = strings.TrimSuffix(line, "\\") + " "
			continue
		}
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		invalid := func() error { return fmt.Errorf("%w: OBJ line %d (%s)", ErrInvalidModel, lineNumber, f[0]) }
		switch f[0] {
		case "v":
			if len(f) != 4 && len(f) != 5 {
				return nil, invalid()
			}
			values := f[1:4]
			scale := o.MillimetresPerUnit
			if len(f) == 5 {
				weight, err := modelNumber(f[4])
				if err != nil || weight == 0 {
					return nil, invalid()
				}
				scale /= weight
			}
			point, err := modelPoint(values, scale)
			if err != nil {
				return nil, err
			}
			if err := modelVertex(out.Mesh, point, o); err != nil {
				return nil, err
			}
		case "vn":
			point, err := modelPoint(f[1:], 1)
			if err != nil {
				return nil, err
			}
			if point == [3]float32{} {
				return nil, invalid()
			}
			if len(out.Normals) >= o.MaxVertices {
				return nil, ErrModelLimit
			}
			out.Normals = append(out.Normals, point)
		case "vt":
			if len(f) < 2 || len(f) > 4 {
				return nil, invalid()
			}
			raw := []string{"0", "0", "0"}
			copy(raw, f[1:])
			point, err := modelPoint(raw, 1)
			if err != nil {
				return nil, err
			}
			if len(out.TextureCoordinates) >= o.MaxVertices {
				return nil, ErrModelLimit
			}
			out.TextureCoordinates = append(out.TextureCoordinates, point)
		case "f":
			if len(f) < 4 || len(f)-1 > o.MaxFaceVertices {
				return nil, invalid()
			}
			corners := make([]objCorner, len(f)-1)
			for i, token := range f[1:] {
				parts := strings.Split(token, "/")
				if len(parts) > 3 || parts[0] == "" {
					return nil, invalid()
				}
				vertex, err := objIndex(parts[0], len(out.Mesh.Vertices))
				if err != nil {
					return nil, err
				}
				c := objCorner{vertex: uint32(vertex), normal: -1, texture: -1}
				if len(parts) > 1 && parts[1] != "" {
					c.texture, err = objIndex(parts[1], len(out.TextureCoordinates))
					if err != nil {
						return nil, err
					}
				}
				if len(parts) > 2 {
					if parts[2] == "" {
						return nil, invalid()
					}
					c.normal, err = objIndex(parts[2], len(out.Normals))
					if err != nil {
						return nil, err
					}
				}
				corners[i] = c
			}
			triangles, err := triangulateOBJ(ctx, out.Mesh, corners)
			if err != nil {
				return nil, err
			}
			for _, indices := range triangles {
				a, b, c := corners[indices[0]], corners[indices[1]], corners[indices[2]]
				if err := modelTriangle(out.Mesh, [3]uint32{a.vertex, b.vertex, c.vertex}, o); err != nil {
					return nil, err
				}
				out.TriangleNormals = append(out.TriangleNormals, [3]int32{a.normal, b.normal, c.normal})
				out.TriangleTextures = append(out.TriangleTextures, [3]int32{a.texture, b.texture, c.texture})
				out.TriangleMaterials = append(out.TriangleMaterials, material)
			}
		case "usemtl":
			name, err := modelName(strings.TrimSpace(strings.TrimPrefix(line, "usemtl")), o)
			if err != nil {
				return nil, err
			}
			index, found := names[name]
			if !found {
				if len(names) >= o.MaxMaterials {
					return nil, ErrModelLimit
				}
				index = int32(len(out.MaterialNames))
				names[name] = index
				out.MaterialNames = append(out.MaterialNames, name)
			}
			material = index
		case "mtllib":
			if len(f) < 2 {
				return nil, invalid()
			}
			for _, raw := range f[1:] {
				name, err := modelName(raw, o)
				if err != nil {
					return nil, err
				}
				if len(out.MaterialLibraries) >= o.MaxMaterials {
					return nil, ErrModelLimit
				}
				out.MaterialLibraries = append(out.MaterialLibraries, name)
			}
		case "o", "g":
			if len(line) > o.MaxNameBytes+2 {
				return nil, ErrModelLimit
			}
		case "s":
			if len(f) != 2 {
				return nil, invalid()
			}
			if f[1] != "off" && f[1] != "on" {
				if _, err := strconv.ParseUint(f[1], 10, 32); err != nil {
					return nil, invalid()
				}
			}
		default:
			return nil, fmt.Errorf("%w: OBJ directive %s", ErrUnsupportedModel, f[0])
		}
		if !memory() {
			return nil, ErrModelLimit
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: OBJ line exceeds capacity", ErrModelLimit)
	}
	if continuation != "" || len(out.Mesh.Triangles) == 0 {
		return nil, fmt.Errorf("%w: incomplete/empty OBJ", ErrInvalidModel)
	}
	return out, nil
}
