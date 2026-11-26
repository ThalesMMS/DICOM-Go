package jpeglossless

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/decoderecovery"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

func quirkContext() context.Context {
	return decoderecovery.With(context.Background(), decoderecovery.Options{JPEGLosslessEncoderQuirks: true})
}

func decodeQuirks(t *testing.T, fragment []byte, precision uint16, bitsAllocated uint16, rows, cols uint16) (pixeldata.Frames, error, pixeldata.Frames, error) {
	t.Helper()
	obj := object.FromElements(metadataElements(rows, cols, bitsAllocated, precision, 1), nil)
	strict, strictErr := New().DecodeContext(context.Background(), encapsulated(fragment), obj)
	lenient, lenientErr := New().DecodeContext(quirkContext(), encapsulated(fragment), obj)
	return strict, strictErr, lenient, lenientErr
}

// The test encoder writes 16 bits after a category-16 difference, as the
// Cornell and MARCONI encoders do; T.81 H.1.2.2 defines none.
func TestEncoderQuirkCategory16ExtraBits(t *testing.T) {
	samples := []int32{0, 1, 2, 3}
	fragment := encodeLossless(4, 1, 16, 1, samples) // first difference is -32768
	_, strictErr, frames, err := decodeQuirks(t, fragment, 16, 16, 1, 4)
	if strictErr == nil {
		t.Fatal("strict decode accepted extra bits after category 16")
	}
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(frames.Diagnostics, pixeldata.DiagnosticJPEGLosslessCategory16ExtraBits) {
		t.Fatalf("diagnostics %v", frames.Diagnostics)
	}
	if want := []byte{0, 0, 1, 0, 2, 0, 3, 0}; !bytes.Equal(frames.Data[0], want) {
		t.Fatalf("pixels % X", frames.Data[0])
	}
}

func TestEncoderQuirkNonzeroSe(t *testing.T) {
	samples := []int32{10, 20, 30, 40, 50, 60}
	fragment := encodeLossless(3, 2, 8, 1, samples)
	sos := bytes.Index(fragment, []byte{0xff, 0xda})
	fragment[sos+8] = 0x3f // Se
	strict, strictErr, frames, err := decodeQuirks(t, fragment, 8, 8, 2, 3)
	if !errors.Is(strictErr, ErrUnsupportedScan) || strict.Data != nil {
		t.Fatalf("strict error = %v", strictErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(frames.Diagnostics, []string{pixeldata.DiagnosticJPEGLosslessSeIgnored}) {
		t.Fatalf("diagnostics %v", frames.Diagnostics)
	}
	if want := []byte{10, 20, 30, 40, 50, 60}; !bytes.Equal(frames.Data[0], want) {
		t.Fatalf("pixels % X", frames.Data[0])
	}
}

func TestEncoderQuirkFillBits(t *testing.T) {
	// Three zero differences take 15 bits: one fill bit precedes EOI.
	fragment := encodeLossless(3, 1, 8, 1, []int32{128, 128, 128})
	fragment[len(fragment)-3] &^= 1
	_, strictErr, frames, err := decodeQuirks(t, fragment, 8, 8, 1, 3)
	if strictErr == nil {
		t.Fatal("strict decode accepted zero fill bits")
	}
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(frames.Diagnostics, []string{pixeldata.DiagnosticJPEGLosslessFillBits}) {
		t.Fatalf("diagnostics %v", frames.Diagnostics)
	}
}

// A conforming stream never reports a quirk with the recovery on.
func TestEncoderQuirksLeaveConformingStreams(t *testing.T) {
	fragment := encodeLossless(3, 2, 8, 1, []int32{1, 2, 3, 4, 5, 6})
	_, strictErr, frames, err := decodeQuirks(t, fragment, 8, 8, 2, 3)
	if strictErr != nil || err != nil || len(frames.Diagnostics) != 0 {
		t.Fatalf("strict %v lenient %v diagnostics %v", strictErr, err, frames.Diagnostics)
	}
}
