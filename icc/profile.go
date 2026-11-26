// Package icc validates ICC colour profiles, such as ICC Profile (0028,2000),
// and converts colours they describe to sRGB for display and export.
//
// Parse checks the profile structure (ICC.1:2010 header, tag table and the
// tag types it uses); NewSRGBTransform builds the conversion of the
// profile's RGB device colours to sRGB through the profile connection space:
// the AToB0 (perceptual) lut8, lut16 or lutAtoB tag when present, otherwise
// the red, green and blue matrix/TRC tags. The written ICC specification is
// the authority; no colour management library code is used.
package icc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

var (
	// ErrInvalidProfile reports a profile whose structure is malformed.
	ErrInvalidProfile = errors.New("icc: invalid profile")
	// ErrUnsupportedProfile reports a well-formed profile this package does
	// not convert (a non-RGB device space, a device link, a tag type it
	// does not implement).
	ErrUnsupportedProfile = errors.New("icc: unsupported profile")
)

const (
	headerSize = 128
	maxTags    = 1024
	// MaxProfileBytes bounds the profiles Parse accepts.
	MaxProfileBytes = 64 << 20
)

// Profile is a parsed ICC profile.
type Profile struct {
	Version         uint32 // major.minor.bugfix as in the header, e.g. 0x04300000
	Class           string // "mntr", "scnr", "spac", ...
	ColorSpace      string // data colour space, e.g. "RGB "
	PCS             string // "XYZ " or "Lab "
	RenderingIntent uint32
	Illuminant      [3]float64

	data []byte
	tags map[string][]byte
}

// Parse validates data and returns the profile. Bytes after the size the
// header declares (DICOM pads OB values to even length) are ignored.
func Parse(data []byte) (*Profile, error) {
	if len(data) < headerSize+4 {
		return nil, fmt.Errorf("%w: %d bytes is shorter than the header and tag count", ErrInvalidProfile, len(data))
	}
	size := int64(binary.BigEndian.Uint32(data[0:4]))
	if size < headerSize+4 || size > int64(len(data)) || size > MaxProfileBytes {
		return nil, fmt.Errorf("%w: declared size %d for %d bytes", ErrInvalidProfile, size, len(data))
	}
	data = data[:size]
	if string(data[36:40]) != "acsp" {
		return nil, fmt.Errorf("%w: missing the acsp signature", ErrInvalidProfile)
	}
	p := &Profile{
		Version:         binary.BigEndian.Uint32(data[8:12]),
		Class:           string(data[12:16]),
		ColorSpace:      string(data[16:20]),
		PCS:             string(data[20:24]),
		RenderingIntent: binary.BigEndian.Uint32(data[64:68]),
		data:            data,
		tags:            map[string][]byte{},
	}
	if major := p.Version >> 24; major < 2 || major > 5 {
		return nil, fmt.Errorf("%w: version %d", ErrInvalidProfile, major)
	}
	for i := range 3 {
		p.Illuminant[i] = s15Fixed16(data[68+4*i:])
	}
	count := int64(binary.BigEndian.Uint32(data[128:132]))
	if count > maxTags || headerSize+4+12*count > size {
		return nil, fmt.Errorf("%w: %d tags do not fit", ErrInvalidProfile, count)
	}
	tableEnd := headerSize + 4 + 12*count
	for i := range count {
		entry := data[132+12*i:]
		signature := string(entry[0:4])
		offset := int64(binary.BigEndian.Uint32(entry[4:8]))
		length := int64(binary.BigEndian.Uint32(entry[8:12]))
		if offset < tableEnd || length < 8 || offset+length > size {
			return nil, fmt.Errorf("%w: tag %q at %d+%d is outside the profile", ErrInvalidProfile, printable(signature), offset, length)
		}
		if _, dup := p.tags[signature]; dup {
			return nil, fmt.Errorf("%w: tag %q appears twice", ErrInvalidProfile, printable(signature))
		}
		p.tags[signature] = data[offset : offset+length]
	}
	if p.PCS != "XYZ " && p.PCS != "Lab " && p.Class != "link" {
		return nil, fmt.Errorf("%w: connection space %q", ErrInvalidProfile, printable(p.PCS))
	}
	return p, nil
}

// Bytes returns the profile's bytes (without trailing padding).
func (p *Profile) Bytes() []byte { return p.data }

// Has reports whether the profile has the tag with signature.
func (p *Profile) Has(signature string) bool {
	_, ok := p.tags[signature]
	return ok
}

// tag returns a tag's data after checking its type signature.
func (p *Profile) tag(signature string, types ...string) ([]byte, string, error) {
	data, ok := p.tags[signature]
	if !ok {
		return nil, "", fmt.Errorf("%w: missing tag %q", ErrInvalidProfile, signature)
	}
	kind := string(data[0:4])
	for _, t := range types {
		if kind == t {
			return data, kind, nil
		}
	}
	return nil, "", fmt.Errorf("%w: tag %q has type %q", ErrUnsupportedProfile, signature, printable(kind))
}

