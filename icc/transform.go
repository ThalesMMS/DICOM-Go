package icc

import (
	"encoding/binary"
	"fmt"
	"image"
	"image/draw"
	"math"
)

// D50 is the profile connection space white (ICC.1 7.2.16).
var D50 = [3]float64{0.9642, 1, 0.8249}

// perceptualBlack is the ICC v4 perceptual reference medium black in XYZ.
var perceptualBlack = [3]float64{0.00336, 0.0034731, 0.00287}

// xyzD50ToLinearSRGB is the sRGB (IEC 61966-2-1) matrix from XYZ after a
// Bradford adaptation from D50 to D65.
var xyzD50ToLinearSRGB = [3][3]float64{
	{3.1338561, -1.6168667, -0.4906146},
	{-0.9787684, 1.9161415, 0.0334540},
	{0.0719453, -0.2289914, 1.4052427},
}

// Transform converts the device RGB colours of a profile to sRGB.
type Transform struct {
	toPCS    func(in [3]float64) [3]float64
	identity bool
	// Source names the profile part used: "AToB0", "matrix/TRC" or "sRGB".
	Source string

	// Matrix/TRC fast path for 8-bit samples.
	trc    [3]curve
	matrix [3][3]float64
}

// NewSRGBTransform builds the conversion of p's RGB colours to sRGB.
func NewSRGBTransform(p *Profile) (*Transform, error) {
	if p == nil {
		return nil, fmt.Errorf("%w: nil profile", ErrInvalidProfile)
	}
	if p.ColorSpace != "RGB " {
		return nil, fmt.Errorf("%w: data colour space %q", ErrUnsupportedProfile, printable(p.ColorSpace))
	}
	switch p.Class {
	case "mntr", "scnr", "spac":
	default:
		return nil, fmt.Errorf("%w: profile class %q", ErrUnsupportedProfile, printable(p.Class))
	}
	if p.Has("A2B0") {
		toPCS, err := p.lutToPCS("A2B0")
		if err != nil {
			return nil, err
		}
		if p.Version>>24 >= 4 {
			// ICC v4 perceptual PCS black is the perceptual reference medium
			// black (ICC.1 6.3.4.3); sRGB displays reach zero, so scale it
			// to zero (black point compensation) as the perceptual intent
			// intends.
			lut := toPCS
			toPCS = func(in [3]float64) [3]float64 {
				xyz := lut(in)
				for i := range xyz {
					xyz[i] = D50[i] * (xyz[i] - perceptualBlack[i]) / (D50[i] - perceptualBlack[i])
				}
				return xyz
			}
		}
		return &Transform{toPCS: toPCS, Source: "AToB0"}, nil
	}
	t := &Transform{Source: "matrix/TRC"}
	for i, name := range []string{"rTRC", "gTRC", "bTRC"} {
		data, _, err := p.tag(name, "curv", "para")
		if err != nil {
			return nil, err
		}
		if t.trc[i], _, err = parseCurve(data); err != nil {
			return nil, err
		}
	}
	for column, name := range []string{"rXYZ", "gXYZ", "bXYZ"} {
		data, _, err := p.tag(name, "XYZ ")
		if err != nil {
			return nil, err
		}
		if len(data) < 20 {
			return nil, fmt.Errorf("%w: truncated %s", ErrInvalidProfile, name)
		}
		for row := range 3 {
			t.matrix[row][column] = s15Fixed16(data[8+4*row:])
		}
	}
	if p.PCS != "XYZ " {
		return nil, fmt.Errorf("%w: matrix/TRC profile with a %q connection space", ErrInvalidProfile, printable(p.PCS))
	}
	t.toPCS = func(in [3]float64) [3]float64 {
		var lin [3]float64
		for i := range 3 {
			lin[i] = t.trc[i].eval(in[i])
		}
		return mul3(t.matrix, lin)
	}
	t.identity = isSRGBLike(t)
	if t.identity {
		t.Source = "sRGB"
	}
	return t, nil
}

// Identity reports a profile equivalent to sRGB (colorants and transfer
// curves within 1/1000), for which conversion leaves samples unchanged.
func (t *Transform) Identity() bool { return t != nil && t.identity }

