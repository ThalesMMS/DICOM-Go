package jpeg

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Generated independently with libjpeg-turbo 3.1.0 using 12-bit precision,
// quality 100 and a restart interval of one block. The 16x8 source sample at
// (x,y) is x*256+y*16.
const process4RestartFixtureBase64 = "/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAAEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQH/wQALDAAIABABAREA/8QAFQABAQAAAAAAAAAAAAAAAAAADQ7/xAAcEAAABgMAAAAAAAAAAAAAAAAACQ0lOURYYnb/3QAEAAH/2gAIAQEAAD8Anb9Nxi24F2DG2tzWo//QdwE3GLbgXYMba3Naj//Z"

func TestJPEGExtendedProcess4DecodesRestartIntervals(t *testing.T) {
	stream, err := base64.StdEncoding.DecodeString(process4RestartFixtureBase64)
	if err != nil {
		t.Fatal(err)
	}
	obj, pixel := process4ObjectNoTest(stream, 8, 16, 16, 12, 11, 0)
	frames, err := New().Decode(pixel, obj)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames.Data) != 1 || len(frames.Data[0]) != 16*8*2 {
		t.Fatalf("decoded frame layout = %d frames/%d bytes", len(frames.Data), len(frames.Data[0]))
	}
	for y := 0; y < 8; y++ {
		for x := 0; x < 16; x++ {
			index := y*16 + x
			got := int(binary.LittleEndian.Uint16(frames.Data[0][index*2:]))
			want := x*256 + y*16
			if delta := got - want; delta < -1 || delta > 1 {
				t.Fatalf("sample (%d,%d) = %d, want %d (+/-1)", x, y, got, want)
			}
		}
	}
}

func TestRegisteredJPEGCodecsKeepBaselineAndExtendedModesSeparate(t *testing.T) {
	sof0 := encodeGrayJPEG(t, 2, 1, []byte{0, 255})
	sof1 := makeJPEGExtendedSOF1(t, encodeGrayJPEG(t, 2, 1, []byte{0, 255}))
	obj, pixel := jpegObjectWithFragments(t, 1, 2, sof1)
	registry := pixeldata.NewMemoryRegistry()
	if err := Register(registry); err != nil {
		t.Fatal(err)
	}

	if _, err := registry.DecodeFrames(transfer.JPEGBaseline.UID, pixel, obj); !errors.Is(err, ErrInvalidFragment) {
		t.Fatalf("Baseline decoding SOF1 error = %v, want ErrInvalidFragment", err)
	}
	if _, err := registry.DecodeFrames(transfer.JPEGExtended.UID, pixel, obj); err != nil {
		t.Fatalf("Extended decoding SOF1 error = %v", err)
	}

	baselineObject, baselinePixel := jpegObjectWithFragments(t, 1, 2, sof0)
	if _, err := registry.DecodeFrames(transfer.JPEGBaseline.UID, baselinePixel, baselineObject); err != nil {
		t.Fatalf("Baseline decoding SOF0 error = %v", err)
	}
	// Baseline is a subset of Process 2 & 4: SOF0 decodes under .51 with an
	// informative diagnostic.
	frames, err := registry.DecodeFrames(transfer.JPEGExtended.UID, baselinePixel, baselineObject)
	if err != nil {
		t.Fatalf("Extended decoding SOF0 error = %v", err)
	}
	if len(frames.Diagnostics) != 1 || frames.Diagnostics[0] != pixeldata.DiagnosticJPEGBaselineUnderExtended {
		t.Fatalf("diagnostics = %v", frames.Diagnostics)
	}
}

func withSOF(t *testing.T, stream []byte, code byte) []byte {
	t.Helper()
	out := append([]byte(nil), stream...)
	i := bytes.Index(out, []byte{0xff, 0xc0})
	if i < 0 {
		t.Fatal("no SOF0")
	}
	out[i+1] = code
	return out
}

