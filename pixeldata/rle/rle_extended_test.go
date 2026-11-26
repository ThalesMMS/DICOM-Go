package rle

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/decoderecovery"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// rleObject builds an RLE object with explicit Number of Frames, Basic
// Offset Table and fragments.
func rleObject(t *testing.T, rows, columns, samples, bits uint16, photometric string, frames int, planar *uint16, offsetTable []byte, fragments ...[]byte) (*object.Object, pixeldata.PixelData) {
	t.Helper()
	obj := object.FromElements(append(
		rleMetadataElementsWithOptions(rows, columns, samples, bits, photometric, frames, rleMetadataOptions{planarConfiguration: planar}),
		dicomtest.NewFragmentSequenceElement(core.TagPixelData, offsetTable, fragments...),
	), nil)
	pixel, err := pixeldata.Extract(obj)
	if err != nil {
		t.Fatal(err)
	}
	return obj, pixel
}

func TestDecodeYBRFullAndPlanarConfigurationOne(t *testing.T) {
	// Segments are color-by-plane; the output is interleaved even when the
	// object says Planar Configuration 1 (as GDCM decodes it).
	fragment := rleFragment([]byte{10, 11}, []byte{20, 21}, []byte{30, 31})
	obj, pixel := rleObject(t, 1, 2, 3, 8, "YBR_FULL", 1, uint16Ptr(1), nil, fragment)
	frames, err := New().Decode(pixel, obj)
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte{10, 20, 30, 11, 21, 31}; !bytes.Equal(frames.Data[0], want) {
		t.Fatalf("frame %v, want %v", frames.Data[0], want)
	}
	obj, pixel = rleObject(t, 1, 2, 3, 8, "YBR_FULL_422", 1, uint16Ptr(0), nil, fragment)
	if _, err := New().Decode(pixel, obj); !errors.Is(err, pixeldata.ErrUnsupportedPhotometricInterpretation) {
		t.Fatalf("YBR_FULL_422: %v", err)
	}
}

func TestEncoderDecoderRoundTripWideSamplesAndYBR(t *testing.T) {
	cases := []struct {
		name        string
		samples     uint16
		bits        uint16
		photometric string
	}{
		{"mono-24", 1, 24, "MONOCHROME2"},
		{"mono-32", 1, 32, "MONOCHROME2"},
		{"mono-120", 1, 120, "MONOCHROME2"},
		{"ybr-full-8", 3, 8, "YBR_FULL"},
		{"rgb-32", 3, 32, "RGB"},
		{"ybr-full-40", 3, 40, "YBR_FULL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const rows, columns = 3, 5
			metadata := encoderMetadata(rows, columns, tc.samples, tc.bits, tc.photometric)
			frame := make([]byte, rows*columns*int(tc.samples)*int(tc.bits)/8)
			for i := range frame {
				frame[i] = byte(i*37 + i/7)
				if i%11 < 4 {
					frame[i] = 0xAA // runs
				}
			}
			encoded, err := NewEncoder().EncodeFrame(context.Background(), frame, metadata)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := decodeFragment(encoded.Data, rows, columns, int(tc.samples), int(tc.bits))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(decoded, frame) {
				t.Fatalf("round trip differs")
			}
		})
	}
	if supportedBitsAllocated(128, 1) || supportedBitsAllocated(48, 3) || supportedBitsAllocated(12, 1) {
		t.Fatal("more than 15 segments or partial bytes accepted")
	}
}

func TestDecodeFramesSpanningFragments(t *testing.T) {
	first := rleFragment([]byte{1, 2, 3, 4})
	second := rleFragment([]byte{5, 6, 7, 8})
	split := func(fragment []byte, at int) [][]byte { return [][]byte{fragment[:at], fragment[at:]} }

	// One frame in three fragments.
	parts := append(split(first, 40), []byte{})
	obj, pixel := rleObject(t, 2, 2, 1, 8, "MONOCHROME2", 1, nil, nil, parts...)
	frames, err := New().Decode(pixel, obj)
	if err != nil || !bytes.Equal(frames.Data[0], []byte{1, 2, 3, 4}) {
		t.Fatalf("single frame: %v %v", frames.Data, err)
	}

	// Two frames, each split in two, located by the Basic Offset Table.
	fragments := append(split(first, 66), split(second, 64)...)
	table := binary.LittleEndian.AppendUint32(nil, 0)
	table = binary.LittleEndian.AppendUint32(table, uint32(8+8+len(first))) // two Item headers
	obj, pixel = rleObject(t, 2, 2, 1, 8, "MONOCHROME2", 2, nil, table, fragments...)
	frames, err = New().Decode(pixel, obj)
	if err != nil || !bytes.Equal(frames.Data[1], []byte{5, 6, 7, 8}) {
		t.Fatalf("offset table: %v %v", frames.Data, err)
	}

	// Without the table, frames start at the fragments holding RLE headers.
	obj, pixel = rleObject(t, 2, 2, 1, 8, "MONOCHROME2", 2, nil, nil, fragments...)
	frames, err = New().Decode(pixel, obj)
	if err != nil || !bytes.Equal(frames.Data[0], []byte{1, 2, 3, 4}) || !bytes.Equal(frames.Data[1], []byte{5, 6, 7, 8}) {
		t.Fatalf("header split: %v %v", frames.Data, err)
	}

	// Three frames cannot come from two headers.
	obj, pixel = rleObject(t, 2, 2, 1, 8, "MONOCHROME2", 3, nil, nil, fragments...)
	if _, err := New().Decode(pixel, obj); !errors.Is(err, pixeldata.ErrPixelDataSizeMismatch) {
		t.Fatalf("ambiguous split: %v", err)
	}
	obj, pixel = rleObject(t, 2, 2, 1, 8, "MONOCHROME2", 5, nil, nil, fragments...)
	if _, err := New().Decode(pixel, obj); !errors.Is(err, pixeldata.ErrPixelDataSizeMismatch) {
		t.Fatalf("more frames than fragments: %v", err)
	}
}

