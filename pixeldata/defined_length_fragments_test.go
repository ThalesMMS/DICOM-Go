package pixeldata_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/rle"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func item(element uint16, value []byte) []byte {
	out := binary.LittleEndian.AppendUint16(nil, 0xFFFE)
	out = binary.LittleEndian.AppendUint16(out, element)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(value)))
	return append(out, value...)
}

// definedLengthRLE is an RLE image whose Pixel Data holds its Items in a
// defined-length OB value, as JPEGDefinedLengthSequenceOfFragments.dcm of
// gdcmData does, and its native pixels.
func definedLengthRLE(t *testing.T) (*object.Object, []byte) {
	t.Helper()
	pixels := make([]byte, pixelsPerFrame)
	for i := range pixels {
		pixels[i] = byte(i * 21)
	}
	obj := nativeImage("MONOCHROME2", 1, 8, 8, 0, pixels)
	obj.Put(text(0x0028, 0x0008, core.VRIS, "1"))
	metadata, err := pixeldata.ExtractMetadata(obj)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := rle.NewEncoder().EncodeFrame(context.Background(), pixels, metadata)
	if err != nil {
		t.Fatal(err)
	}
	value := slices.Concat(item(0xE000, nil), item(0xE000, encoded.Data), item(0xE0DD, nil))
	obj.Put(core.NewRawElement(core.TagPixelData, core.VROB, value))
	return obj, pixels
}

func rleDecoders(t *testing.T) pixeldata.Registry {
	t.Helper()
	registry := pixeldata.NewMemoryRegistry()
	if err := rle.Register(registry); err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestDefinedLengthFragmentsIsOptIn(t *testing.T) {
	obj, pixels := definedLengthRLE(t)
	options := pixeldata.DecompressOptions{Registry: rleDecoders(t)}
	if _, _, err := pixeldata.DecompressDataSet(obj, transfer.RLELossless, options); !errors.Is(err, pixeldata.ErrIncompatiblePixelData) {
		t.Fatalf("strict: err = %v", err)
	}
	var report pixeldata.DecompressReport
	options.Recovery.DefinedLengthFragments = true
	options.Report = &report
	decoded, _, err := pixeldata.DecompressDataSet(obj, transfer.RLELossless, options)
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := decoded.GetRaw(core.TagPixelData); !bytes.Equal(raw, pixels) {
		t.Fatalf("decoded %v", raw)
	}
	if !slices.Equal(report.Diagnostics, []string{pixeldata.DiagnosticDefinedLengthFragments}) {
		t.Fatalf("diagnostics %v", report.Diagnostics)
	}
	// A value that does not split exactly into Items stays an error.
	broken, _ := definedLengthRLE(t)
	value, _ := broken.GetRaw(core.TagPixelData)
	broken.Put(core.NewRawElement(core.TagPixelData, core.VROB, append(bytes.Clone(value), 0, 0)))
	if _, _, err := pixeldata.DecompressDataSet(broken, transfer.RLELossless, options); !errors.Is(err, pixeldata.ErrIncompatiblePixelData) {
		t.Fatalf("left-over bytes: err = %v", err)
	}
}

func TestTranscodeRewritesDefinedLengthFragments(t *testing.T) {
	obj, pixels := definedLengthRLE(t)
	options := pixeldata.TranscodeOptions{DecoderRegistry: rleDecoders(t), Recovery: pixeldata.DecodeRecovery{DefinedLengthFragments: true}}
	output, report, err := pixeldata.TranscodeDataSet(context.Background(), obj, transfer.RLELossless, transfer.ExplicitVRLittleEndian, options)
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := output.GetRaw(core.TagPixelData); !bytes.Equal(raw, pixels) {
		t.Fatalf("transcoded %v", raw)
	}
	if len(report.DecodeDiagnostics) == 0 || report.DecodeDiagnostics[0] != pixeldata.DiagnosticDefinedLengthFragments {
		t.Fatalf("diagnostics %v", report.DecodeDiagnostics)
	}
}
