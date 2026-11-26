package vendors

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

func le16(values ...uint16) []byte {
	out := make([]byte, 0, 2*len(values))
	for _, v := range values {
		out = binary.LittleEndian.AppendUint16(out, v)
	}
	return out
}

// encodePMSCTRLE1 is a test encoder of the PMSCT_RLE1 layout: absolute
// samples where a difference does not fit a byte or would read as the 0x5A
// marker, and every 0xA5 byte written as a one-byte run, since the run pass
// sees the absolute values' bytes too. Runs of equal differences use 0xA5.
func encodePMSCTRLE1(samples []uint16) []byte {
	var deltas []byte
	previous := uint16(0)
	for _, s := range samples {
		d := int(int16(s - previous))
		if d >= -128 && d <= 127 && byte(int8(d)) != 0x5A {
			deltas = append(deltas, byte(int8(d)))
		} else {
			deltas = append(deltas, 0x5A, byte(s), byte(s>>8))
		}
		previous = s
	}
	var out []byte
	for i := 0; i < len(deltas); {
		j := i
		for j < len(deltas) && j-i < 256 && deltas[j] == deltas[i] {
			j++
		}
		if n := j - i; n >= 4 || deltas[i] == 0xA5 {
			out = append(out, 0xA5, byte(n-1), deltas[i])
		} else {
			out = append(out, deltas[i:j]...)
		}
		i = j
	}
	return out
}

func TestDecodeElscintPMSCTRLE1(t *testing.T) {
	// 0x5A sets 1000; +5; -3; a run of three +1; a run of one 0x5A byte
	// is the absolute marker of 0xFFFF; +1 wraps to 0.
	stream := []byte{0x5A, 0xE8, 0x03, 0x05, 0xFD, 0xA5, 0x02, 0x01, 0xA5, 0x00, 0x5A, 0xFF, 0xFF, 0x01}
	got, err := DecodeElscintPMSCTRLE1(stream, 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	want := le16(1000, 1005, 1002, 1003, 1004, 1005, 0xFFFF, 0)
	if !bytes.Equal(got, want) {
		t.Fatalf("got % x, want % x", got, want)
	}
	// One pad sample after the frame is dropped.
	if got, err := DecodeElscintPMSCTRLE1(append(stream, 0x00), 2, 4); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("pad sample: %v", err)
	}
	for name, bad := range map[string][]byte{
		"too few samples":     stream[:len(stream)-1],
		"two extra samples":   append(append([]byte(nil), stream...), 0, 0),
		"truncated run":       {0x01, 0xA5, 0x02},
		"truncated absolute":  {0x5A, 0x01},
		"run ends a absolute": {0x5A, 0xA5, 0x00, 0x01},
	} {
		if _, err := DecodeElscintPMSCTRLE1(bad, 2, 4); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := DecodeElscintPMSCTRLE1(stream, 0, 4); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero rows: %v", err)
	}
	// Round trip through the test encoder, including samples whose bytes
	// are 0xA5 or 0x5A and differences of -128, 127 and 90 (0x5A).
	var samples []uint16
	for i := 0; i < 64*48; i++ {
		v := uint16(i*37%4096) + uint16(i%7)
		switch i % 97 {
		case 3:
			v = 0xA5A5
		case 4:
			v = 0x5A5A
		case 5:
			v = 0x5A5A + 90
		case 6:
			v = 0x5A5A + 90 - 128
		case 7:
			v = 0x5A5A + 90 - 128 + 127
		}
		if i/64%5 == 2 {
			v = 700 // flat rows make long runs of zero differences
		}
		samples = append(samples, v)
	}
	got, err = DecodeElscintPMSCTRLE1(encodePMSCTRLE1(samples), 48, 64)
	if err != nil || !bytes.Equal(got, le16(samples...)) {
		t.Fatalf("round trip: %v", err)
	}
}

