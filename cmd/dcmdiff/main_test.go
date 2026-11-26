package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpegls"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// monoFile writes an 8x8 MONOCHROME2 image of 12-bit samples in 16 bits.
func monoFile(t *testing.T, name string, sample func(i int) uint16) string {
	t.Helper()
	values := make([]uint16, 64)
	for i := range values {
		values[i] = sample(i)
	}
	return writeFile(t, name, transfer.ExplicitVRLittleEndian,
		dicomtest.NewUIElement(core.NewTag(0x0008, 0x0016), "1.2.840.10008.5.1.4.1.1.7"),
		dicomtest.NewUIElement(core.NewTag(0x0008, 0x0018), dicomtest.TestSOPInstanceUID),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0002), 1),
		dicomtest.NewStringElement(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME2"),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0010), 8),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0011), 8),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0100), 16),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0101), 12),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0102), 11),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0103), 0),
		dicomtest.Uint16Element(core.TagPixelData, core.VROW, binary.LittleEndian, values...),
	)
}

func metric(t *testing.T, stdout, name string) float64 {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		if value, ok := strings.CutPrefix(line, name+" "); ok {
			parsed, err := strconv.ParseFloat(value, 64)
			if err != nil {
				t.Fatal(err)
			}
			return parsed
		}
	}
	t.Fatalf("no %s in %q", name, stdout)
	return 0
}

// TestPixels: -pixels measures a near-lossless transcode against its
// source and the limits decide the exit status.
func TestPixels(t *testing.T) {
	reference := monoFile(t, "ref.dcm", func(i int) uint16 { return uint16(i * 61 % 4096) })
	decoders, err := builtin.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	encoders := pixeldata.NewMemoryEncoderRegistry()
	if err := jpegls.RegisterNearLosslessEncoder(encoders, jpegls.EncoderOptions{Near: 4, AllowLossy: true}); err != nil {
		t.Fatal(err)
	}
	lossy := filepath.Join(t.TempDir(), "near.dcm")
	if _, err := pixeldata.TranscodePath(context.Background(), reference, lossy, transfer.JPEGLSNearLossless,
		pixeldata.TranscodeOptions{DecoderRegistry: decoders, EncoderRegistry: encoders, AllowLossy: true}); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runDiff("-pixels", reference, lossy)
	if code != 1 || !strings.Contains(stdout, "frames 1\nsamples 64\n") {
		t.Fatalf("code %d stdout %q stderr %q", code, stdout, stderr)
	}
	maxError, psnr := metric(t, stdout, "max-error"), metric(t, stdout, "psnr-db")
	if maxError == 0 || maxError > 4 || psnr < 40 || math.IsInf(psnr, 0) {
		t.Fatalf("max-error %v psnr %v", maxError, psnr)
	}
	if code, stdout, _ := runDiff("-pixels", "-max-error", "4", "-min-psnr", "40", reference, lossy); code != 0 || strings.Contains(stdout, "!") {
		t.Fatalf("within limits: code %d %q", code, stdout)
	}
	if code, stdout, _ := runDiff("-pixels", "-max-error", "4", "-min-psnr", "200", reference, lossy); code != 1 || !strings.Contains(stdout, "! -min-psnr 200 exceeded: ") {
		t.Fatalf("PSNR limit: code %d %q", code, stdout)
	}
	if code, stdout, _ := runDiff("-pixels", "-q", "-max-rmse", "0.01", reference, lossy); code != 1 || stdout != "" {
		t.Fatalf("-q: code %d %q", code, stdout)
	}
	if code, stdout, _ := runDiff("-pixels", reference, reference); code != 0 || !strings.Contains(stdout, "psnr-db +Inf") {
		t.Fatalf("identical: code %d %q", code, stdout)
	}
	if code, stdout, _ := runDiff("-pixels", "-stored", reference, lossy); code != 1 || metric(t, stdout, "max-error") != maxError {
		t.Fatalf("-stored: code %d %q", code, stdout)
	}
	// An image of other dimensions differs.
	small := writeFile(t, "small.dcm", transfer.ExplicitVRLittleEndian,
		dicomtest.NewUIElement(core.NewTag(0x0008, 0x0016), "1.2.840.10008.5.1.4.1.1.7"),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0002), 1),
		dicomtest.NewStringElement(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME2"),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0010), 2),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0011), 2),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0100), 8),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0101), 8),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0102), 7),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0103), 0),
		dicomtest.NewOBElement(core.TagPixelData, []byte{1, 2, 3, 4}),
	)
	if code, stdout, _ := runDiff("-pixels", reference, small); code != 1 || !strings.Contains(stdout, "! pixels: ") {
		t.Fatalf("other dimensions: code %d %q", code, stdout)
	}
	for _, args := range [][]string{{"-max-error", "1", reference, lossy}, {"-stored", reference, lossy}, {"-pixels", "-meta", reference, lossy},
		{"-pixels", "-ignore", "Rows", reference, lossy}, {"-pixels", "-min-psnr", "-1", reference, lossy}} {
		if code, _, _ := runDiff(args...); code != 2 {
			t.Fatalf("%v: code %d, want 2", args, code)
		}
	}
}

