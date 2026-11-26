package pixeldata

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/storedsample"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestMaskCounts(t *testing.T) {
	for v, want := range map[uint16]uint16{0xffff: 16, 0x0fff: 12, 0x00ff: 8, 0x001f: 5, 0x7fff: 15} {
		if n, ok := maskCount(v); !ok || n != want {
			t.Errorf("maskCount(%#x) = %d %v, want %d", v, n, ok, want)
		}
	}
	// Real counts and values that are not masks stay as they are.
	for _, v := range []uint16{0, 1, 3, 7, 8, 12, 15, 16, 17, 0x00fe, 0x1000} {
		if _, ok := maskCount(v); ok {
			t.Errorf("maskCount(%#x) accepted", v)
		}
	}
	for v, want := range map[uint16]uint16{0xfffe: 15, 0x0ffe: 11, 0x00fe: 7} {
		if n, ok := highBitMask(v); !ok || n != want {
			t.Errorf("highBitMask(%#x) = %d %v, want %d", v, n, ok, want)
		}
	}
	for _, v := range []uint16{7, 11, 15, 0xffff, 0x0ffc} {
		if _, ok := highBitMask(v); ok {
			t.Errorf("highBitMask(%#x) accepted", v)
		}
	}
}

func maskedImage(allocated, stored, high uint16, pixels []byte) *object.Object {
	return object.FromElements([]core.Element{
		photometricInterpretationElement("MONOCHROME1"),
		uint16RawElement(tagRows, 2), uint16RawElement(tagColumns, 3), uint16RawElement(tagSamplesPerPixel, 1),
		uint16RawElement(tagBitsAllocated, allocated), uint16RawElement(tagBitsStored, stored), uint16RawElement(tagHighBit, high),
		uint16RawElement(tagPixelRepresentation, 0),
		core.NewRawElement(core.TagPixelData, core.VROW, pixels),
	}, nil)
}

func TestRepairBitCountMasksIsOptIn(t *testing.T) {
	pixels := make([]byte, 12)
	for i := 0; i < 6; i++ {
		binary.LittleEndian.PutUint16(pixels[2*i:], uint16(i*700))
	}
	obj := maskedImage(0xffff, 0x0fff, 0x0ffe, pixels)
	if _, _, err := DecompressDataSet(obj, transfer.ExplicitVRLittleEndian, DecompressOptions{}); err == nil {
		t.Fatal("strict decode accepted Bits Allocated 0xFFFF")
	}
	var report DecompressReport
	options := DecompressOptions{Recovery: DecodeRecovery{RepairBitCountMasks: true}, Report: &report}
	decoded, _, err := DecompressDataSet(obj, transfer.ExplicitVRLittleEndian, options)
	if err != nil {
		t.Fatal(err)
	}
	m, err := ExtractMetadata(decoded)
	if err != nil || m.BitsAllocated != 16 || m.BitsStored != 12 || m.HighBit != 11 {
		t.Fatalf("metadata %+v %v", m, err)
	}
	if raw, _ := decoded.GetRaw(core.TagPixelData); !bytes.Equal(raw, pixels) {
		t.Fatal("pixels changed")
	}
	if len(report.Diagnostics) != 1 || report.Diagnostics[0] != DiagnosticBitCountMasks || len(report.Reconciliations) != 3 {
		t.Fatalf("report %+v", report)
	}
	if r := report.Reconciliations[0]; r.Attribute != "BitsAllocated" || r.Declared != "65535" || r.Used != "16" {
		t.Fatalf("reconciliation %+v", r)
	}

	// Masks are replaced only when the counts agree with each other and
	// with the Pixel Data size.
	for name, bad := range map[string]*object.Object{
		"8-bit sized data":    maskedImage(0xffff, 0x0fff, 0x0ffe, make([]byte, 6)),
		"stored above":        maskedImage(0x00ff, 0x0fff, 0x0ffe, make([]byte, 6)),
		"high bit mismatched": maskedImage(0xffff, 0x0fff, 0x00fe, pixels),
	} {
		if out, applied, diagnostics := withBitCountMaskRepair(bad, transfer.ExplicitVRLittleEndian); out != bad || applied != nil || diagnostics != nil {
			t.Errorf("%s: repaired", name)
		}
	}
	// A conformant data set is left alone.
	good := maskedImage(16, 12, 11, pixels)
	if out, applied, diagnostics := withBitCountMaskRepair(good, transfer.ExplicitVRLittleEndian); out != good || applied != nil || diagnostics != nil {
		t.Fatal("counts rewritten")
	}
}