func TestSegmentOverrunRecoveryIsOptInAndReported(t *testing.T) {
	// A literal run of 4 bytes cut after 2: strict decoding refuses it; the
	// recovery keeps the two bytes and zero-fills the rest.
	segment := []byte{3, 9, 8}
	fragment := rleFragmentEncoded(segment)
	obj, pixel := rleObject(t, 2, 2, 1, 8, "MONOCHROME2", 1, nil, nil, fragment)
	if _, err := New().Decode(pixel, obj); !errors.Is(err, ErrSegmentDecodeFailed) {
		t.Fatalf("strict: %v", err)
	}
	ctx := decoderecovery.With(context.Background(), decoderecovery.Options{RLESegmentOverrun: true})
	frames, err := New().DecodeContext(ctx, pixel, obj)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(frames.Data[0], []byte{9, 8, 0, 0}) || !slices.Contains(frames.Diagnostics, pixeldata.DiagnosticRLESegmentOverrun) {
		t.Fatalf("tolerant: %v %v", frames.Data, frames.Diagnostics)
	}
	// A repeat run past the frame is clipped.
	out, recovered, err := decodePackBitsContext(context.Background(), []byte{0xFB, 7}, 4, true)
	if err != nil || !recovered || !bytes.Equal(out, []byte{7, 7, 7, 7}) {
		t.Fatalf("clipped repeat: %v %v %v", out, recovered, err)
	}
	// A clean segment is not reported.
	if _, recovered, _ := decodePackBitsContext(context.Background(), []byte{0xFD, 7}, 4, true); recovered {
		t.Fatal("clean segment reported as recovered")
	}
}

// TestGDCMDataRLEReferences decodes the gdcmData RLE files the strict decoder
// refused and compares GDCM's MD5 of the decoded pixels. Set
// DICOMGO_GDCMDATA_ROOT to a GDCM Testing/Data checkout.
func TestGDCMDataRLEReferences(t *testing.T) {
	root := os.Getenv("DICOMGO_GDCMDATA_ROOT")
	if root == "" {
		t.Skip("set DICOMGO_GDCMDATA_ROOT to a GDCM Testing/Data checkout")
	}
	cases := []struct {
		name, md5 string
		overrun   bool
	}{
		{"ACUSON-24-YBR_FULL-RLE.dcm", "435c66f7e113d11d226d500294aae865", false},
		{"ACUSON-24-YBR_FULL-RLE-b.dcm", "22b32f23beb118f7b64c13bf04bc2809", false},
		{"LEADTOOLS_FLOWERS-8-PAL-RLE.dcm", "16e999d6afc5574bcb075f296c3bcbbc", false},
		{"ALOKA_SSD-8-MONO2-RLE-SQ.dcm", "7d8858e3419392b7f39a99fdc8028064", true},
	}
	for _, tc := range cases {
		file, err := object.OpenFile(root + "/" + tc.name)
		if err != nil {
			t.Fatal(err)
		}
		pixel, err := pixeldata.Extract(file.Dataset)
		if err != nil {
			t.Fatal(err)
		}
		ctx := decoderecovery.With(context.Background(), decoderecovery.Options{RLESegmentOverrun: tc.overrun})
		frames, err := New().DecodeContext(ctx, pixel, file.Dataset)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := md5Frames(frames.Data); got != tc.md5 {
			t.Errorf("%s: MD5 %s, GDCM %s", tc.name, got, tc.md5)
		}
		if tc.overrun != slices.Contains(frames.Diagnostics, pixeldata.DiagnosticRLESegmentOverrun) {
			t.Errorf("%s: diagnostics %v", tc.name, frames.Diagnostics)
		}
	}
}

func md5Frames(frames [][]byte) string {
	h := md5.New()
	for _, frame := range frames {
		h.Write(frame)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestDecompressRLEColorAndOverrunReport(t *testing.T) {
	registry := pixeldata.NewMemoryRegistry()
	if err := Register(registry); err != nil {
		t.Fatal(err)
	}
	syntax := transfer.RLELossless
	obj, _ := rleObject(t, 1, 2, 3, 8, "YBR_FULL", 1, uint16Ptr(1), nil, rleFragment([]byte{10, 11}, []byte{20, 21}, []byte{30, 31}))
	out, _, err := pixeldata.DecompressDataSet(obj, syntax, pixeldata.DecompressOptions{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	planar, _ := out.GetRaw(tagPlanarConfiguration)
	raw, _ := out.GetRaw(core.TagPixelData)
	if len(planar) != 2 || planar[0] != 0 || !bytes.Equal(raw, []byte{10, 20, 30, 11, 21, 31}) {
		t.Fatalf("decompressed planar %v pixels %v", planar, raw)
	}
	truncated, _ := rleObject(t, 2, 2, 1, 8, "MONOCHROME2", 1, nil, nil, rleFragmentEncoded([]byte{3, 9, 8}))
	if _, _, err := pixeldata.DecompressDataSet(truncated, syntax, pixeldata.DecompressOptions{Registry: registry}); err == nil {
		t.Fatal("strict decompression accepted a truncated run")
	}
	report := &pixeldata.DecompressReport{}
	if _, _, err := pixeldata.DecompressDataSet(truncated, syntax, pixeldata.DecompressOptions{Registry: registry, Recovery: pixeldata.DecodeRecovery{RLESegmentOverrun: true}, Report: report}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(report.Diagnostics, pixeldata.DiagnosticRLESegmentOverrun) {
		t.Fatalf("report %v", report.Diagnostics)
	}
}