func TestJPEGProcessBySyntaxTable(t *testing.T) {
	sof0 := encodeGrayJPEG(t, 2, 1, []byte{0, 255})
	streams := map[string][]byte{
		"SOF0": sof0,
		"SOF1": makeJPEGExtendedSOF1(t, sof0),
		"SOF2": withSOF(t, sof0, 0xc2),
		"SOF3": withSOF(t, sof0, 0xc3),
	}
	registry, err := builtinLikeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		uid  string
		ok   map[string]bool
		name string
	}{
		{transfer.JPEGBaseline.UID, map[string]bool{"SOF0": true}, ".50"},
		{transfer.JPEGExtended.UID, map[string]bool{"SOF0": true, "SOF1": true}, ".51"},
	}
	for _, c := range cases {
		for name, stream := range streams {
			obj, pixel := jpegObjectWithFragments(t, 1, 2, stream)
			_, err := registry.DecodeFrames(c.uid, pixel, obj)
			if c.ok[name] && err != nil {
				t.Errorf("%s %s: %v", c.name, name, err)
			}
			if !c.ok[name] && err == nil {
				t.Errorf("%s %s: accepted", c.name, name)
			}
		}
	}
}

func builtinLikeRegistry() (*pixeldata.MemoryRegistry, error) {
	registry := pixeldata.NewMemoryRegistry()
	return registry, Register(registry)
}

func TestJPEGExtendedProcess4RejectsDuplicateAndOutOfOrderMarkers(t *testing.T) {
	base := process4Fixture(t)
	dqt := jpegMarkerBytes(t, base, 0xdb)
	sof := jpegMarkerBytes(t, base, 0xc1)
	sos := jpegMarkerBytes(t, base, 0xda)
	eoi := bytes.Index(base, []byte{0xff, 0xd9})
	if eoi < 0 {
		t.Fatal("fixture has no EOI")
	}
	restart, err := base64.StdEncoding.DecodeString(process4RestartFixtureBase64)
	if err != nil {
		t.Fatal(err)
	}
	dri := jpegMarkerBytes(t, restart, 0xdd)

	tests := []struct {
		name          string
		stream        []byte
		rows, columns uint16
	}{
		{name: "duplicate SOF1", stream: insertJPEGBytes(base, bytes.Index(base, sos), sof), rows: 8, columns: 8},
		{name: "duplicate SOS", stream: insertJPEGBytes(base, eoi, sos), rows: 8, columns: 8},
		{name: "DQT after final scan", stream: insertJPEGBytes(base, eoi, dqt), rows: 8, columns: 8},
		{name: "DHT after final scan", stream: insertJPEGBytes(base, eoi, jpegMarkerBytes(t, base, 0xc4)), rows: 8, columns: 8},
		{name: "DRI after final scan", stream: insertJPEGBytes(base, eoi, dri), rows: 8, columns: 8},
		{name: "duplicate DRI", stream: insertJPEGBytes(restart, bytes.Index(restart, dri)+len(dri), dri), rows: 8, columns: 16},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			obj, pixel := process4ObjectNoTest(test.stream, test.rows, test.columns, 16, 12, 11, 0)
			if _, err := New().Decode(pixel, obj); err == nil {
				t.Fatal("Decode() error = nil")
			}
		})
	}
}

func TestJPEGExtendedProcess4TablesBetweenScans(t *testing.T) {
	// Repeat an independently encoded gray scan for each RGB component.
	// Each scan redefines the same table IDs and uses its own DRI/RST0.
	base := color12Fixture(t, process4RestartFixtureBase64)
	dqt := jpegMarkerBytes(t, base, 0xdb)
	dc := jpegMarkerBytes(t, base, 0xc4)
	ac := jpegMarkerBytes(t, base[bytes.Index(base, dc)+len(dc):], 0xc4)
	dri := jpegMarkerBytes(t, base, 0xdd)
	sos := jpegMarkerBytes(t, base, 0xda)
	entropy := base[bytes.Index(base, sos)+len(sos) : bytes.Index(base, []byte{0xff, 0xd9})]
	stream := []byte{0xff, 0xd8, 0xff, 0xc1, 0, 17, 12, 0, 8, 0, 16, 3, 'R', 0x11, 0, 'G', 0x11, 0, 'B', 0x11, 0}
	for _, id := range []byte{'R', 'G', 'B'} {
		stream = append(stream, dqt...)
		stream = append(stream, dc...)
		stream = append(stream, ac...)
		stream = append(stream, dri...)
		sos[5] = id
		stream = append(stream, sos...)
		stream = append(stream, entropy...)
	}
	stream = append(stream, 0xff, 0xd9)
	obj, pixel := colorJPEGObject(t, stream, 8, 16, 16, 12, "RGB")
	frames, err := New().Decode(pixel, obj)
	if err != nil {
		t.Fatal(err)
	}
	for y := range 8 {
		for x := range 16 {
			for c := range 3 {
				got := int(binary.LittleEndian.Uint16(frames.Data[0][((y*16+x)*3+c)*2:]))
				if delta := got - (x*256 + y*16); delta < -1 || delta > 1 {
					t.Fatalf("sample (%d,%d,%d) = %d", x, y, c, got)
				}
			}
		}
	}
}

