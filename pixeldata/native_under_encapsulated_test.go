package pixeldata_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpeg"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func jpegDecoders(t *testing.T) pixeldata.Registry {
	t.Helper()
	registry := pixeldata.NewMemoryRegistry()
	if err := jpeg.Register(registry); err != nil {
		t.Fatal(err)
	}
	return registry
}

// nativeUnderJPEG is a 3x4 image whose Pixel Data is native although the
// file declares JPEG Baseline, as some writers produce.
func nativeUnderJPEG(bits int, pixels []byte) *object.Object {
	obj := nativeImage("MONOCHROME2", 1, bits, bits, 0, pixels)
	obj.Put(text(0x0028, 0x0008, core.VRIS, "1"))
	return obj
}

func TestNativePixelDataUnderEncapsulatedIsOptIn(t *testing.T) {
	pixels := make([]byte, pixelsPerFrame)
	for i := range pixels {
		pixels[i] = byte(i * 17)
	}
	obj := nativeUnderJPEG(8, pixels)
	options := pixeldata.DecompressOptions{Registry: jpegDecoders(t)}
	if _, _, err := pixeldata.DecompressDataSet(obj, transfer.JPEGBaseline, options); err == nil {
		t.Fatal("strict decode accepted native Pixel Data under JPEG Baseline")
	}
	var report pixeldata.DecompressReport
	options.Recovery.NativePixelDataUnderEncapsulated = true
	options.Report = &report
	decoded, syntax, err := pixeldata.DecompressDataSet(obj, transfer.JPEGBaseline, options)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := decoded.GetRaw(core.TagPixelData)
	if syntax.UID != transfer.ExplicitVRLittleEndian.UID || !bytes.Equal(raw, pixels) {
		t.Fatalf("decoded %s %v", syntax.UID, raw)
	}
	if len(report.Diagnostics) != 1 || report.Diagnostics[0] != pixeldata.DiagnosticNativePixelDataUnderEncapsulated {
		t.Fatalf("diagnostics %v", report.Diagnostics)
	}

	// Only the exact native size (plus one pad byte for an odd size) is read
	// as native; anything else stays an error.
	for _, size := range []int{pixelsPerFrame - 2, pixelsPerFrame + 2, 2 * pixelsPerFrame} {
		obj := nativeUnderJPEG(8, make([]byte, size))
		if _, _, err := pixeldata.DecompressDataSet(obj, transfer.JPEGBaseline, options); err == nil {
			t.Fatalf("%d bytes read as native", size)
		}
	}
	odd := nativeImage("MONOCHROME2", 1, 8, 8, 0, make([]byte, pixelsPerFrame))
	odd.Put(us(0x0028, 0x0011, 3)) // 3x3: 9 bytes plus a pad byte
	odd.Put(text(0x0028, 0x0008, core.VRIS, "1"))
	odd.Put(core.NewRawElement(core.TagPixelData, core.VROB, make([]byte, 10)))
	if _, _, err := pixeldata.DecompressDataSet(odd, transfer.JPEGBaseline, options); err != nil {
		t.Fatalf("odd size with its pad byte: %v", err)
	}
}

func TestTranscodeCorrectsSyntaxOfNativePixelData(t *testing.T) {
	pixels := make([]byte, 2*pixelsPerFrame)
	for i := range pixels {
		pixels[i] = byte(i * 5)
	}
	file := &object.File{
		Meta: object.FromElements([]core.Element{
			text(0x0002, 0x0010, core.VRUI, transfer.JPEGBaseline.UID),
		}, nil),
		Dataset:        nativeUnderJPEG(16, pixels),
		TransferSyntax: transfer.JPEGBaseline,
	}
	options := pixeldata.TranscodeOptions{DecoderRegistry: jpegDecoders(t)}
	if _, _, err := pixeldata.TranscodeFile(context.Background(), file, transfer.ExplicitVRLittleEndian, options); err == nil {
		t.Fatal("strict transcode accepted native Pixel Data under JPEG Baseline")
	}
	options.Recovery.NativePixelDataUnderEncapsulated = true
	out, report, err := pixeldata.TranscodeFile(context.Background(), file, transfer.ExplicitVRLittleEndian, options)
	if err != nil {
		t.Fatal(err)
	}
	if uid, _ := out.Meta.GetString(core.NewTag(0x0002, 0x0010)); uid != transfer.ExplicitVRLittleEndian.UID {
		t.Fatalf("output Transfer Syntax UID %q", uid)
	}
	raw, _ := out.Dataset.GetRaw(core.TagPixelData)
	if !bytes.Equal(raw, pixels) || len(report.DecodeDiagnostics) != 1 || report.DecodeDiagnostics[0] != pixeldata.DiagnosticNativePixelDataUnderEncapsulated {
		t.Fatalf("pixels changed or diagnostics %v", report.DecodeDiagnostics)
	}
}