func TestDecodeElscintPMSCTRGB1(t *testing.T) {
	// Gray +10, +5 (escaped 0x81 difference adds 0x81), repeat 2, toggle to
	// color, (+1,+2,+3), escaped (0x83,0x82,0x81), repeat 1, toggle back to
	// a zero gray, +7, then a trailing pad byte.
	stream := []byte{
		0x0A, 0x05, 0x82, 0x81, 0x83, 0x02,
		0x81, 0x01, 0x02, 0x03, 0x82, 0x83, 0x82, 0x81, 0x83, 0x01,
		0x81, 0x07, 0x00,
	}
	gray := func(v byte) [3]byte { return [3]byte{v, v, v} }
	pixels := [][3]byte{gray(10), gray(15), gray(0x90), gray(0x90), gray(0x90), {1, 2, 3}, {0x84, 0x84, 0x84}, {0x84, 0x84, 0x84}, gray(7)}
	const rows, columns = 3, 3
	var interleaved, planar []byte
	for _, p := range pixels {
		interleaved = append(interleaved, p[:]...)
	}
	for c := 0; c < 3; c++ {
		for _, p := range pixels {
			planar = append(planar, p[c])
		}
	}
	got, err := DecodeElscintPMSCTRGB1(stream, rows, columns, false)
	if err != nil || !bytes.Equal(got, interleaved) {
		t.Fatalf("interleaved: % x %v, want % x", got, err, interleaved)
	}
	if got, err = DecodeElscintPMSCTRGB1(stream, rows, columns, true); err != nil || !bytes.Equal(got, planar) {
		t.Fatalf("planar: % x %v", got, err)
	}
	for name, bad := range map[string][]byte{
		"truncated":         stream[:10],
		"repeat overflow":   {0x83, 10},
		"truncated repeat":  {0x0A, 0x83},
		"truncated color":   {0x81, 0x01, 0x02},
		"truncated escape":  {0x82},
		"no pixels decoded": {0x81, 0x81},
	} {
		if _, err := DecodeElscintPMSCTRGB1(bad, rows, columns, false); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// elscintObject returns an object naming compression with stream in the
// ELSCINT1 private element (or, with standard, in Pixel Data).
func elscintObject(compression string, stream []byte, rows, columns, samples, bits uint16, standard bool) *object.Object {
	dataset := object.New(std.Dictionary)
	us := func(element, value uint16) {
		dataset.Put(core.NewRawElement(core.NewTag(0x0028, element), core.VRUS, le16(value)))
	}
	us(0x0002, samples)
	photometric := "MONOCHROME2 "
	if samples == 3 {
		photometric = "RGB "
		us(0x0006, 0)
	}
	dataset.Put(core.NewRawElement(core.NewTag(0x0028, 0x0004), core.VRCS, []byte(photometric)))
	us(0x0010, rows)
	us(0x0011, columns)
	us(0x0100, bits)
	stored := bits
	if bits == 16 {
		stored = 12
	}
	us(0x0101, stored)
	us(0x0102, stored-1)
	us(0x0103, 0)
	dataset.Put(core.NewRawElement(core.NewTag(0x07A1, 0x0010), core.VRLO, []byte("ELSCINT1")))
	if compression != "" {
		dataset.Put(core.NewRawElement(core.NewTag(0x07A1, 0x1011), core.VRCS, []byte(compression)))
	}
	if len(stream)%2 == 1 {
		stream = append(stream, 0)
	}
	if standard {
		dataset.Put(core.NewRawElement(core.TagPixelData, core.VROB, stream))
	} else {
		dataset.Put(core.NewRawElement(core.NewTag(0x07A1, 0x100A), core.VROB, stream))
	}
	return dataset
}

func TestElscintPixelDataOf(t *testing.T) {
	samples := []uint16{100, 101, 103, 0xA5A5, 2, 2, 2, 2, 2, 4095, 0, 7}
	stream := encodePMSCTRLE1(samples)
	dataset := elscintObject(ElscintPMSCTRLE1, stream, 3, 4, 1, 16, false)
	pixels, err := ElscintPixelDataOf(dataset, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if pixels.Compression != ElscintPMSCTRLE1 || pixels.Source != core.NewTag(0x07A1, 0x100A) || !bytes.Equal(pixels.Frame, le16(samples...)) {
		t.Fatalf("RLE1: %+v", pixels)
	}
	if len(pixels.Diagnostics) != 1 || pixels.Diagnostics[0] != DiagnosticElscintPrivatePixels || pixels.Metadata.Rows != 3 || pixels.Metadata.Columns != 4 {
		t.Fatalf("RLE1 diagnostics %v metadata %+v", pixels.Diagnostics, pixels.Metadata)
	}
	if dataset.Has(core.TagPixelData) {
		t.Fatal("decoding added Pixel Data")
	}

	// The compressed stream in Pixel Data, as rle2img also reads it.
	standard := elscintObject(ElscintPMSCTRLE1, stream, 3, 4, 1, 16, true)
	pixels, err = ElscintPixelDataOf(standard, Limits{})
	if err != nil || pixels.Source != core.TagPixelData || !bytes.Equal(pixels.Frame, le16(samples...)) || pixels.Diagnostics[1] != DiagnosticElscintStandardPixelData {
		t.Fatalf("standard Pixel Data: %+v %v", pixels, err)
	}
	// Native-length Pixel Data is not this decoder's.
	if _, err := ElscintPixelDataOf(elscintObject(ElscintPMSCTRLE1, le16(samples...), 3, 4, 1, 16, true), Limits{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("native Pixel Data: %v", err)
	}
	// A private value with the native length is stored as is.
	pixels, err = ElscintPixelDataOf(elscintObject(ElscintPMSCTRLE1, le16(samples...), 3, 4, 1, 16, false), Limits{})
	if err != nil || !bytes.Equal(pixels.Frame, le16(samples...)) || pixels.Diagnostics[len(pixels.Diagnostics)-1] != DiagnosticElscintStored {
		t.Fatalf("stored: %+v %v", pixels, err)
	}

	rgb := elscintObject(ElscintPMSCTRGB1, []byte{0x81, 1, 2, 3, 0x83, 3}, 2, 2, 3, 8, false)
	pixels, err = ElscintPixelDataOf(rgb, Limits{})
	if err != nil || !bytes.Equal(pixels.Frame, bytes.Repeat([]byte{1, 2, 3}, 4)) {
		t.Fatalf("RGB1: %+v %v", pixels, err)
	}

	for name, tc := range map[string]struct {
		dataset *object.Object
		limits  Limits
		want    error
	}{
		"no compression type":    {elscintObject("", stream, 3, 4, 1, 16, false), Limits{}, ErrNotFound},
		"unknown compression":    {elscintObject("LOSSLESS RICE", stream, 3, 4, 1, 16, false), Limits{}, ErrInvalid},
		"RLE1 with 8-bit pixels": {elscintObject(ElscintPMSCTRLE1, stream, 3, 4, 1, 8, false), Limits{}, ErrInvalid},
		"RGB1 with monochrome":   {elscintObject(ElscintPMSCTRGB1, stream, 3, 4, 1, 8, false), Limits{}, ErrInvalid},
		"malformed stream":       {elscintObject(ElscintPMSCTRLE1, []byte{0x5A}, 3, 4, 1, 16, false), Limits{}, ErrInvalid},
		"stream beyond limit":    {elscintObject(ElscintPMSCTRLE1, stream, 3, 4, 1, 16, false), Limits{MaxBytes: 4}, ErrLimit},
		"frame beyond limit":     {elscintObject(ElscintPMSCTRLE1, stream, 3, 4, 1, 16, false), Limits{MaxDecodedBytes: 23}, ErrLimit},
		"nil dataset":            {nil, Limits{}, ErrNotFound},
	} {
		if _, err := ElscintPixelDataOf(tc.dataset, tc.limits); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	missing := elscintObject(ElscintPMSCTRLE1, stream, 3, 4, 1, 16, false)
	missing.Remove(core.NewTag(0x07A1, 0x100A))
	if _, err := ElscintPixelDataOf(missing, Limits{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no stream: %v", err)
	}
}

func FuzzElscintPixels(f *testing.F) {
	f.Add(encodePMSCTRLE1([]uint16{1, 2, 0xA5A5, 0x5A5A}), false)
	f.Add([]byte{0x0A, 0x81, 1, 2, 3, 0x83, 2, 0x82, 0x81, 0x81, 0x81}, true)
	f.Fuzz(func(t *testing.T, data []byte, rgb bool) {
		const rows, columns = 2, 3
		if rgb {
			if frame, err := DecodeElscintPMSCTRGB1(data, rows, columns, false); err == nil && len(frame) != rows*columns*3 {
				t.Fatalf("%d bytes", len(frame))
			}
			return
		}
		if frame, err := DecodeElscintPMSCTRLE1(data, rows, columns); err == nil && len(frame) != rows*columns*2 {
			t.Fatalf("%d bytes", len(frame))
		}
	})
}
