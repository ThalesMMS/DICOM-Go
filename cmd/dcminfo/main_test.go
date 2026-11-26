package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/encapdoc"
	"github.com/ThalesMMS/dicom-go/imagebuild"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpegls"
	"github.com/ThalesMMS/dicom-go/pixeldata/rle"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// ctImage writes a synthetic 5x4 CT image with a marker patient name.
func ctImage(t *testing.T, dir string) string {
	t.Helper()
	geometry := imagebuild.Geometry{Rows: 4, Columns: 5, PixelSpacing: [2]float64{1, 1},
		RowDirection: [3]float64{1, 0, 0}, ColumnDirection: [3]float64{0, 1, 0}, SliceThickness: 1}
	values := make([]float64, 20)
	for p := range values {
		values[p] = float64(100 + 61*p)
	}
	files, err := imagebuild.Build(geometry, []imagebuild.Slice{{Values: values}}, imagebuild.Options{
		IOD: imagebuild.CT, RescaleType: "HU", ContentTime: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	files[0].Dataset.Put(core.NewRawElement(core.NewTag(0x0010, 0x0010), core.VRPN, []byte("SECRET^NAME ")))
	return writeFile(t, filepath.Join(dir, "ct.dcm"), files[0])
}

func writeFile(t *testing.T, path string, file *object.File) string {
	t.Helper()
	var buf bytes.Buffer
	if err := object.WriteFile(&buf, file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// transcode writes source in target, near-lossless JPEG-LS when near > 0.
func transcode(t *testing.T, source, name string, target transfer.Syntax, near int) string {
	t.Helper()
	decoders, err := builtin.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	encoders := pixeldata.NewMemoryEncoderRegistry()
	err = errors.Join(rle.RegisterEncoder(encoders), jpegls.RegisterEncoder(encoders))
	if near > 0 {
		err = errors.Join(err, jpegls.RegisterNearLosslessEncoder(encoders, jpegls.EncoderOptions{Near: near, AllowLossy: true}))
	}
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(filepath.Dir(source), name)
	if _, err := pixeldata.TranscodePath(context.Background(), source, out, target, pixeldata.TranscodeOptions{
		DecoderRegistry: decoders, EncoderRegistry: encoders, AllowLossy: near > 0}); err != nil {
		t.Fatal(err)
	}
	return out
}

func runTool(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func summaryOf(t *testing.T, args ...string) summary {
	t.Helper()
	code, stdout, stderr := runTool(append([]string{"-json"}, args...)...)
	if code != 0 {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	var s summary
	if err := json.Unmarshal([]byte(stdout), &s); err != nil {
		t.Fatalf("%v: %q", err, stdout)
	}
	return s
}

func TestImageSummaries(t *testing.T) {
	dir := t.TempDir()
	native := ctImage(t, dir)
	code, stdout, stderr := runTool("-md5", native)
	if code != 0 {
		t.Fatalf("code %d %q", code, stderr)
	}
	for _, want := range []string{"sop-class:      1.2.840.10008.5.1.4.1.1.2 (CT Image Storage)", "image:          5x4, 1 frame(s), 1 sample(s) MONOCHROME2",
		"pixel-data:     native, 40 bytes", "lossy:          no", "pixel-md5:"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("missing %q in:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "SECRET") {
		t.Fatalf("patient data printed:\n%s", stdout)
	}
	file, err := os.Open(native)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := object.ReadFile(file)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	frames, err := pixeldata.ExtractNativeFramesView(parsed.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	sum := md5.Sum(frames.Data[0])
	nativeMD5 := hex.EncodeToString(sum[:])
	if s := summaryOf(t, "-md5", native); s.PixelData.MD5 != nativeMD5 {
		t.Fatalf("md5 %s, want %s", s.PixelData.MD5, nativeMD5)
	}

	// A lossless transcode keeps the pixel MD5; the codestream says lossless.
	lossless := summaryOf(t, "-md5", transcode(t, native, "jls.dcm", transfer.JPEGLSLossless, 0))
	if lossless.PixelData.MD5 != nativeMD5 || lossless.PixelData.Lossy || !lossless.PixelData.Encapsulated || lossless.PixelData.Fragments != 1 {
		t.Fatalf("JPEG-LS lossless %+v", lossless.PixelData)
	}
	if c := lossless.PixelData.Codestream; c == nil || c.Format != "JPEG-LS" || c.Process != "lossless (NEAR 0)" || c.Columns != 5 || c.Rows != 4 || c.Components != 1 {
		t.Fatalf("codestream %+v", lossless.PixelData.Codestream)
	}
	near := summaryOf(t, transcode(t, native, "near.dcm", transfer.JPEGLSNearLossless, 3))
	if c := near.PixelData.Codestream; !near.PixelData.Lossy || c == nil || c.Process != "near-lossless (NEAR 3)" {
		t.Fatalf("near-lossless %+v %+v", near.PixelData, near.PixelData.Codestream)
	}
	if rleSummary := summaryOf(t, "-md5", transcode(t, native, "rle.dcm", transfer.RLELossless, 0)); rleSummary.PixelData.Codestream.Format != "RLE" || rleSummary.PixelData.MD5 != nativeMD5 {
		t.Fatalf("RLE %+v", rleSummary.PixelData)
	}
	bigEndian := transcode(t, native, "be.dcm", transfer.ExplicitVRBigEndian, 0)
	if s := summaryOf(t, "-md5", bigEndian); s.PixelData.MD5 != nativeMD5 {
		t.Fatalf("Big Endian md5 %s, want %s", s.PixelData.MD5, nativeMD5)
	}
}

func TestDocumentSummary(t *testing.T) {
	dir := t.TempDir()
	pdf := []byte("%PDF-1.4\n1 0 obj << >> endobj\ntrailer << >>\n%%EOF\n")
	file, err := encapdoc.NewPDF(encapdoc.PDFOptions{
		SOPInstanceUID: "1.2.826.0.1.3680043.10.1.1194.1", StudyInstanceUID: "1.2.826.0.1.3680043.10.1.1194.2", SeriesInstanceUID: "1.2.826.0.1.3680043.10.1.1194.3",
		PatientName: "SECRET^NAME", DocumentTitle: "SECRET TITLE", Data: pdf,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := writeFile(t, filepath.Join(dir, "doc.dcm"), file)
	sum := md5.Sum(pdf)
	code, stdout, _ := runTool(path)
	want := "document:       application/pdf, " + strconv.Itoa(len(pdf)) + " bytes, PDF 1.4, md5 " + hex.EncodeToString(sum[:])
	if code != 0 || !strings.Contains(stdout, want) || strings.Contains(stdout, "SECRET") {
		t.Fatalf("code %d, want %q in:\n%s", code, want, stdout)
	}
}

func TestDocumentSummaryReportsContradictoryLengthWithoutPayload(t *testing.T) {
	pdf := []byte("%PDF-1.7\nPRIVATE-PAYLOAD\n%%EOF\n")
	file, err := encapdoc.NewPDF(encapdoc.PDFOptions{
		SOPInstanceUID: "2.25.1051833", StudyInstanceUID: "2.25.1051834", SeriesInstanceUID: "2.25.1051835",
		PatientName: "PRIVATE^NAME", DocumentTitle: "PRIVATE TITLE", Data: pdf,
	})
	if err != nil {
		t.Fatal(err)
	}
	file.Dataset.SetUL(core.NewTag(0x0042, 0x0015), 2)
	path := writeFile(t, filepath.Join(t.TempDir(), "doc.dcm"), file)
	code, stdout, stderr := runTool("-json", path)
	if code != 0 || strings.Contains(stdout+stderr, "PRIVATE") {
		t.Fatalf("summary code %d, expected a redacted diagnostic", code)
	}
	var result summary
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if result.Document != nil || !strings.Contains(result.DocumentFailure, "Encapsulated Document Length") {
		t.Fatalf("wanted a length diagnostic without derived document metadata or a digest")
	}
}

func TestErrors(t *testing.T) {
	dir := t.TempDir()
	native := ctImage(t, dir)
	notDICOM := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notDICOM, []byte("plain text"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runTool(notDICOM, native)
	if code != 1 || !strings.Contains(stderr, "dcminfo: ") || !strings.Contains(stdout, "CT Image Storage") {
		t.Fatalf("code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if code, _, _ := runTool(); code != 2 {
		t.Fatalf("no file: code %d", code)
	}
	if code, _, stderr := runTool("-h"); code != 0 || !strings.Contains(stderr, "Usage: dcminfo") {
		t.Fatalf("-h: %d %q", code, stderr)
	}
}
