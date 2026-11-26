package icc

import (
	"encoding/binary"
	"math"
)

// MatrixTRC describes an RGB display profile by its D50-adapted colorant
// columns and one power-law transfer curve.
type MatrixTRC struct {
	Description string
	// Colorants holds rXYZ, gXYZ and bXYZ as the columns of the matrix from
	// linear RGB to PCS XYZ.
	Colorants [3][3]float64
	Gamma     float64
}

// AdobeRGB1998 is the Adobe RGB (1998) colour space (D50-adapted
// colorants, gamma 563/256), a common wide-gamut photography space.
var AdobeRGB1998 = MatrixTRC{
	Description: "Adobe RGB (1998) compatible",
	Colorants: [3][3]float64{
		{0.6097412, 0.2052765, 0.1491852},
		{0.3111115, 0.6256714, 0.0632172},
		{0.0194702, 0.0608673, 0.7445679},
	},
	Gamma: 563.0 / 256,
}

// Build writes m as an ICC v4.3 display (mntr) profile.
func (m MatrixTRC) Build() []byte {
	fixed := func(v float64) uint32 { return uint32(int32(math.Round(v * 65536))) }
	xyz := func(x, y, z float64) []byte {
		out := make([]byte, 20)
		copy(out, "XYZ ")
		for i, v := range []float64{x, y, z} {
			binary.BigEndian.PutUint32(out[8+4*i:], fixed(v))
		}
		return out
	}
	curve := make([]byte, 16)
	copy(curve, "para")
	binary.BigEndian.PutUint32(curve[12:], fixed(m.Gamma))
	text := []byte(m.Description)
	desc := make([]byte, 28+2*len(text))
	copy(desc, "mluc")
	binary.BigEndian.PutUint32(desc[8:], 1)
	binary.BigEndian.PutUint32(desc[12:], 12)
	copy(desc[16:], "enUS")
	binary.BigEndian.PutUint32(desc[20:], uint32(2*len(text)))
	binary.BigEndian.PutUint32(desc[24:], 28)
	for i, c := range text {
		desc[29+2*i] = c
	}
	c := m.Colorants
	tags := []struct {
		signature string
		value     []byte
	}{
		{"desc", desc},
		{"wtpt", xyz(D50[0], D50[1], D50[2])},
		{"rXYZ", xyz(c[0][0], c[1][0], c[2][0])},
		{"gXYZ", xyz(c[0][1], c[1][1], c[2][1])},
		{"bXYZ", xyz(c[0][2], c[1][2], c[2][2])},
		{"rTRC", curve},
		{"gTRC", curve},
		{"bTRC", curve},
	}
	tableEnd := headerSize + 4 + 12*len(tags)
	size := tableEnd
	for _, tag := range tags {
		size += pad4(len(tag.value))
	}
	out := make([]byte, size)
	binary.BigEndian.PutUint32(out[0:], uint32(size))
	binary.BigEndian.PutUint32(out[8:], 0x04300000)
	copy(out[12:], "mntr")
	copy(out[16:], "RGB ")
	copy(out[20:], "XYZ ")
	copy(out[36:], "acsp")
	for i, v := range D50 {
		binary.BigEndian.PutUint32(out[68+4*i:], fixed(v))
	}
	binary.BigEndian.PutUint32(out[128:], uint32(len(tags)))
	at := tableEnd
	for i, tag := range tags {
		entry := out[132+12*i:]
		copy(entry, tag.signature)
		binary.BigEndian.PutUint32(entry[4:], uint32(at))
		binary.BigEndian.PutUint32(entry[8:], uint32(len(tag.value)))
		copy(out[at:], tag.value)
		at += pad4(len(tag.value))
	}
	return out
}
