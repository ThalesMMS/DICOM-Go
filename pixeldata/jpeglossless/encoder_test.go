package jpeglossless

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

func encoderTestMetadata(rows, columns, samples, allocated, stored, signed uint16, photometric string) pixeldata.Metadata {
	return pixeldata.Metadata{
		Rows: rows, Columns: columns, SamplesPerPixel: samples, BitsAllocated: allocated, BitsStored: stored, HighBit: stored - 1,
		PixelRepresentation: signed, PhotometricInterpretation: photometric, NumberOfFrames: 1,
		PlanarConfigurationPresent: samples == 3,
	}
}

// encoderTestFrame fills a native frame with edges, gradients, extremes and
// noise so every predictor and difference category is exercised.
func encoderTestFrame(metadata pixeldata.Metadata, seed int) []byte {
	samples := int(metadata.Rows) * int(metadata.Columns) * int(metadata.SamplesPerPixel)
	width := int(metadata.BitsAllocated / 8)
	maxValue := uint32(1)<<metadata.BitsStored - 1
	out := make([]byte, samples*width)
	state := uint32(seed)*2654435761 + 1
	for i := 0; i < samples; i++ {
		state = state*1664525 + 1013904223
		var value uint32
		switch (i / 97) % 5 {
		case 0:
			value = uint32(i*7) & maxValue
		case 1:
			value = state >> 7 & maxValue
		case 2:
			value = maxValue
		case 3:
			value = 0
		default:
			value = (uint32(i) * 131) % (maxValue + 1)
		}
		if metadata.PixelRepresentation == 1 && value&(1<<(metadata.BitsStored-1)) != 0 {
			value |= ^maxValue // sign-extend in the container
		}
		if width == 2 {
			binary.LittleEndian.PutUint16(out[2*i:], uint16(value))
		} else {
			out[i] = byte(value)
		}
	}
	return out
}

// canonical returns the native frame as the decoder returns it: the Bits
// Stored bits, with the unused high bits zero.
func canonical(frame []byte, metadata pixeldata.Metadata) []byte {
	out := append([]byte(nil), frame...)
	if metadata.BitsStored == metadata.BitsAllocated {
		return out
	}
	mask := uint16(1)<<metadata.BitsStored - 1
	for i := 0; i < len(out); i += int(metadata.BitsAllocated / 8) {
		var v uint16
		if metadata.BitsAllocated == 16 {
			v = binary.LittleEndian.Uint16(out[i:])
		} else {
			v = uint16(out[i])
		}
		v &= mask
		if metadata.BitsAllocated == 16 {
			binary.LittleEndian.PutUint16(out[i:], v)
		} else {
			out[i] = byte(v)
		}
	}
	return out
}

func decodeEncoded(t testing.TB, codec *Codec, encoded []byte, metadata pixeldata.Metadata) []byte {
	t.Helper()
	elements := []core.Element{
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0010), core.VRUS, nil, metadata.Rows),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0011), core.VRUS, nil, metadata.Columns),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0002), core.VRUS, nil, metadata.SamplesPerPixel),
		dicomtest.NewStringElement(core.NewTag(0x0028, 0x0004), core.VRCS, metadata.PhotometricInterpretation),
		dicomtest.NewStringElement(core.NewTag(0x0028, 0x0008), core.VRIS, "1"),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0100), core.VRUS, nil, metadata.BitsAllocated),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0101), core.VRUS, nil, metadata.BitsStored),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0102), core.VRUS, nil, metadata.HighBit),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0103), core.VRUS, nil, metadata.PixelRepresentation),
	}
	if metadata.SamplesPerPixel == 3 {
		elements = append(elements, dicomtest.Uint16Element(core.NewTag(0x0028, 0x0006), core.VRUS, nil, 0))
	}
	if len(encoded)%2 == 1 {
		encoded = append(encoded, 0)
	}
	elements = append(elements, dicomtest.NewFragmentSequenceElement(core.TagPixelData, nil, encoded))
	obj := object.FromElements(elements, nil)
	pixel, err := pixeldata.Extract(obj)
	if err != nil {
		t.Fatal(err)
	}
	frames, err := codec.Decode(pixel, obj)
	if err != nil {
		t.Fatal(err)
	}
	return frames.Data[0]
}