// decodedSamples decompresses obj and interprets its first frame as stored
// samples, returning the decompress report and the first error.
func decodedSamples(t *testing.T, obj *object.Object, source transfer.Syntax, recovery DecodeRecovery) ([]int32, DecompressReport, error) {
	t.Helper()
	var report DecompressReport
	out, _, err := DecompressDataSet(obj, source, DecompressOptions{Recovery: recovery, Report: &report})
	if err != nil {
		return nil, report, err
	}
	frames, err := ExtractNativeFrames(out)
	if err != nil {
		return nil, report, err
	}
	samples, err := StoredSamples[int32](context.Background(), frames.Data[0], frames.Metadata.SampleFormat(binary.LittleEndian), 0)
	return samples, report, err
}

func TestRepairHighBitZeroMatchesTwin(t *testing.T) {
	pixels := make([]byte, 12)
	for i, v := range []uint16{0x0000, 0x0123, 0x0fff, 0xf800, 0x5a5a, 0x0800} {
		// Bits above Bits Stored are set in some samples: both files must
		// mask them alike.
		binary.LittleEndian.PutUint16(pixels[2*i:], v)
	}
	image := func(high uint16) *object.Object {
		obj := maskedImage(16, 12, high, pixels)
		obj.Put(photometricInterpretationElement("MONOCHROME2"))
		return obj
	}
	want, _, err := decodedSamples(t, image(11), transfer.ExplicitVRLittleEndian, DecodeRecovery{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(want, []int32{0x000, 0x123, 0xfff, 0x800, 0xa5a, 0x800}) {
		t.Fatalf("twin samples %#x", want)
	}

	// Strict decoding keeps refusing High Bit 0.
	if _, _, err := decodedSamples(t, image(0), transfer.ExplicitVRLittleEndian, DecodeRecovery{}); !errors.Is(err, storedsample.ErrHighBit) {
		t.Fatalf("strict High Bit 0: %v, want ErrHighBit", err)
	}

	got, report, err := decodedSamples(t, image(0), transfer.ExplicitVRLittleEndian, DecodeRecovery{RepairBitCountMasks: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("repaired samples %#x, twin %#x", got, want)
	}
	if !slices.Equal(report.Diagnostics, []string{DiagnosticHighBitZero}) || len(report.Reconciliations) != 1 {
		t.Fatalf("report %+v", report)
	}
	if r := report.Reconciliations[0]; r.Attribute != "HighBit" || r.Tag != tagHighBit || r.Declared != "0" || r.Used != "11" {
		t.Fatalf("reconciliation %+v", r)
	}
	// The detectable retry profile includes it.
	if got, _, err := decodedSamples(t, image(0), transfer.ExplicitVRLittleEndian, DetectableDecodeRecovery()); err != nil || !slices.Equal(got, want) {
		t.Fatalf("DetectableDecodeRecovery: %#x %v", got, err)
	}
}

func TestRepairHighBitZeroScope(t *testing.T) {
	pixels := make([]byte, 12)
	for name, obj := range map[string]*object.Object{
		// Bits Stored 1 with High Bit 0 is valid and stays.
		"one bit stored":       maskedImage(16, 1, 0, pixels),
		"stored above":         maskedImage(8, 12, 0, make([]byte, 6)),
		"8-bit sized data":     maskedImage(16, 12, 0, make([]byte, 6)),
		"conformant high bit":  maskedImage(16, 12, 15, pixels),
		"inconsistent masks":   maskedImage(0x00ff, 0x0fff, 0, pixels),
		"twelve bit container": maskedImage(12, 12, 0, pixels),
	} {
		if out, applied, diagnostics := withBitCountMaskRepair(obj, transfer.ExplicitVRLittleEndian); out != obj || applied != nil || diagnostics != nil {
			t.Errorf("%s: repaired %+v %v", name, applied, diagnostics)
		}
	}
	// With a Bits Stored mask both repairs apply and are reported.
	out, applied, diagnostics := withBitCountMaskRepair(maskedImage(16, 0x0fff, 0, pixels), transfer.ExplicitVRLittleEndian)
	if !slices.Equal(diagnostics, []string{DiagnosticBitCountMasks, DiagnosticHighBitZero}) || len(applied) != 2 {
		t.Fatalf("diagnostics %v reconciliations %+v", diagnostics, applied)
	}
	if high, _ := getUint16(out, tagHighBit); high != 11 {
		t.Fatalf("High Bit %d", high)
	}
	// A 32-bit container is repaired when no mask is involved.
	out, _, diagnostics = withBitCountMaskRepair(maskedImage(32, 24, 0, make([]byte, 24)), transfer.ExplicitVRLittleEndian)
	if high, _ := getUint16(out, tagHighBit); !slices.Equal(diagnostics, []string{DiagnosticHighBitZero}) || high != 23 {
		t.Fatalf("32-bit: High Bit %d diagnostics %v", high, diagnostics)
	}
}
