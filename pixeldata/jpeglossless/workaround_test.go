package jpeglossless

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/internal/decoderecovery"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// overflowedPredictor6 models the defective encoders: Ra and Rc held in
// signed 16-bit integers.
func overflowedPredictor6(a, b, c int32) int32 {
	return b + ((int32(int16(a)) - int32(int16(c))) >> 1)
}

func withOverflowedPredictor6(e *Encoder) { e.faultyPredictor6 = overflowedPredictor6 }

func withCornellBug(e *Encoder) { e.cornell = true }

// workaroundVector encodes a synthetic frame with a defective encoder and
// returns the metadata, the native frame and the compressed frame.
func workaroundVector(t testing.TB, predictor int, stored uint16, faulty func(*Encoder)) (pixeldata.Metadata, []byte, []byte) {
	t.Helper()
	metadata := encoderTestMetadata(37, 53, 1, 16, stored, 0, "MONOCHROME2")
	frame := encoderTestFrame(metadata, 99)
	encoder, err := NewProcess14Encoder(predictor)
	if err != nil {
		t.Fatal(err)
	}
	faulty(encoder)
	encoded, err := encoder.EncodeFrame(context.Background(), frame, metadata)
	if err != nil {
		t.Fatal(err)
	}
	return metadata, canonical(frame, metadata), encoded.Data
}

func decodeWithRecovery(t testing.TB, encoded []byte, metadata pixeldata.Metadata, options decoderecovery.Options) (pixeldata.Frames, error) {
	t.Helper()
	if len(encoded)%2 == 1 {
		encoded = append(encoded, 0)
	}
	return New().DecodeContext(decoderecovery.With(context.Background(), options), encapsulated(encoded), nativeObject(metadata, nil))
}

func TestPredictor6OverflowWorkaround(t *testing.T) {
	metadata, want, encoded := workaroundVector(t, 6, 16, withOverflowedPredictor6)
	strict, err := decodeWithRecovery(t, encoded, metadata, decoderecovery.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(strict.Data[0], want) || len(strict.Diagnostics) != 0 {
		t.Fatalf("strict decode reproduced the defective encoder (diagnostics %v)", strict.Diagnostics)
	}
	fixed, err := decodeWithRecovery(t, encoded, metadata, decoderecovery.Options{JPEGLosslessPredictor6Overflow: true})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fixed.Data[0], want) {
		t.Fatal("predictor 6 workaround did not restore the samples")
	}
	if !slices.Equal(fixed.Diagnostics, []string{pixeldata.DiagnosticJPEGLosslessPredictor6Overflow}) {
		t.Fatalf("diagnostics = %v", fixed.Diagnostics)
	}
}

// Below 16 bits the samples never wrap, and other predictors are
// unaffected, so the workaround changes and reports nothing.
func TestPredictor6OverflowWorkaroundLeavesOtherStreams(t *testing.T) {
	for _, tc := range []struct{ predictor, stored int }{{6, 15}, {6, 12}, {5, 16}, {1, 16}} {
		metadata, want, encoded := workaroundVector(t, tc.predictor, uint16(tc.stored), func(*Encoder) {})
		frames, err := decodeWithRecovery(t, encoded, metadata, decoderecovery.Options{JPEGLosslessPredictor6Overflow: true})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(frames.Data[0], want) || len(frames.Diagnostics) != 0 {
			t.Fatalf("predictor %d, %d bits: changed, diagnostics %v", tc.predictor, tc.stored, frames.Diagnostics)
		}
	}
}

func TestCornellCategory16Workaround(t *testing.T) {
	for predictor := 1; predictor <= 7; predictor++ {
		metadata, want, encoded := workaroundVector(t, predictor, 16, withCornellBug)
		if _, err := decodeWithRecovery(t, encoded, metadata, decoderecovery.Options{}); err == nil {
			t.Fatalf("predictor %d: strict decode accepted Cornell extra bits", predictor)
		}
		frames, err := decodeWithRecovery(t, encoded, metadata, decoderecovery.Options{JPEGLosslessEncoderQuirks: true})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(frames.Data[0], want) {
			t.Fatalf("predictor %d: Cornell workaround did not restore the samples", predictor)
		}
		if !slices.Contains(frames.Diagnostics, pixeldata.DiagnosticJPEGLosslessCategory16ExtraBits) {
			t.Fatalf("predictor %d: diagnostics = %v", predictor, frames.Diagnostics)
		}
	}
}