func TestEncoderRoundTripsEveryCombination(t *testing.T) {
	for _, predictor := range []int{1, 2, 3, 4, 5, 6, 7} {
		encoder, err := NewProcess14Encoder(predictor)
		if err != nil {
			t.Fatal(err)
		}
		for _, stored := range []uint16{2, 7, 8, 10, 12, 15, 16} {
			allocated := uint16(8)
			if stored > 8 {
				allocated = 16
			}
			for _, samples := range []uint16{1, 3} {
				for _, signed := range []uint16{0, 1} {
					if samples == 3 && signed == 1 {
						continue
					}
					photometric := "MONOCHROME2"
					if samples == 3 {
						photometric = "RGB"
					}
					metadata := encoderTestMetadata(13, 29, samples, allocated, stored, signed, photometric)
					name := fmt.Sprintf("sv%d-%dbit-%dspp-signed%d", predictor, stored, samples, signed)
					frame := encoderTestFrame(metadata, predictor+int(stored))
					encoded, err := encoder.EncodeFrame(context.Background(), frame, metadata)
					if err != nil {
						t.Fatalf("%s: %v", name, err)
					}
					if got := decodeEncoded(t, New(), encoded.Data, metadata); !bytes.Equal(got, canonical(frame, metadata)) {
						t.Fatalf("%s: round trip differs", name)
					}
				}
			}
		}
	}
	// SV1 streams decode under the .70 codec, which requires predictor 1.
	metadata := encoderTestMetadata(5, 7, 1, 16, 16, 0, "MONOCHROME2")
	frame := encoderTestFrame(metadata, 3)
	encoded, err := NewSV1Encoder().EncodeFrame(context.Background(), frame, metadata)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeEncoded(t, newSV1(), encoded.Data, metadata); !bytes.Equal(got, frame) {
		t.Fatal("SV1 round trip differs")
	}
	// Extreme differences: 0 next to 65535 needs category 16.
	extreme := make([]byte, 8)
	binary.LittleEndian.PutUint16(extreme[2:], 65535)
	binary.LittleEndian.PutUint16(extreme[6:], 32768)
	metadata = encoderTestMetadata(1, 4, 1, 16, 16, 0, "MONOCHROME2")
	encoded, err = NewSV1Encoder().EncodeFrame(context.Background(), extreme, metadata)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeEncoded(t, newSV1(), encoded.Data, metadata); !bytes.Equal(got, extreme) {
		t.Fatalf("extreme round trip %v", got)
	}
}

