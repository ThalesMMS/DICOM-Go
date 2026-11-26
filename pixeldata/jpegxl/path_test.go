package jpegxl_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpegxl"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// TranscodePath recompresses a JPEG Baseline file to .111 and verifies, by
// decoding both, that the pixels are unchanged.
func TestTranscodePathRecompressesAndVerifies(t *testing.T) {
	img := newImage("rgb8", 30, 26, 3, 8, 8, 0, "RGB", 1)
	opts := options(t)
	opts.AllowLossy = true
	baseline, _, err := pixeldata.TranscodeDataSet(context.Background(), img.native(), transfer.ExplicitVRLittleEndian, transfer.JPEGBaseline, opts)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := object.WriteFile(&buf, &object.File{Dataset: baseline, TransferSyntax: transfer.JPEGBaseline}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	source, dest := filepath.Join(dir, "source.dcm"), filepath.Join(dir, "derived.dcm")
	if err := os.WriteFile(source, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	opts.AllowLossy = false
	report, err := pixeldata.TranscodePath(context.Background(), source, dest, transfer.JPEGXLJPEGRecompression, opts)
	if err != nil {
		t.Fatal(err)
	}
	if report.Lossy || report.Frames != 1 {
		t.Fatalf("report = %+v", report)
	}
	r, err := os.Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	file, err := object.ReadFile(r)
	if err != nil {
		t.Fatal(err)
	}
	if file.TransferSyntax.UID != transfer.JPEGXLJPEGRecompression.UID {
		t.Fatalf("output syntax %s", file.TransferSyntax.UID)
	}
	restored, err := jpegxl.ReconstructJPEG(fragments(t, file.Dataset)[0])
	if err != nil {
		t.Fatal(err)
	}
	if want := fragments(t, baseline)[0]; !bytes.Equal(restored, want) && !bytes.Equal(append(restored, 0), want) {
		t.Fatal("stored .111 frame does not restore the source JPEG")
	}
}
