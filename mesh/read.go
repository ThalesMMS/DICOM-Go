package mesh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

var ErrInvalidModel = errors.New("mesh: invalid model input")
var ErrModelLimit = errors.New("mesh: model read limit exceeded")
var ErrUnsupportedModel = errors.New("mesh: unsupported model feature")

// ReadOptions requires an explicit positive physical scale for geometry.
// Zero numeric limits select finite defaults; no format implies a unit.
// MaxOutputBytes bounds logical retained geometry and declaration storage,
// excluding input bytes, parser scratch, Go allocation capacity and map overhead.
// The independent count/line/name/input limits also bound those allocations.
type ReadOptions struct {
	MillimetresPerUnit                                                                   float64
	MaxInputBytes, MaxOutputBytes                                                        int64
	MaxVertices, MaxTriangles, MaxMaterials, MaxLineBytes, MaxNameBytes, MaxFaceVertices int
}

func DefaultReadOptions() ReadOptions {
	return ReadOptions{MaxInputBytes: 64 << 20, MaxOutputBytes: 128 << 20, MaxVertices: 1_000_000, MaxTriangles: 2_000_000, MaxMaterials: 1024, MaxLineBytes: 65536, MaxNameBytes: 256, MaxFaceVertices: 256}
}
func (o ReadOptions) normalized(geometry bool) (ReadOptions, error) {
	d := DefaultReadOptions()
	if o.MaxInputBytes == 0 {
		o.MaxInputBytes = d.MaxInputBytes
	}
	if o.MaxOutputBytes == 0 {
		o.MaxOutputBytes = d.MaxOutputBytes
	}
	if o.MaxVertices == 0 {
		o.MaxVertices = d.MaxVertices
	}
	if o.MaxTriangles == 0 {
		o.MaxTriangles = d.MaxTriangles
	}
	if o.MaxMaterials == 0 {
		o.MaxMaterials = d.MaxMaterials
	}
	if o.MaxLineBytes == 0 {
		o.MaxLineBytes = d.MaxLineBytes
	}
	if o.MaxNameBytes == 0 {
		o.MaxNameBytes = d.MaxNameBytes
	}
	if o.MaxFaceVertices == 0 {
		o.MaxFaceVertices = d.MaxFaceVertices
	}
	if o.MaxInputBytes < 1 || o.MaxInputBytes > 1<<30 || o.MaxOutputBytes < 1 || o.MaxOutputBytes > 1<<30 || o.MaxVertices < 1 || o.MaxVertices > 16_000_000 || o.MaxTriangles < 1 || o.MaxTriangles > 16_000_000 || o.MaxMaterials < 1 || o.MaxMaterials > 65536 || o.MaxLineBytes < 1 || o.MaxLineBytes > 1<<20 || o.MaxNameBytes < 1 || o.MaxNameBytes > 65536 || o.MaxFaceVertices < 3 || o.MaxFaceVertices > 4096 {
		return o, fmt.Errorf("%w: invalid read limits", ErrInvalidModel)
	}
	if geometry && (!finiteModel(o.MillimetresPerUnit) || o.MillimetresPerUnit <= 0) {
		return o, fmt.Errorf("%w: explicit millimetres-per-unit scale required", ErrInvalidModel)
	}
	return o, nil
}
func finiteModel(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

type modelContextReader struct {
	ctx    context.Context
	source io.Reader
}

func (r modelContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.source.Read(p)
	if cancel := r.ctx.Err(); cancel != nil {
		return n, cancel
	}
	return n, err
}
func modelInput(ctx context.Context, r io.Reader, o ReadOptions) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if r == nil {
		return nil, fmt.Errorf("%w: nil reader", ErrInvalidModel)
	}
	data, err := io.ReadAll(io.LimitReader(modelContextReader{ctx: ctx, source: r}, o.MaxInputBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > o.MaxInputBytes {
		return nil, ErrModelLimit
	}
	return data, nil
}
func modelNumber(raw string) (float64, error) {
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || !finiteModel(v) {
		return 0, fmt.Errorf("%w: non-finite/invalid number %q", ErrInvalidModel, raw)
	}
	return v, nil
}
func modelPoint(raw []string, scale float64) ([3]float32, error) {
	var p [3]float32
	if len(raw) != 3 {
		return p, fmt.Errorf("%w: expected 3 coordinates", ErrInvalidModel)
	}
	for i := range p {
		v, err := modelNumber(raw[i])
		if err != nil {
			return p, err
		}
		v *= scale
		if !finiteModel(v) || math.Abs(v) > math.MaxFloat32 {
			return p, fmt.Errorf("%w: coordinate outside finite float32 millimetres", ErrInvalidModel)
		}
		p[i] = float32(v)
	}
	return p, nil
}
func modelName(raw string, o ReadOptions) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > o.MaxNameBytes || !utf8.ValidString(raw) || strings.ContainsAny(raw, "\x00\r\n") {
		return "", fmt.Errorf("%w: invalid/oversized name", ErrInvalidModel)
	}
	return strings.Clone(raw), nil
}
func modelTriangle(m *Mesh, t [3]uint32, o ReadOptions) error {
	if len(m.Triangles) >= o.MaxTriangles || int64(len(m.Vertices))*12+int64(len(m.Triangles)+1)*16 > o.MaxOutputBytes {
		return ErrModelLimit
	}
	for _, index := range t {
		if uint64(index) >= uint64(len(m.Vertices)) {
			return fmt.Errorf("%w: face indexes missing vertex", ErrInvalidModel)
		}
	}
	a, b, c := m.point(t[0]), m.point(t[1]), m.point(t[2])
	normal := cross3(sub3(b, a), sub3(c, a))
	if t[0] == t[1] || t[1] == t[2] || t[0] == t[2] || dot3(normal, normal) == 0 {
		return fmt.Errorf("%w: degenerate triangle", ErrInvalidModel)
	}
	m.Triangles = append(m.Triangles, t)
	return nil
}
func modelVertex(m *Mesh, p [3]float32, o ReadOptions) error {
	if len(m.Vertices) >= o.MaxVertices || int64(len(m.Vertices)+1)*12+int64(len(m.Triangles))*16 > o.MaxOutputBytes {
		return ErrModelLimit
	}
	m.Vertices = append(m.Vertices, p)
	return nil
}