// Convert maps device RGB in [0, 1] to sRGB in [0, 1].
func (t *Transform) Convert(r, g, b float64) (float64, float64, float64) {
	if t.identity {
		return r, g, b
	}
	xyz := t.toPCS([3]float64{r, g, b})
	lin := mul3(xyzD50ToLinearSRGB, xyz)
	return encodeSRGB(lin[0]), encodeSRGB(lin[1]), encodeSRGB(lin[2])
}

// Apply returns img converted to sRGB. *image.RGBA, *image.NRGBA and
// *image.RGBA64 are converted in place (alpha kept); other images are first
// copied to an *image.RGBA.
func (t *Transform) Apply(img image.Image) image.Image {
	if t == nil || t.identity || img == nil {
		return img
	}
	switch m := img.(type) {
	case *image.RGBA:
		t.apply8(m.Pix, m.Stride, m.Rect)
		return m
	case *image.NRGBA:
		t.apply8(m.Pix, m.Stride, m.Rect)
		return m
	case *image.RGBA64:
		t.apply16(m)
		return m
	}
	out := image.NewRGBA(img.Bounds())
	draw.Draw(out, out.Rect, img, img.Bounds().Min, draw.Src)
	t.apply8(out.Pix, out.Stride, out.Rect)
	return out
}

func (t *Transform) apply8(pix []byte, stride int, rect image.Rectangle) {
	var table *[3][256]float64
	if t.Source == "matrix/TRC" {
		table = new([3][256]float64)
		for c := range 3 {
			for v := range 256 {
				table[c][v] = t.trc[c].eval(float64(v) / 255)
			}
		}
	}
	cache := map[uint32][3]byte{}
	for y := 0; y < rect.Dy(); y++ {
		row := pix[y*stride : y*stride+rect.Dx()*4]
		for i := 0; i+3 < len(row); i += 4 {
			key := uint32(row[i])<<16 | uint32(row[i+1])<<8 | uint32(row[i+2])
			out, ok := cache[key]
			if !ok {
				var xyz [3]float64
				if table != nil {
					xyz = mul3(t.matrix, [3]float64{table[0][row[i]], table[1][row[i+1]], table[2][row[i+2]]})
				} else {
					xyz = t.toPCS([3]float64{float64(row[i]) / 255, float64(row[i+1]) / 255, float64(row[i+2]) / 255})
				}
				lin := mul3(xyzD50ToLinearSRGB, xyz)
				for c := range 3 {
					out[c] = uint8(math.Round(encodeSRGB(lin[c]) * 255))
				}
				if len(cache) < 1<<20 {
					cache[key] = out
				}
			}
			row[i], row[i+1], row[i+2] = out[0], out[1], out[2]
		}
	}
}

func (t *Transform) apply16(m *image.RGBA64) {
	for y := 0; y < m.Rect.Dy(); y++ {
		row := m.Pix[y*m.Stride : y*m.Stride+m.Rect.Dx()*8]
		for i := 0; i+7 < len(row); i += 8 {
			var in [3]float64
			for c := range 3 {
				in[c] = float64(binary.BigEndian.Uint16(row[i+2*c:])) / 65535
			}
			r, g, b := t.Convert(in[0], in[1], in[2])
			for c, v := range []float64{r, g, b} {
				binary.BigEndian.PutUint16(row[i+2*c:], uint16(math.Round(v*65535)))
			}
		}
	}
}

func mul3(m [3][3]float64, v [3]float64) [3]float64 {
	return [3]float64{
		m[0][0]*v[0] + m[0][1]*v[1] + m[0][2]*v[2],
		m[1][0]*v[0] + m[1][1]*v[1] + m[1][2]*v[2],
		m[2][0]*v[0] + m[2][1]*v[1] + m[2][2]*v[2],
	}
}

func encodeSRGB(v float64) float64 {
	v = clamp01(v)
	if v <= 0.0031308 {
		return 12.92 * v
	}
	return 1.055*math.Pow(v, 1/2.4) - 0.055
}