func writeFile(t *testing.T, name string, syntax transfer.Syntax, elements ...core.Element) string {
	t.Helper()
	data, err := dicomtest.Part10File(syntax, elements...)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func base(instanceNumber, itemValue string, pixels []byte) []core.Element {
	return []core.Element{
		dicomtest.NewUIElement(core.NewTag(0x0008, 0x0016), dicomtest.TestSOPClassUID),
		dicomtest.NewUIElement(core.NewTag(0x0008, 0x0018), dicomtest.TestSOPInstanceUID),
		dicomtest.NewSequenceElement(core.NewTag(0x0008, 0x1115), core.DataSet{Elements: []core.Element{
			dicomtest.NewUIElement(core.NewTag(0x0020, 0x000E), itemValue),
		}}),
		dicomtest.NewStringElement(core.NewTag(0x0020, 0x0013), core.VRIS, instanceNumber),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0010), 8),
		dicomtest.NewOBElement(core.TagPixelData, pixels),
	}
}

func runDiff(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// TestDiffComparesValuesNotEncodings: the same attributes in Implicit and
// Explicit VR files match; changed, added and removed attributes, sequence
// items and binary values are each reported once.
func TestDiffComparesValuesNotEncodings(t *testing.T) {
	a := writeFile(t, "a.dcm", transfer.ExplicitVRLittleEndian, base("1", "1.2.3", []byte{1, 2, 3, 4})...)
	same := writeFile(t, "same.dcm", transfer.ImplicitVRLittleEndian, base("1", "1.2.3", []byte{1, 2, 3, 4})...)
	if code, stdout, stderr := runDiff(a, same); code != 0 || stdout != "" {
		t.Fatalf("equal files: code %d stdout %q stderr %q", code, stdout, stderr)
	}
	changed := append(base("2", "1.2.4", []byte{1, 2, 3, 5}), dicomtest.NewStringElement(core.NewTag(0x0008, 0x103E), core.VRLO, "ADDED"))
	b := writeFile(t, "b.dcm", transfer.ExplicitVRLittleEndian, changed...)
	code, stdout, _ := runDiff(a, b)
	want := []string{
		`> (0008,103E) SeriesDescription LO "ADDED"`,
		`! (0008,1115)[0].(0020,000E) SeriesInstanceUID UI "1.2.3" | "1.2.4"`,
		`! (0020,0013) InstanceNumber IS "1" | "2"`,
		`! (7FE0,0010) PixelData OB 4 bytes sha256 `,
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if code != 1 || len(lines) != len(want) {
		t.Fatalf("code %d, lines:\n%s", code, stdout)
	}
	for i := range want {
		if !strings.HasPrefix(lines[i], want[i]) {
			t.Fatalf("line %d = %q, want prefix %q", i, lines[i], want[i])
		}
	}
	code, stdout, _ = runDiff("-ignore", "InstanceNumber", "-ignore", "(0008,103E)", "-ignore", "7FE00010", "-ignore", "SeriesInstanceUID", a, b)
	if code != 0 || stdout != "" {
		t.Fatalf("ignored differences: code %d stdout %q", code, stdout)
	}
	if code, stdout, _ = runDiff("-q", a, b); code != 1 || stdout != "" {
		t.Fatalf("-q: code %d stdout %q", code, stdout)
	}
	if code, stdout, _ = runDiff("-meta", a, same); code != 1 || !strings.Contains(stdout, "(0002,0010) TransferSyntaxUID") {
		t.Fatalf("-meta: code %d stdout %q", code, stdout)
	}
}

func TestDiffErrors(t *testing.T) {
	a := writeFile(t, "a.dcm", transfer.ExplicitVRLittleEndian, base("1", "1.2.3", []byte{1, 2})...)
	for _, args := range [][]string{{a}, {"-ignore", "NoSuchKeyword", a, a}, {a, filepath.Join(t.TempDir(), "missing.dcm")}} {
		if code, _, _ := runDiff(args...); code != 2 {
			t.Fatalf("%v: code %d, want 2", args, code)
		}
	}
	if code, _, stderr := runDiff("-h"); code != 0 || !strings.Contains(stderr, "Usage: dcmdiff") {
		t.Fatalf("-h: %d %q", code, stderr)
	}
}

// TestDiffIgnoresByteOrder: OW values of a Big Endian file match the same
// values in a Little Endian file, and a changed word is still found.
func TestDiffIgnoresByteOrder(t *testing.T) {
	le := writeFile(t, "le.dcm", transfer.ExplicitVRLittleEndian, dicomtest.Uint16Element(core.TagPixelData, core.VROW, binary.LittleEndian, 1, 2, 3))
	be := writeFile(t, "be.dcm", transfer.ExplicitVRBigEndian, dicomtest.Uint16Element(core.TagPixelData, core.VROW, binary.BigEndian, 1, 2, 3))
	if code, stdout, _ := runDiff(le, be); code != 0 {
		t.Fatalf("same words in both byte orders: %q", stdout)
	}
	other := writeFile(t, "other.dcm", transfer.ExplicitVRBigEndian, dicomtest.Uint16Element(core.TagPixelData, core.VROW, binary.BigEndian, 1, 2, 4))
	if code, _, _ := runDiff(le, other); code != 1 {
		t.Fatal("changed word not reported")
	}
}