func TestEncoderRejectsUnsupportedMetadata(t *testing.T) {
	valid := encoderTestMetadata(2, 2, 1, 8, 8, 0, "MONOCHROME2")
	cases := map[string]func(*pixeldata.Metadata){
		"bits allocated": func(m *pixeldata.Metadata) { m.BitsAllocated = 12 },
		"one bit stored": func(m *pixeldata.Metadata) { m.BitsStored, m.HighBit = 1, 0 },
		"high bit":       func(m *pixeldata.Metadata) { m.HighBit = 6 },
		"palette":        func(m *pixeldata.Metadata) { m.PhotometricInterpretation = "PALETTE COLOR" },
		"signed color": func(m *pixeldata.Metadata) {
			m.SamplesPerPixel, m.PhotometricInterpretation, m.PixelRepresentation = 3, "RGB", 1
		},
		"planar color": func(m *pixeldata.Metadata) {
			m.SamplesPerPixel, m.PhotometricInterpretation, m.PlanarConfiguration, m.PlanarConfigurationPresent = 3, "RGB", 1, true
		},
		"no frames":         func(m *pixeldata.Metadata) { m.NumberOfFrames = 0 },
		"samples per pixel": func(m *pixeldata.Metadata) { m.SamplesPerPixel = 2 },
	}
	for name, change := range cases {
		metadata := valid
		change(&metadata)
		frame := make([]byte, int(metadata.Rows)*int(metadata.Columns)*int(max(metadata.SamplesPerPixel, 1))*2)
		if _, err := NewSV1Encoder().EncodeFrame(context.Background(), frame, metadata); !errors.Is(err, pixeldata.ErrUnsupportedEncoderMetadata) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := NewSV1Encoder().EncodeFrame(context.Background(), []byte{1, 2, 3}, valid); !errors.Is(err, pixeldata.ErrPixelDataSizeMismatch) {
		t.Errorf("frame length: %v", err)
	}
	if _, err := NewProcess14Encoder(8); !errors.Is(err, pixeldata.ErrUnsupportedEncoderMetadata) {
		t.Errorf("selection value 8: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewSV1Encoder().EncodeFrame(ctx, make([]byte, 4), valid); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

func TestOptimalHuffmanSpecLimitsLengthsAndKeepsAllOnesFree(t *testing.T) {
	// Fibonacci frequencies force lengths beyond 16 before limiting.
	var frequencies [17]int
	a, b := 1, 1
	for i := range frequencies {
		frequencies[i] = a
		a, b = b, a+b
	}
	spec := optimalHuffmanSpec(frequencies)
	total, kraft := 0, 0.0
	for length := 1; length <= 16; length++ {
		total += int(spec.counts[length])
		kraft += float64(spec.counts[length]) / float64(uint(1)<<length)
	}
	if total != 17 || len(spec.symbols) != 17 || kraft >= 1 {
		t.Fatalf("codes=%d symbols=%d kraft=%v", total, len(spec.symbols), kraft)
	}
	// One used symbol still gets a code that is not all ones.
	var single [17]int
	single[0] = 100
	spec = optimalHuffmanSpec(single)
	if spec.counts[1] != 1 || len(spec.symbols) != 1 {
		t.Fatalf("single symbol spec %+v", spec)
	}
	if code := spec.codes(); code.length[0] != 1 || code.code[0] != 0 {
		t.Fatalf("single symbol code %+v", code)
	}
}

func BenchmarkEncodeSV1MR16(b *testing.B) {
	metadata := encoderTestMetadata(512, 512, 1, 16, 12, 0, "MONOCHROME2")
	frame := smoothTestFrame(metadata)
	encoder := NewSV1Encoder()
	b.SetBytes(int64(len(frame)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := encoder.EncodeFrame(context.Background(), frame, metadata); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodeSV1RGB8(b *testing.B) {
	metadata := encoderTestMetadata(480, 640, 3, 8, 8, 0, "RGB")
	frame := smoothTestFrame(metadata)
	encoder := NewSV1Encoder()
	b.SetBytes(int64(len(frame)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := encoder.EncodeFrame(context.Background(), frame, metadata); err != nil {
			b.Fatal(err)
		}
	}
}

// smoothTestFrame is an image-like frame (gradients with mild texture) for
// benchmarks.
func smoothTestFrame(metadata pixeldata.Metadata) []byte {
	rows, columns, samples := int(metadata.Rows), int(metadata.Columns), int(metadata.SamplesPerPixel)
	width := int(metadata.BitsAllocated / 8)
	maxValue := 1<<metadata.BitsStored - 1
	out := make([]byte, rows*columns*samples*width)
	for y := 0; y < rows; y++ {
		for x := 0; x < columns; x++ {
			for c := 0; c < samples; c++ {
				value := ((x*(c+3) + y*5 + (x*y)%17) * maxValue / (columns*(c+3) + rows*5 + 17)) & maxValue
				i := (y*columns+x)*samples + c
				if width == 2 {
					binary.LittleEndian.PutUint16(out[2*i:], uint16(value))
				} else {
					out[i] = byte(value)
				}
			}
		}
	}
	return out
}

func FuzzEncoderRoundTrip(f *testing.F) {
	f.Add([]byte{0, 255, 17, 3, 200, 9}, uint8(1), uint8(12))
	f.Add([]byte{255, 255, 0, 0}, uint8(6), uint8(16))
	f.Add(encoderTestFrame(encoderTestMetadata(13, 17, 3, 16, 12, 0, "RGB"), 7), uint8(0x81), uint8(12))
	f.Fuzz(func(t *testing.T, data []byte, predictor, stored uint8) {
		selection := int(predictor%7) + 1
		bits := uint16(stored%15) + 2
		allocated := uint16(8)
		if bits > 8 {
			allocated = 16
		}
		components, photometric := uint16(1), "MONOCHROME2"
		if predictor&0x80 != 0 {
			components, photometric = 3, "RGB"
		}
		width := int(allocated/8) * int(components)
		rows := 1 + int(stored/16%8)
		columns := len(data) / (width * rows)
		if columns == 0 || columns > 256 {
			return
		}
		metadata := encoderTestMetadata(uint16(rows), uint16(columns), components, allocated, bits, 0, photometric)
		frame := data[:rows*columns*width]
		encoder, _ := NewProcess14Encoder(selection)
		encoded, err := encoder.EncodeFrame(context.Background(), frame, metadata)
		if err != nil {
			t.Fatal(err)
		}
		if got := decodeEncoded(t, New(), encoded.Data, metadata); !bytes.Equal(got, canonical(frame, metadata)) {
			t.Fatalf("round trip differs for predictor %d, %d bits", selection, bits)
		}
	})
}
