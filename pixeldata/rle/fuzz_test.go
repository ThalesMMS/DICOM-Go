package rle

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/decoderecovery"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// fuzzLayout is one image layout the RLE fuzz targets decode against.
type fuzzLayout struct {
	samples, bits uint16
	photometric   string
}

// fuzzLayouts lists the supported layouts first (up to the 15-segment
// limit) and then layouts the decoder must refuse from the metadata.
var fuzzLayouts = []fuzzLayout{
	{1, 8, "MONOCHROME2"},
	{1, 16, "MONOCHROME2"},
	{3, 8, "RGB"},
	{3, 16, "RGB"},
	{1, 32, "MONOCHROME1"},
	{3, 40, "YBR_FULL"},
	{1, 120, "MONOCHROME2"},
	{1, 8, "PALETTE COLOR"},
	// Refused: 12-bit samples, two samples, 18 segments, subsampled YBR.
	{1, 12, "MONOCHROME2"},
	{2, 8, "MONOCHROME2"},
	{3, 48, "RGB"},
	{3, 8, "YBR_FULL_422"},
}

const supportedFuzzLayouts = 8

func (l fuzzLayout) segments() int { return int(l.samples) * int(l.bits) / 8 }

// metadata describes a rows×columns frame of the layout.
func (l fuzzLayout) metadata(rows, columns int) pixeldata.Metadata {
	return encoderMetadata(uint16(rows), uint16(columns), l.samples, l.bits, l.photometric)
}

// fuzzDimensions keeps fuzzed frames small (at most 64×64 and 15 segments)
// so every execution stays well below a megabyte of decoded pixels.
func fuzzDimensions(rows, columns uint8) (int, int) {
	return int(rows % 65), int(columns % 65)
}

// rleSeed is a malformed or boundary RLE frame. The bytes are written here
// from the RLE layout of DICOM PS3.5 Annex G (a 64-byte header with a
// segment count and 15 offsets, then PackBits segments); provenance names
// the class of DCMTK decoder fix the seed exercises, with the commits and
// classes as listed in go-dev issue #1023. No DCMTK code or test file is
// used.
type rleSeed struct {
	name       string
	provenance string
	layout     uint8
	rows, cols uint8
	fragment   []byte
	// strict is the error of a strict decode (nil: it decodes).
	strict error
	// recovered is set when the opt-in RLESegmentOverrun recovery decodes
	// what strict decoding refuses, reporting the recovery.
	recovered bool
	// want is the decoded frame, strict or recovered, when checked.
	want []byte
}

// header returns a 64-byte RLE header with the given count and offsets.
func header(count uint32, offsets ...uint32) []byte {
	return rleHeaderOnly(count, offsets...)
}

// withBody appends segment bytes to a hand-written header.
func withBody(head []byte, body ...byte) []byte {
	return append(slices.Clone(head), body...)
}