func decodeSRGB(v float64) float64 {
	if v <= 0.04045 {
		return v / 12.92
	}
	return math.Pow((v+0.055)/1.055, 2.4)
}

// srgbColorants are the D50-adapted sRGB colorant columns.
var srgbColorants = [3][3]float64{
	{0.4360747, 0.3850649, 0.1430804},
	{0.2225045, 0.7168786, 0.0606169},
	{0.0139322, 0.0971045, 0.7141733},
}

func isSRGBLike(t *Transform) bool {
	for r := range 3 {
		for c := range 3 {
			if math.Abs(t.matrix[r][c]-srgbColorants[r][c]) > 1e-3 {
				return false
			}
		}
	}
	for c := range 3 {
		for i := 0; i <= 64; i++ {
			x := float64(i) / 64
			if math.Abs(t.trc[c].eval(x)-decodeSRGB(x)) > 1e-3 {
				return false
			}
		}
	}
	return true
}

// labToXYZ converts CIELAB relative to D50 to XYZ.
func labToXYZ(l, a, b float64) [3]float64 {
	fy := (l + 16) / 116
	fx := fy + a/500
	fz := fy - b/200
	inv := func(t float64) float64 {
		if t > 6.0/29 {
			return t * t * t
		}
		return 3 * (6.0 / 29) * (6.0 / 29) * (t - 4.0/29)
	}
	return [3]float64{D50[0] * inv(fx), D50[1] * inv(fy), D50[2] * inv(fz)}
}

// pcsEncoding decodes normalized [0, 1] lut outputs into PCS values.
type pcsEncoding int

const (
	pcsLegacy8  pcsEncoding = iota // lut8Type
	pcsLegacy16                    // lut16Type: Lab 0xFF00 is L* 100
	pcsV4                          // lutAToBType
)

func decodePCS(pcs string, encoding pcsEncoding, n [3]float64) [3]float64 {
	if pcs == "XYZ " {
		scale := 65535.0 / 32768 // u1Fixed15Number
		return [3]float64{n[0] * scale, n[1] * scale, n[2] * scale}
	}
	scale := 1.0
	if encoding == pcsLegacy16 {
		scale = 65535.0 / 65280
	}
	return labToXYZ(n[0]*scale*100, n[1]*scale*255-128, n[2]*scale*255-128)
}

func (p *Profile) lutToPCS(signature string) (func([3]float64) [3]float64, error) {
	data, kind, err := p.tag(signature, "mft1", "mft2", "mAB ")
	if err != nil {
		return nil, err
	}
	var stages []func([]float64) []float64
	var encoding pcsEncoding
	switch kind {
	case "mft1", "mft2":
		stages, err = parseLegacyLUT(data, kind == "mft2")
		encoding = pcsLegacy8
		if kind == "mft2" {
			encoding = pcsLegacy16
		}
	case "mAB ":
		stages, err = parseLutAToB(data)
		encoding = pcsV4
	}
	if err != nil {
		return nil, err
	}
	pcs := p.PCS
	return func(in [3]float64) [3]float64 {
		v := []float64{in[0], in[1], in[2]}
		for _, stage := range stages {
			v = stage(v)
		}
		return decodePCS(pcs, encoding, [3]float64{v[0], v[1], v[2]})
	}, nil
}

func curvesStage(curves []curve) func([]float64) []float64 {
	return func(v []float64) []float64 {
		out := make([]float64, len(v))
		for i := range v {
			out[i] = curves[i].eval(v[i])
		}
		return out
	}
}