func TestJPEGExtendedProcess4AcceptsOneArbitraryDICOMPaddingByte(t *testing.T) {
	base := process4Fixture(t)
	for _, padding := range []byte{0x00, 0xa2, 0xff} {
		stream := append(append([]byte(nil), base...), padding)
		if _, err := decodeProcess4Fixture(stream, 8, 8, 12); err != nil {
			t.Fatalf("padding 0x%02x: %v", padding, err)
		}
	}
	stream := append(append([]byte(nil), base...), 0, 0)
	if _, err := decodeProcess4Fixture(stream, 8, 8, 12); err == nil {
		t.Fatal("two padding bytes accepted")
	}
}

func TestJPEGExtendedProcess4RejectsCompleteHuffmanCodeTree(t *testing.T) {
	// Two one-bit codes consume the entire tree, including the all-ones code
	// that JPEG reserves so fill bits cannot become a symbol.
	segment := make([]byte, 0, 19)
	segment = append(segment, 0x00, 0x02)
	segment = append(segment, make([]byte, 15)...)
	segment = append(segment, 0x00, 0x01)
	if err := (&extendedProcess4Decoder{}).parseDHT(segment); err == nil {
		t.Fatal("complete Huffman tree accepted")
	}
}

func TestJPEGExtendedProcess4RejectsOutOfRangeDCHuffmanSymbol(t *testing.T) {
	segment := make([]byte, 0, 18)
	segment = append(segment, 0x00, 0x01)
	segment = append(segment, make([]byte, 15)...)
	segment = append(segment, 0x10)
	if err := (&extendedProcess4Decoder{}).parseDHT(segment); err == nil {
		t.Fatal("DC Huffman symbol 16 accepted")
	}
}

func TestJPEGExtendedProcess4RejectsAggregateRequestAboveLimit(t *testing.T) {
	tests := []struct {
		name      string
		metadata  pixeldata.Metadata
		fragments [][]byte
	}{
		{
			name: "decoded frames accumulate above limit",
			metadata: pixeldata.Metadata{
				Rows: 32768, Columns: 4096, SamplesPerPixel: 1,
				BitsAllocated: 16, BitsStored: 12, HighBit: 11, NumberOfFrames: 2,
			},
			fragments: [][]byte{{0xff, 0xd8}, {0xff, 0xd8}},
		},
		{
			name: "compressed fragments accumulate above limit",
			metadata: pixeldata.Metadata{
				Rows: 1, Columns: 1, SamplesPerPixel: 1,
				BitsAllocated: 16, BitsStored: 12, HighBit: 11, NumberOfFrames: 513,
			},
			fragments: func() [][]byte {
				// Reusing one backing buffer keeps the regression lightweight while
				// exercising the logical retained-input sum.
				fragment := make([]byte, 1<<20)
				fragments := make([][]byte, 513)
				for i := range fragments {
					fragments[i] = fragment
				}
				return fragments
			}(),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateExtendedProcess4Request(test.metadata, test.fragments); !errors.Is(err, ErrInvalidFragment) {
				t.Fatalf("error = %v, want ErrInvalidFragment", err)
			}
		})
	}
}

func jpegMarkerBytes(t *testing.T, stream []byte, marker byte) []byte {
	t.Helper()
	offset := bytes.Index(stream, []byte{0xff, marker})
	if offset < 0 || offset+4 > len(stream) {
		t.Fatalf("marker 0xff%02x not found", marker)
	}
	length := int(binary.BigEndian.Uint16(stream[offset+2:]))
	end := offset + 2 + length
	if length < 2 || end > len(stream) {
		t.Fatalf("marker 0xff%02x length=%d", marker, length)
	}
	return append([]byte(nil), stream[offset:end]...)
}

func insertJPEGBytes(stream []byte, offset int, addition []byte) []byte {
	result := make([]byte, 0, len(stream)+len(addition))
	result = append(result, stream[:offset]...)
	result = append(result, addition...)
	return append(result, stream[offset:]...)
}