// rleSeeds are the DCMTK-modelled regression seeds, shared by the fuzz
// targets and TestRLEMalformedSeeds. Layout 0 is 8-bit monochrome, 1 is
// 16-bit monochrome and 3 is three 16-bit samples; frames are 2×2 unless
// stated.
func rleSeeds() []rleSeed {
	const (
		headerFix     = "defective RLE header (DCMTK 92fc86e9e)"
		stripeFix     = "invalid RLE stripe (DCMTK bffa3e911, f93cf77f1)"
		zeroFix       = "zero-length RLE stripe (DCMTK 2846f2914, 45469f3c3)"
		paddingFix    = "uninitialized output after a short stripe (DCMTK df10c63ca)"
		annexG        = "PS3.5 Annex G boundary"
		mono8, mono16 = 0, 1
		rgb16         = 3
	)
	plane := []byte{3, 1, 2, 3, 4} // one literal run filling a 2×2 plane
	return []rleSeed{
		{name: "valid mono8", provenance: annexG, layout: mono8, rows: 2, cols: 2, fragment: rleFragment([]byte{1, 2, 3, 4}), want: []byte{1, 2, 3, 4}},
		{name: "empty fragment", provenance: headerFix, layout: mono8, rows: 2, cols: 2, fragment: nil, strict: ErrInvalidHeader},
		{name: "header shorter than 64 bytes", provenance: headerFix, layout: mono8, rows: 2, cols: 2, fragment: header(1, 64)[:63], strict: ErrInvalidHeader},
		{name: "zero segments", provenance: headerFix, layout: mono8, rows: 2, cols: 2, fragment: withBody(header(0), plane...), strict: ErrInvalidSegmentCount},
		{name: "sixteen segments", provenance: headerFix, layout: mono8, rows: 2, cols: 2, fragment: withBody(header(16, 64, 64, 64, 64, 64, 64, 64, 64, 64, 64, 64, 64, 64, 64, 64), plane...), strict: ErrInvalidSegmentCount},
		{name: "segment count 0xFFFFFFFF", provenance: headerFix, layout: mono8, rows: 2, cols: 2, fragment: withBody(header(0xFFFFFFFF, 64), plane...), strict: ErrInvalidSegmentCount},
		{name: "offset zero", provenance: headerFix, layout: mono8, rows: 2, cols: 2, fragment: withBody(header(1, 0), plane...), strict: ErrInvalidSegmentOffset},
		{name: "offset inside header", provenance: headerFix, layout: mono8, rows: 2, cols: 2, fragment: withBody(header(1, 63), plane...), strict: ErrInvalidSegmentOffset},
		{name: "offset past fragment", provenance: headerFix, layout: mono8, rows: 2, cols: 2, fragment: withBody(header(1, 70), plane...), strict: ErrInvalidSegmentOffset},
		{name: "offset 0xFFFFFFFF", provenance: headerFix, layout: mono8, rows: 2, cols: 2, fragment: withBody(header(1, 0xFFFFFFFF), plane...), strict: ErrInvalidSegmentOffset},
		{name: "offsets not monotonic", provenance: headerFix, layout: mono16, rows: 2, cols: 2, fragment: withBody(header(2, 69, 64), append(slices.Clone(plane), plane...)...), strict: ErrInvalidSegmentOffset},
		{name: "count below samples times bytes", provenance: headerFix, layout: rgb16, rows: 2, cols: 2, fragment: rleFragment([]byte{1, 2, 3, 4}, []byte{1, 2, 3, 4}, []byte{1, 2, 3, 4}), strict: ErrInvalidSegmentCount},
		{name: "count above samples times bytes", provenance: headerFix, layout: mono8, rows: 2, cols: 2, fragment: rleFragment([]byte{1, 2, 3, 4}, []byte{1, 2, 3, 4}), strict: ErrInvalidSegmentCount},
		{name: "offset equals fragment length", provenance: zeroFix, layout: mono8, rows: 2, cols: 2, fragment: header(1, 64), strict: ErrSegmentDecodeFailed, recovered: true, want: []byte{0, 0, 0, 0}},
		{name: "equal offsets give a zero-length stripe", provenance: zeroFix, layout: mono16, rows: 2, cols: 2, fragment: withBody(header(2, 64, 64), plane...), strict: ErrSegmentDecodeFailed, recovered: true, want: []byte{1, 0, 2, 0, 3, 0, 4, 0}},
		{name: "stripe one byte short", provenance: stripeFix, layout: mono8, rows: 2, cols: 2, fragment: withBody(header(1, 64), 3, 1, 2, 3), strict: ErrSegmentDecodeFailed, recovered: true, want: []byte{1, 2, 3, 0}},
		{name: "stripe one run short", provenance: stripeFix, layout: mono8, rows: 2, cols: 2, fragment: withBody(header(1, 64), 2, 1, 2, 3), strict: ErrSegmentDecodeFailed, recovered: true, want: []byte{1, 2, 3, 0}},
		{name: "literal run overruns the frame", provenance: stripeFix, layout: mono8, rows: 2, cols: 2, fragment: withBody(header(1, 64), 5, 1, 2, 3, 4, 5, 6), strict: ErrSegmentDecodeFailed, recovered: true, want: []byte{1, 2, 3, 4}},
		{name: "replicate run overruns the frame", provenance: stripeFix, layout: mono8, rows: 2, cols: 2, fragment: withBody(header(1, 64), 0x81, 9), strict: ErrSegmentDecodeFailed, recovered: true, want: []byte{9, 9, 9, 9}},
		{name: "replicate run without its value", provenance: stripeFix, layout: mono8, rows: 2, cols: 2, fragment: withBody(header(1, 64), 1, 5, 6, 0xFF), strict: ErrSegmentDecodeFailed, recovered: true, want: []byte{5, 6, 0, 0}},
		{name: "only no-op runs", provenance: paddingFix, layout: mono8, rows: 2, cols: 2, fragment: withBody(header(1, 64), 0x80, 0x80, 0x80, 0x80), strict: ErrSegmentDecodeFailed, recovered: true, want: []byte{0, 0, 0, 0}},
		{name: "last of six stripes short", provenance: paddingFix, layout: rgb16, rows: 1, cols: 2, fragment: rleFragmentEncoded([]byte{1, 1, 2}, []byte{1, 3, 4}, []byte{1, 5, 6}, []byte{1, 7, 8}, []byte{1, 9, 10}, []byte{0, 11}), strict: ErrSegmentDecodeFailed, recovered: true, want: []byte{3, 1, 7, 5, 11, 9, 4, 2, 8, 6, 0, 10}},
		{name: "three 16-bit samples", provenance: annexG, layout: rgb16, rows: 1, cols: 2, fragment: rleFragmentEncoded([]byte{1, 1, 2}, []byte{1, 3, 4}, []byte{1, 5, 6}, []byte{1, 7, 8}, []byte{1, 9, 10}, []byte{1, 11, 12}), want: []byte{3, 1, 7, 5, 11, 9, 4, 2, 8, 6, 12, 10}},
		{name: "odd-length fragment without pad", provenance: annexG, layout: mono8, rows: 1, cols: 2, fragment: withBody(header(1, 64), 1, 7, 8), want: []byte{7, 8}},
		{name: "runs after a complete stripe", provenance: annexG, layout: mono8, rows: 2, cols: 2, fragment: withBody(header(1, 64), 3, 1, 2, 3, 4, 0x81, 9, 0), want: []byte{1, 2, 3, 4}},
		{name: "unused offsets not zero", provenance: annexG, layout: mono8, rows: 2, cols: 2, fragment: withBody(header(1, 64, 64, 0xFFFFFFFF), plane...), want: []byte{1, 2, 3, 4}},
		{name: "replicate runs of 128 across rows", provenance: annexG, layout: mono8, rows: 4, cols: 64, fragment: withBody(header(1, 64), 0x81, 1, 0x81, 2), want: slices.Concat(bytes.Repeat([]byte{1}, 128), bytes.Repeat([]byte{2}, 128))},
		{name: "tiny stripe for a 64x64 frame", provenance: stripeFix, layout: mono16, rows: 64, cols: 64, fragment: withBody(header(2, 64, 66), 0x81, 1, 0x81, 2), strict: ErrSegmentDecodeFailed, recovered: true},
	}
}