// parseLegacyLUT reads lut8Type or lut16Type with three inputs and three
// outputs. The matrix applies only to XYZ input and is ignored for RGB.
func parseLegacyLUT(data []byte, wide bool) ([]func([]float64) []float64, error) {
	if len(data) < 52 {
		return nil, fmt.Errorf("%w: truncated lut", ErrInvalidProfile)
	}
	in, out, grid := int(data[8]), int(data[9]), int(data[10])
	if in != 3 || out != 3 {
		return nil, fmt.Errorf("%w: lut with %d inputs and %d outputs", ErrUnsupportedProfile, in, out)
	}
	if grid < 2 {
		return nil, fmt.Errorf("%w: lut grid of %d points", ErrInvalidProfile, grid)
	}
	inEntries, outEntries, at, sample := 256, 256, 48, 1
	if wide {
		inEntries = int(binary.BigEndian.Uint16(data[48:50]))
		outEntries = int(binary.BigEndian.Uint16(data[50:52]))
		at, sample = 52, 2
		if inEntries < 2 || inEntries > 4096 || outEntries < 2 || outEntries > 4096 {
			return nil, fmt.Errorf("%w: lut16 table sizes %d and %d", ErrInvalidProfile, inEntries, outEntries)
		}
	}
	clutPoints := grid * grid * grid
	need := at + sample*(in*inEntries+clutPoints*out+out*outEntries)
	if need > len(data) {
		return nil, fmt.Errorf("%w: lut of %d bytes needs %d", ErrInvalidProfile, len(data), need)
	}
	read := func(count int) []float64 {
		values := make([]float64, count)
		for i := range values {
			if wide {
				values[i] = float64(binary.BigEndian.Uint16(data[at+2*i:])) / 65535
			} else {
				values[i] = float64(data[at+i]) / 255
			}
		}
		at += count * sample
		return values
	}
	tables := func(channels, entries int) []curve {
		curves := make([]curve, channels)
		for c := range curves {
			curves[c] = tableCurve(read(entries))
		}
		return curves
	}
	inCurves := tables(in, inEntries)
	clut := newCLUT([]int{grid, grid, grid}, out, read(clutPoints*out))
	outCurves := tables(out, outEntries)
	return []func([]float64) []float64{curvesStage(inCurves), clut.eval, curvesStage(outCurves)}, nil
}

// parseLutAToB reads lutAToBType: A curves, CLUT, M curves, matrix, B
// curves, each optional except B.
func parseLutAToB(data []byte) ([]func([]float64) []float64, error) {
	if len(data) < 32 {
		return nil, fmt.Errorf("%w: truncated lutAToB", ErrInvalidProfile)
	}
	in, out := int(data[8]), int(data[9])
	if in != 3 || out != 3 {
		return nil, fmt.Errorf("%w: lutAToB with %d inputs and %d outputs", ErrUnsupportedProfile, in, out)
	}
	offset := func(at int) (int, error) {
		o := int(binary.BigEndian.Uint32(data[at:]))
		if o != 0 && (o < 32 || o >= len(data)) {
			return 0, fmt.Errorf("%w: lutAToB element offset %d", ErrInvalidProfile, o)
		}
		return o, nil
	}
	var offsets [5]int // B, matrix, M, CLUT, A
	for i := range offsets {
		var err error
		if offsets[i], err = offset(12 + 4*i); err != nil {
			return nil, err
		}
	}
	curves := func(at, count int) ([]curve, error) {
		list := make([]curve, count)
		for i := range list {
			c, size, err := parseCurve(data[at:])
			if err != nil {
				return nil, err
			}
			list[i] = c
			if at += size; at > len(data) {
				return nil, fmt.Errorf("%w: truncated lutAToB curves", ErrInvalidProfile)
			}
		}
		return list, nil
	}
	if offsets[0] == 0 {
		return nil, fmt.Errorf("%w: lutAToB without B curves", ErrInvalidProfile)
	}
	var stages []func([]float64) []float64
	if o := offsets[4]; o != 0 {
		a, err := curves(o, in)
		if err != nil {
			return nil, err
		}
		stages = append(stages, curvesStage(a))
	}
	if o := offsets[3]; o != 0 {
		if o+20 > len(data) {
			return nil, fmt.Errorf("%w: truncated lutAToB CLUT", ErrInvalidProfile)
		}
		dims := make([]int, in)
		points := 1
		for i := range dims {
			dims[i] = int(data[o+i])
			if dims[i] < 2 {
				return nil, fmt.Errorf("%w: CLUT dimension of %d points", ErrInvalidProfile, dims[i])
			}
			points *= dims[i]
		}
		precision := int(data[o+16])
		if precision != 1 && precision != 2 {
			return nil, fmt.Errorf("%w: CLUT precision %d", ErrInvalidProfile, precision)
		}
		start := o + 20
		if start+points*out*precision > len(data) {
			return nil, fmt.Errorf("%w: truncated lutAToB CLUT", ErrInvalidProfile)
		}
		values := make([]float64, points*out)
		for i := range values {
			if precision == 2 {
				values[i] = float64(binary.BigEndian.Uint16(data[start+2*i:])) / 65535
			} else {
				values[i] = float64(data[start+i]) / 255
			}
		}
		stages = append(stages, newCLUT(dims, out, values).eval)
	} else if offsets[4] != 0 && in != out {
		return nil, fmt.Errorf("%w: lutAToB without CLUT changes channel count", ErrInvalidProfile)
	}
	if o := offsets[2]; o != 0 {
		m, err := curves(o, out)
		if err != nil {
			return nil, err
		}
		stages = append(stages, curvesStage(m))
	}
	if o := offsets[1]; o != 0 {
		if o+48 > len(data) {
			return nil, fmt.Errorf("%w: truncated lutAToB matrix", ErrInvalidProfile)
		}
		var e [12]float64
		for i := range e {
			e[i] = s15Fixed16(data[o+4*i:])
		}
		stages = append(stages, func(v []float64) []float64 {
			return []float64{
				e[0]*v[0] + e[1]*v[1] + e[2]*v[2] + e[9],
				e[3]*v[0] + e[4]*v[1] + e[5]*v[2] + e[10],
				e[6]*v[0] + e[7]*v[1] + e[8]*v[2] + e[11],
			}
		})
	}
	b, err := curves(offsets[0], out)
	if err != nil {
		return nil, err
	}
	stages = append(stages, curvesStage(b))
	return stages, nil
}