// TestWorkaroundsMatchDCMTK decodes the synthetic defective streams with
// dcmdjpeg +w6 and +wc and requires the samples the workarounds restore.
// dcmdjpeg +wc is left out for predictors 5 and 6, where it does not read
// the extra bits of these vectors. Enable with DICOMGO_DCMTK_INTEGRATION=1.
func TestWorkaroundsMatchDCMTK(t *testing.T) {
	dcmdjpeg := dcmtkoracle.Tool(t, "dcmdjpeg")
	type vector struct {
		predictor int
		flag      string
		faulty    func(*Encoder)
		options   decoderecovery.Options
	}
	vectors := []vector{{6, "+w6", withOverflowedPredictor6, decoderecovery.Options{JPEGLosslessPredictor6Overflow: true}}}
	for _, predictor := range []int{1, 2, 3, 4, 7} {
		vectors = append(vectors, vector{predictor, "+wc", withCornellBug, decoderecovery.Options{JPEGLosslessEncoderQuirks: true}})
	}
	dir := t.TempDir()
	for _, v := range vectors {
		name := fmt.Sprintf("sv%d%s", v.predictor, v.flag)
		metadata, want, encoded := workaroundVector(t, v.predictor, 16, v.faulty)
		ours, err := decodeWithRecovery(t, encoded, metadata, v.options)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(ours.Data[0], want) {
			t.Fatalf("%s: workaround did not restore the samples", name)
		}
		if len(encoded)%2 == 1 {
			encoded = append(encoded, 0)
		}
		obj := nativeObject(metadata, nil)
		obj.Put(dicomtest.NewFragmentSequenceElement(core.TagPixelData, nil, encoded))
		var buf bytes.Buffer
		if err := object.WriteFile(&buf, &object.File{Dataset: obj, TransferSyntax: transfer.JPEGLosslessNonHierarchical}); err != nil {
			t.Fatal(err)
		}
		input := filepath.Join(dir, name+".dcm")
		if err := os.WriteFile(input, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		output := filepath.Join(dir, name+"-native.dcm")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, err := dcmtkoracle.Command(ctx, t, dcmtkoracle.Exact, dcmdjpeg, v.flag, input, output).CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("%s: dcmdjpeg: %v: %s", name, err, out)
		}
		decoded, err := object.OpenFile(output)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := decoded.Dataset.GetRaw(core.TagPixelData)
		if len(raw) < len(want) || !bytes.Equal(raw[:len(want)], want) {
			t.Errorf("%s: dcmdjpeg %s decodes different samples", name, v.flag)
		}
	}
}

// DecodeRecovery.JPEGLosslessPredictor6Overflow reaches the codec through
// DecompressDataSet and is reported.
func TestDecompressPredictor6OverflowOption(t *testing.T) {
	metadata, want, encoded := workaroundVector(t, 6, 16, withOverflowedPredictor6)
	if len(encoded)%2 == 1 {
		encoded = append(encoded, 0)
	}
	obj := nativeObject(metadata, nil)
	obj.Put(dicomtest.NewFragmentSequenceElement(core.TagPixelData, nil, encoded))
	registry := pixeldata.NewMemoryRegistry()
	if err := Register(registry); err != nil {
		t.Fatal(err)
	}
	report := &pixeldata.DecompressReport{}
	opts := pixeldata.DecompressOptions{Registry: registry, Recovery: pixeldata.DecodeRecovery{JPEGLosslessPredictor6Overflow: true}, Report: report}
	out, _, err := pixeldata.DecompressDataSet(obj, transfer.JPEGLosslessNonHierarchical, opts)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := out.GetRaw(core.TagPixelData)
	if !bytes.Equal(raw, want) {
		t.Fatal("decompressed samples differ from the source")
	}
	if !slices.Contains(report.Diagnostics, pixeldata.DiagnosticJPEGLosslessPredictor6Overflow) {
		t.Fatalf("report %v", report.Diagnostics)
	}
}