func TestRLEMalformedSeeds(t *testing.T) {
	for _, seed := range rleSeeds() {
		t.Run(seed.name, func(t *testing.T) {
			layout := fuzzLayouts[seed.layout]
			rows, cols := int(seed.rows), int(seed.cols)
			strict, strictRecovered, err := decodeFrame(context.Background(), seed.fragment, rows, cols, int(layout.samples), int(layout.bits), false)
			if !errors.Is(err, seed.strict) || (seed.strict == nil && err != nil) {
				t.Fatalf("strict error %v, want %v (%s)", err, seed.strict, seed.provenance)
			}
			if strictRecovered {
				t.Fatal("strict decode reported a recovery")
			}
			got := strict
			if seed.strict != nil {
				tolerant, recovered, err := decodeFrame(context.Background(), seed.fragment, rows, cols, int(layout.samples), int(layout.bits), true)
				if seed.recovered != (err == nil) || recovered != seed.recovered {
					t.Fatalf("recovery: error %v recovered %v, want recovered %v", err, recovered, seed.recovered)
				}
				if err != nil && !errors.Is(err, seed.strict) {
					t.Fatalf("tolerant error %v, want %v", err, seed.strict)
				}
				got = tolerant
			}
			if seed.want != nil && !bytes.Equal(got, seed.want) {
				t.Fatalf("decoded % x, want % x", got, seed.want)
			}
			checkRLEFrame(t, seed.fragment, seed.layout, seed.rows, seed.cols, 0)
		})
	}
}