// clut is a three-input colour lookup table; the first input varies
// slowest.
type clut struct {
	dims   []int
	out    int
	values []float64
}

func newCLUT(dims []int, out int, values []float64) clut {
	return clut{dims: dims, out: out, values: values}
}

// eval interpolates tetrahedrally inside the grid cell.
func (c clut) eval(v []float64) []float64 {
	var base [3]int
	var frac [3]float64
	for i := range 3 {
		pos := clamp01(v[i]) * float64(c.dims[i]-1)
		base[i] = int(pos)
		if base[i] >= c.dims[i]-1 {
			base[i] = c.dims[i] - 2
		}
		frac[i] = pos - float64(base[i])
	}
	index := func(i0, i1, i2 int) int {
		return ((base[0]+i0)*c.dims[1]*c.dims[2] + (base[1]+i1)*c.dims[2] + (base[2] + i2)) * c.out
	}
	rx, ry, rz := frac[0], frac[1], frac[2]
	c000 := index(0, 0, 0)
	c111 := index(1, 1, 1)
	var c1, c2 int
	var w1, w2, w3 float64 // weights of the three edges walked
	switch {
	case rx >= ry && ry >= rz:
		c1, c2, w1, w2, w3 = index(1, 0, 0), index(1, 1, 0), rx, ry, rz
	case rx >= rz && rz >= ry:
		c1, c2, w1, w2, w3 = index(1, 0, 0), index(1, 0, 1), rx, rz, ry
	case rz >= rx && rx >= ry:
		c1, c2, w1, w2, w3 = index(0, 0, 1), index(1, 0, 1), rz, rx, ry
	case ry >= rx && rx >= rz:
		c1, c2, w1, w2, w3 = index(0, 1, 0), index(1, 1, 0), ry, rx, rz
	case ry >= rz && rz >= rx:
		c1, c2, w1, w2, w3 = index(0, 1, 0), index(0, 1, 1), ry, rz, rx
	default: // rz >= ry >= rx
		c1, c2, w1, w2, w3 = index(0, 0, 1), index(0, 1, 1), rz, ry, rx
	}
	out := make([]float64, c.out)
	for k := range out {
		p0, p1, p2, p3 := c.values[c000+k], c.values[c1+k], c.values[c2+k], c.values[c111+k]
		out[k] = p0 + (p1-p0)*w1 + (p2-p1)*w2 + (p3-p2)*w3
	}
	return out
}