// Description returns the profile description tag's text, or "".
func (p *Profile) Description() string {
	data, ok := p.tags["desc"]
	if !ok {
		return ""
	}
	switch string(data[0:4]) {
	case "desc": // v2 textDescriptionType: ASCII count then text
		if len(data) < 12 {
			return ""
		}
		n := int(binary.BigEndian.Uint32(data[8:12]))
		if n <= 0 || 12+n > len(data) {
			return ""
		}
		return trimNUL(string(data[12 : 12+n]))
	case "mluc": // first record, UTF-16BE
		if len(data) < 28 {
			return ""
		}
		length := int(binary.BigEndian.Uint32(data[20:24]))
		offset := int(binary.BigEndian.Uint32(data[24:28]))
		if length < 0 || offset < 0 || offset+length > len(data) {
			return ""
		}
		runes := make([]rune, 0, length/2)
		for i := offset; i+1 < offset+length; i += 2 {
			runes = append(runes, rune(binary.BigEndian.Uint16(data[i:])))
		}
		return trimNUL(string(runes))
	}
	return ""
}

func trimNUL(s string) string {
	for len(s) > 0 && s[len(s)-1] == 0 {
		s = s[:len(s)-1]
	}
	return s
}

func printable(s string) string {
	out := []byte(s)
	for i, c := range out {
		if c < 32 || c > 126 {
			out[i] = '?'
		}
	}
	return string(out)
}

func s15Fixed16(b []byte) float64 {
	return float64(int32(binary.BigEndian.Uint32(b))) / 65536
}

// curve is a one-dimensional transfer function on [0, 1].
type curve interface {
	eval(x float64) float64
}

type identityCurve struct{}

func (identityCurve) eval(x float64) float64 { return x }

type gammaCurve float64

func (g gammaCurve) eval(x float64) float64 { return math.Pow(clamp01(x), float64(g)) }

// tableCurve interpolates evenly spaced samples (curveType, lut tables).
type tableCurve []float64

func (t tableCurve) eval(x float64) float64 {
	return interpolate(t, clamp01(x))
}

func interpolate(t []float64, x float64) float64 {
	if len(t) == 1 {
		return t[0]
	}
	pos := x * float64(len(t)-1)
	i := int(pos)
	if i >= len(t)-1 {
		return t[len(t)-1]
	}
	f := pos - float64(i)
	return t[i] + (t[i+1]-t[i])*f
}

// parametricCurve is parametricCurveType, functions 0 to 4.
type parametricCurve struct {
	kind                int
	g, a, b, c, d, e, f float64
}

func (p parametricCurve) eval(x float64) float64 {
	x = clamp01(x)
	pow := func(v float64) float64 {
		if v <= 0 {
			return 0
		}
		return math.Pow(v, p.g)
	}
	var y float64
	switch p.kind {
	case 0:
		y = pow(x)
	case 1:
		if x >= -p.b/p.a {
			y = pow(p.a*x + p.b)
		}
	case 2:
		y = p.c
		if x >= -p.b/p.a {
			y = pow(p.a*x+p.b) + p.c
		}
	case 3:
		if x >= p.d {
			y = pow(p.a*x + p.b)
		} else {
			y = p.c * x
		}
	case 4:
		if x >= p.d {
			y = pow(p.a*x+p.b) + p.e
		} else {
			y = p.c*x + p.f
		}
	}
	return clamp01(y)
}

// parseCurve reads a curveType or parametricCurveType element at the start
// of data and returns it with its padded length.
func parseCurve(data []byte) (curve, int, error) {
	if len(data) < 12 {
		return nil, 0, fmt.Errorf("%w: truncated curve", ErrInvalidProfile)
	}
	switch string(data[0:4]) {
	case "curv":
		n := int(binary.BigEndian.Uint32(data[8:12]))
		if n > (len(data)-12)/2 {
			return nil, 0, fmt.Errorf("%w: curve of %d entries is truncated", ErrInvalidProfile, n)
		}
		size := pad4(12 + 2*n)
		switch n {
		case 0:
			return identityCurve{}, size, nil
		case 1:
			return gammaCurve(float64(binary.BigEndian.Uint16(data[12:14])) / 256), size, nil
		}
		table := make(tableCurve, n)
		for i := range table {
			table[i] = float64(binary.BigEndian.Uint16(data[12+2*i:])) / 65535
		}
		return table, size, nil
	case "para":
		kind := int(binary.BigEndian.Uint16(data[8:10]))
		counts := []int{1, 3, 4, 5, 7}
		if kind >= len(counts) {
			return nil, 0, fmt.Errorf("%w: parametric curve function %d", ErrUnsupportedProfile, kind)
		}
		n := counts[kind]
		if len(data) < 12+4*n {
			return nil, 0, fmt.Errorf("%w: truncated parametric curve", ErrInvalidProfile)
		}
		v := make([]float64, 7)
		for i := range n {
			v[i] = s15Fixed16(data[12+4*i:])
		}
		c := parametricCurve{kind: kind, g: v[0], a: v[1], b: v[2], c: v[3], d: v[4], e: v[5], f: v[6]}
		if kind == 0 {
			c.a = 1
		}
		if (kind == 1 || kind == 2) && c.a == 0 {
			return nil, 0, fmt.Errorf("%w: parametric curve with a = 0", ErrInvalidProfile)
		}
		return c, pad4(12 + 4*n), nil
	}
	return nil, 0, fmt.Errorf("%w: curve type %q", ErrUnsupportedProfile, printable(string(data[0:4])))
}

func pad4(n int) int { return (n + 3) &^ 3 }

func clamp01(x float64) float64 {
	switch {
	case x < 0 || math.IsNaN(x):
		return 0
	case x > 1:
		return 1
	}
	return x
}