// FuzzDecodeRLEFrame decodes one arbitrary RLE frame, strictly and with the
// opt-in overrun recovery, as a whole and as a region.
func FuzzDecodeRLEFrame(f *testing.F) {
	for _, seed := range rleSeeds() {
		f.Add(seed.fragment, seed.layout, seed.rows, seed.cols, uint32(0x01020001))
	}
	f.Add(rleFragmentEncoded([]byte{0x81, 7}, []byte{3, 1}), uint8(1), uint8(2), uint8(2), uint32(0))
	f.Fuzz(checkRLEFrame)
}

// checkRLEFrame is the FuzzDecodeRLEFrame property: decoding never panics,
// fails with the package's errors, returns exactly one frame's bytes, never
// reports a recovery unless it was requested, recovers every run-level
// failure when asked and reports it, and a region decode is the crop of the
// full decode.
func checkRLEFrame(t *testing.T, fragment []byte, layoutIndex, rowsByte, colsByte uint8, window uint32) {
	layout := fuzzLayouts[int(layoutIndex)%supportedFuzzLayouts]
	rows, cols := fuzzDimensions(rowsByte, colsByte)
	samples, bits := int(layout.samples), int(layout.bits)
	size := rows * cols * layout.segments()

	strict, strictRecovered, strictErr := decodeFrame(context.Background(), fragment, rows, cols, samples, bits, false)
	tolerant, recovered, tolerantErr := decodeFrame(context.Background(), fragment, rows, cols, samples, bits, true)
	for _, err := range []error{strictErr, tolerantErr} {
		if err != nil && !isRLEError(err) {
			t.Fatalf("untyped error: %v", err)
		}
	}
	switch {
	case strictErr == nil:
		if strictRecovered || len(strict) != size {
			t.Fatalf("strict: %d bytes (want %d), recovered %v", len(strict), size, strictRecovered)
		}
		if tolerantErr != nil || recovered || !bytes.Equal(strict, tolerant) {
			t.Fatalf("recovery changed a clean frame: %v, recovered %v", tolerantErr, recovered)
		}
	case errors.Is(strictErr, ErrSegmentDecodeFailed):
		if tolerantErr != nil || !recovered || len(tolerant) != size {
			t.Fatalf("recovery of %v: %v, recovered %v, %d bytes", strictErr, tolerantErr, recovered, len(tolerant))
		}
	default:
		if tolerantErr == nil {
			t.Fatalf("recovery accepted %v", strictErr)
		}
	}

	if rows == 0 || cols == 0 {
		return
	}
	region := fuzzRegion(rows, cols, window)
	metadata := layout.metadata(rows, cols)
	for _, tc := range []struct {
		ctx       context.Context
		full      []byte
		fullErr   error
		recovered bool
	}{
		{context.Background(), strict, strictErr, false},
		{decoderecovery.With(context.Background(), decoderecovery.Options{RLESegmentOverrun: true}), tolerant, tolerantErr, recovered},
	} {
		got, err := New().DecodeFrameRegion(tc.ctx, fragment, metadata, region)
		if err != nil {
			if !isRLEError(err) {
				t.Fatalf("region: untyped error: %v", err)
			}
			if tc.fullErr == nil {
				t.Fatalf("region %v fails (%v) where the frame decodes", region, err)
			}
			continue
		}
		reported := slices.Contains(got.Diagnostics, pixeldata.DiagnosticRLESegmentOverrun)
		if reported && !tc.recovered {
			t.Fatalf("region %v reports a recovery the frame does not need", region)
		}
		if tc.fullErr != nil {
			continue // the region ends before the damage
		}
		step := layout.segments()
		var want []byte
		for y := region.Min.Y; y < region.Max.Y; y++ {
			want = append(want, tc.full[(y*cols+region.Min.X)*step:(y*cols+region.Max.X)*step]...)
		}
		if got.Rows != region.Dy() || got.Columns != region.Dx() || len(got.Data) != 1 || !bytes.Equal(got.Data[0], want) {
			t.Fatalf("region %v differs from the cropped frame", region)
		}
	}
}

// fuzzRegion maps window's four bytes to a non-empty region of the frame.
func fuzzRegion(rows, cols int, window uint32) image.Rectangle {
	x0, y0 := int(window&0xFF)%cols, int(window>>8&0xFF)%rows
	x1, y1 := x0+1+int(window>>16&0xFF)%(cols-x0), y0+1+int(window>>24)%(rows-y0)
	return image.Rect(x0, y0, x1, y1)
}

