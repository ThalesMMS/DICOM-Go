package jpeg2000

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// frameCase is a synthetic native frame with its exact samples.
type frameCase struct {
	name     string
	metadata pixeldata.Metadata
	frame    []byte
	samples  [][]int32 // per component, raster order
}

// syntheticFrame mixes extremes, a gradient, texture and noise so that every
// band and every exponent occurs.
func syntheticFrame(width, height, components, bits, allocated int, signed bool, seed int64) frameCase {
	rng := rand.New(rand.NewSource(seed))
	m := pixeldata.Metadata{
		Rows: uint16(height), Columns: uint16(width), SamplesPerPixel: uint16(components),
		BitsAllocated: uint16(allocated), BitsStored: uint16(bits), HighBit: uint16(bits - 1),
		NumberOfFrames: 1, PhotometricInterpretation: "MONOCHROME2",
	}
	if signed {
		m.PixelRepresentation = 1
	}
	if components == 3 {
		m.PhotometricInterpretation = "RGB"
		m.PlanarConfigurationPresent = true
	}
	low, high := 0, 1<<bits-1
	if signed {
		low, high = -(1 << (bits - 1)), 1<<(bits-1)-1
	}
	span := high - low
	samples := make([][]int32, components)
	for c := range samples {
		samples[c] = make([]int32, width*height)
	}
	frame := make([]byte, width*height*components*allocated/8)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			i := y*width + x
			for c := 0; c < components; c++ {
				var v int
				switch (x/7 + y/5 + c) % 5 {
				case 0:
					v = low + (x*span/max(width, 1)+y*span/max(height, 1))/2
				case 1:
					v = low + rng.Intn(span+1)
				case 2:
					if (x+y)%2 == 0 {
						v = low
					} else {
						v = high
					}
				case 3:
					v = low + span/2 + (x%3-1)*(span/16) + c
				default:
					v = low + ((x*31+y*17+c*101)%(span+1)+span+1)%(span+1)
				}
				v = min(max(v, low), high)
				samples[c][i] = int32(v)
				o := (i*components + c) * allocated / 8
				raw := uint32(int32(v)) & (1<<bits - 1)
				if allocated == 8 {
					frame[o] = byte(raw)
				} else {
					if signed && v < 0 {
						raw = uint32(int32(v)) & 0xFFFF // sign-extended container
					}
					binary.LittleEndian.PutUint16(frame[o:], uint16(raw))
				}
			}
		}
	}
	sign := "u"
	if signed {
		sign = "s"
	}
	return frameCase{
		name:     fmt.Sprintf("%dx%d-%dc-%d%s%d", width, height, components, bits, sign, allocated),
		metadata: m, frame: frame, samples: samples,
	}
}

// losslessCases covers every precision 1..16, signed monochrome, colour,
// both containers, tiny and odd geometries and more than one code-block.
func losslessCases() []frameCase {
	var cases []frameCase
	seed := int64(1150)
	add := func(w, h, comps, bits int, signed bool) {
		allocated := 8
		if bits > 8 {
			allocated = 16
		}
		seed++
		cases = append(cases, syntheticFrame(w, h, comps, bits, allocated, signed, seed))
	}
	for bits := 1; bits <= 16; bits++ {
		add(67, 45, 1, bits, false)
		add(33, 70, 1, bits, true)
	}
	for _, bits := range []int{8, 10, 12, 16} {
		add(41, 39, 3, bits, false)
	}
	add(1, 1, 1, 8, false)
	add(1, 1, 3, 16, false)
	add(1, 37, 1, 12, true)
	add(53, 1, 3, 8, false)
	add(2, 2, 1, 16, true)
	add(257, 131, 1, 16, true)
	add(130, 129, 3, 8, false)
	// 8 stored bits in a 16-bit container
	seed++
	cases = append(cases, syntheticFrame(96, 80, 1, 8, 16, false, seed))
	return cases
}

// planesOf reads native interleaved samples into planes, sign-extending
// BitsStored for signed data.
func planesOf(frame []byte, m pixeldata.Metadata) [][]int32 {
	comps := int(m.SamplesPerPixel)
	pixels := int(m.Rows) * int(m.Columns)
	width := int(m.BitsAllocated / 8)
	mask := uint32(1)<<m.BitsStored - 1
	out := make([][]int32, comps)
	for c := range out {
		out[c] = make([]int32, pixels)
		for i := 0; i < pixels; i++ {
			o := (i*comps + c) * width
			raw := uint32(frame[o])
			if width == 2 {
				raw |= uint32(frame[o+1]) << 8
			}
			raw &= mask
			v := int32(raw)
			if m.PixelRepresentation == 1 && raw>>(m.BitsStored-1) != 0 {
				v -= int32(mask) + 1
			}
			out[c][i] = v
		}
	}
	return out
}

// smoothFrame is a band-limited image with mild noise, closer to what lossy
// coding is for than syntheticFrame's extremes.
func smoothFrame(width, height, components, bits int, signed bool, seed int64) frameCase {
	allocated := 8
	if bits > 8 {
		allocated = 16
	}
	tc := syntheticFrame(width, height, components, bits, allocated, signed, seed)
	rng := rand.New(rand.NewSource(seed))
	low, high := 0.0, float64(int(1)<<bits-1)
	if signed {
		low, high = -float64(int(1)<<(bits-1)), float64(int(1)<<(bits-1)-1)
	}
	mid, amp := (low+high)/2, (high-low)/2
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			i := y*width + x
			for c := 0; c < components; c++ {
				fx, fy := float64(x)/float64(width), float64(y)/float64(height)
				v := mid + amp*0.6*math.Sin(2*math.Pi*(fx*1.5+float64(c)*0.2))*math.Cos(2*math.Pi*fy*0.8) +
					amp*0.25*math.Sin(2*math.Pi*(fx*7+fy*5)) + amp*0.01*rng.NormFloat64()
				s := int32(math.Round(math.Min(math.Max(v, low), high)))
				tc.samples[c][i] = s
				o := (i*components + c) * allocated / 8
				raw := uint32(s)
				if allocated == 8 {
					tc.frame[o] = byte(raw)
				} else {
					binary.LittleEndian.PutUint16(tc.frame[o:], uint16(raw))
				}
			}
		}
	}
	tc.name = "smooth-" + tc.name
	return tc
}