// isRLEError reports whether err is one of the typed errors decoding can
// return.
func isRLEError(err error) bool {
	for _, target := range []error{
		ErrInvalidHeader, ErrInvalidSegmentCount, ErrInvalidSegmentOffset, ErrSegmentDecodeFailed,
		ErrUnsupportedBitsAllocated, ErrUnsupportedSamplesPerPixel,
		pixeldata.ErrInvalidMetadata, pixeldata.ErrMissingMetadata,
		pixeldata.ErrPixelDataSizeMismatch, pixeldata.ErrIncompatiblePixelData, pixeldata.ErrInvalidRegion,
		pixeldata.ErrUnsupportedPhotometricInterpretation, pixeldata.ErrUnsupportedPixelRepresentation,
		pixeldata.ErrUnsupportedPlanarConfiguration,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// joinFragments packs fragments for FuzzDecodeRLEObject: each is a 16-bit
// little-endian length followed by its bytes.
func joinFragments(fragments ...[]byte) []byte {
	var out []byte
	for _, fragment := range fragments {
		out = binary.LittleEndian.AppendUint16(out, uint16(len(fragment)))
		out = append(out, fragment...)
	}
	return out
}

// splitFragments undoes joinFragments; a length past the end takes the
// rest, and at most 16 fragments are returned.
func splitFragments(blob []byte) [][]byte {
	var out [][]byte
	for len(blob) > 0 && len(out) < 16 {
		if len(blob) < 2 {
			out = append(out, blob)
			break
		}
		n := min(int(binary.LittleEndian.Uint16(blob)), len(blob)-2)
		out = append(out, blob[2:2+n])
		blob = blob[2+n:]
	}
	return out
}

// FuzzDecodeRLEObject decodes an RLE Pixel Data element through the codec:
// fragments, Basic Offset Table and Number of Frames are fuzzed, so frame
// grouping, header detection and metadata validation are exercised with
// the segment decoder.
func FuzzDecodeRLEObject(f *testing.F) {
	frameA := rleFragment([]byte{1, 2, 3, 4})
	frameB := rleFragment([]byte{5, 6, 7, 8})
	table := binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32(nil, 0), uint32(8+8+len(frameA)))
	f.Add(joinFragments(frameA), []byte(nil), uint8(0), uint8(2), uint8(2), uint8(1))
	f.Add(joinFragments(frameA, frameB), []byte(nil), uint8(0), uint8(2), uint8(2), uint8(2))
	f.Add(joinFragments(frameA[:66], frameA[66:], frameB[:64], frameB[64:]), table, uint8(0), uint8(2), uint8(2), uint8(2))
	f.Add(joinFragments(frameA[:40], frameA[40:], nil), []byte(nil), uint8(0), uint8(2), uint8(2), uint8(1))
	f.Add(joinFragments(frameA, frameB), []byte(nil), uint8(0), uint8(2), uint8(2), uint8(3))
	f.Add(joinFragments(frameA), []byte(nil), uint8(9), uint8(2), uint8(2), uint8(1))
	f.Add(joinFragments(frameA), []byte(nil), uint8(11), uint8(2), uint8(2), uint8(1))
	for _, seed := range rleSeeds() {
		f.Add(joinFragments(seed.fragment), []byte(nil), seed.layout, seed.rows, seed.cols, uint8(1))
	}
	f.Fuzz(func(t *testing.T, blob, offsetTable []byte, layoutIndex, rowsByte, colsByte, framesByte uint8) {
		layout := fuzzLayouts[int(layoutIndex)%len(fuzzLayouts)]
		rows, cols := fuzzDimensions(rowsByte, colsByte)
		frames := int(framesByte % 9)
		fragments := splitFragments(blob)
		if len(offsetTable) > 64 {
			offsetTable = offsetTable[:64]
		}
		obj := object.FromElements(append(
			rleMetadataElementsWithOptions(uint16(rows), uint16(cols), layout.samples, layout.bits, layout.photometric, frames,
				rleMetadataOptions{planarConfiguration: uint16Ptr(0)}),
			dicomtest.NewFragmentSequenceElement(core.TagPixelData, offsetTable, fragments...),
		), nil)
		pixel, err := pixeldata.Extract(obj)
		if err != nil {
			return
		}
		strict, strictErr := New().DecodeContext(context.Background(), pixel, obj)
		ctx := decoderecovery.With(context.Background(), decoderecovery.Options{RLESegmentOverrun: true})
		tolerant, tolerantErr := New().DecodeContext(ctx, pixel, obj)
		for _, err := range []error{strictErr, tolerantErr} {
			if err != nil && !isRLEError(err) {
				t.Fatalf("untyped error: %v", err)
			}
		}
		size := rows * cols * layout.segments()
		for _, decoded := range []struct {
			frames pixeldata.Frames
			err    error
		}{{strict, strictErr}, {tolerant, tolerantErr}} {
			if decoded.err != nil {
				continue
			}
			if want := max(frames, 1); len(decoded.frames.Data) != want {
				t.Fatalf("%d frames, want %d", len(decoded.frames.Data), want)
			}
			for i, frame := range decoded.frames.Data {
				if len(frame) != size {
					t.Fatalf("frame %d: %d bytes, want %d", i, len(frame), size)
				}
			}
		}
		if strictErr == nil {
			if len(strict.Diagnostics) != 0 {
				t.Fatalf("strict decode reported %v", strict.Diagnostics)
			}
			if tolerantErr != nil || len(tolerant.Diagnostics) != 0 || !equalFrames(strict.Data, tolerant.Data) {
				t.Fatalf("recovery changed clean frames: %v %v", tolerantErr, tolerant.Diagnostics)
			}
		} else if errors.Is(strictErr, ErrSegmentDecodeFailed) {
			// A later frame may still fail on its header, never on a run.
			if errors.Is(tolerantErr, ErrSegmentDecodeFailed) ||
				(tolerantErr == nil && !slices.Contains(tolerant.Diagnostics, pixeldata.DiagnosticRLESegmentOverrun)) {
				t.Fatalf("recovery of %v: %v %v", strictErr, tolerantErr, tolerant.Diagnostics)
			}
		}
		if tolerantErr == nil && len(tolerant.Diagnostics) == 0 && strictErr != nil {
			t.Fatalf("unreported recovery of %v", strictErr)
		}
	})
}

// FuzzRLERoundTrip encodes arbitrary frames with Encoder and decodes them
// back, strictly and as regions: the round trip is bit-exact, the encoded
// frame is a well-formed Annex G fragment and never needs a recovery.
func FuzzRLERoundTrip(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4}, uint8(0), uint8(2), uint8(2), uint32(0))
	f.Add(bytes.Repeat([]byte{7}, 300), uint8(1), uint8(3), uint8(64), uint32(0x01020304))
	f.Add([]byte{0, 0xFF, 0xFF, 0, 0x80}, uint8(3), uint8(5), uint8(7), uint32(0xFFFFFFFF))
	f.Add([]byte{9}, uint8(5), uint8(1), uint8(1), uint32(0))
	f.Add([]byte{1, 1, 1, 2, 2, 3}, uint8(6), uint8(4), uint8(33), uint32(0x00010203))
	f.Fuzz(func(t *testing.T, pattern []byte, layoutIndex, rowsByte, colsByte uint8, window uint32) {
		layout := fuzzLayouts[int(layoutIndex)%supportedFuzzLayouts]
		rows, cols := fuzzDimensions(rowsByte, colsByte)
		if rows == 0 || cols == 0 || len(pattern) == 0 {
			return
		}
		frame := make([]byte, rows*cols*layout.segments())
		for i := range frame {
			frame[i] = pattern[i%len(pattern)]
		}
		encoded, err := NewEncoder().EncodeFrame(context.Background(), frame, layout.metadata(rows, cols))
		if err != nil {
			t.Fatal(err)
		}
		data := encoded.Data
		if !looksLikeHeader(data, layout.segments()) {
			t.Fatalf("encoded header % x is not a well-formed %d-segment header", data[:64], layout.segments())
		}
		for i := 0; i < layout.segments(); i++ {
			if offset := binary.LittleEndian.Uint32(data[4+4*i:]); offset%2 != 0 {
				t.Fatalf("segment %d starts at odd offset %d", i, offset)
			}
		}
		decoded, recovered, err := decodeFrame(context.Background(), data, rows, cols, int(layout.samples), int(layout.bits), false)
		if err != nil || recovered || !bytes.Equal(decoded, frame) {
			t.Fatalf("round trip: %v, recovered %v", err, recovered)
		}
		checkRLEFrame(t, data, layoutIndex%supportedFuzzLayouts, rowsByte, colsByte, window)
	})
}
