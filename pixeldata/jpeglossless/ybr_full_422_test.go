package jpeglossless

import (
	"bytes"
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// #1272: YBR_FULL_422 is not a lossless JPEG Photometric Interpretation
// (PS3.5 Table 8.2.1-2), but PS3.5 8.2.1 lets the codestream control
// decompression. A full resolution codestream decodes unconverted, reported
// as YBR_FULL with a diagnostic.
func TestJPEGLosslessYBRFull422FullResolutionDecodesAsYBRFull(t *testing.T) {
	for _, tc := range []struct {
		name  string
		codec *Codec
		data  string
	}{
		{"interleaved scan", newSV1(), conformanceInterleavedBase64},
		{"separate component scans", New(), conformanceSeparateBase64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := conformanceDecodeBase64(t, tc.data)
			frames, err := tc.codec.Decode(encapsulated(stream), conformanceColorObject(3, 4, "YBR_FULL_422", 0, 0))
			if err != nil {
				t.Fatalf("Decode() YBR_FULL_422 with 1x1 components: %v", err)
			}
			if !bytes.Equal(frames.Data[0], conformanceRGBReference) {
				t.Fatal("samples differ from the stored components")
			}
			if frames.PhotometricInterpretation != "YBR_FULL" {
				t.Fatalf("PhotometricInterpretation = %q, want YBR_FULL", frames.PhotometricInterpretation)
			}
			if !slices.Equal(frames.Diagnostics, []string{pixeldata.DiagnosticJPEGLosslessYBRFull422FullResolution}) {
				t.Fatalf("Diagnostics = %v", frames.Diagnostics)
			}
			// A conforming label is decoded as before, without diagnostics.
			plain, err := tc.codec.Decode(encapsulated(stream), conformanceColorObject(3, 4, "YBR_FULL", 0, 0))
			if err != nil || len(plain.Diagnostics) != 0 || plain.PhotometricInterpretation != "" {
				t.Fatalf("YBR_FULL decode: %v %v %q", err, plain.Diagnostics, plain.PhotometricInterpretation)
			}
		})
	}
}

// Decompression writes the reported label: the native data set is YBR_FULL
// with one Y Cb Cr triplet per pixel, and the report carries the diagnostic.
func TestDecompressJPEGLosslessYBRFull422FullResolution(t *testing.T) {
	stream := conformanceDecodeBase64(t, conformanceInterleavedBase64)
	if len(stream)%2 == 1 {
		stream = append(stream, 0)
	}
	obj := conformanceColorObject(3, 4, "YBR_FULL_422", 0, 0)
	obj.Put(dicomtest.NewFragmentSequenceElement(core.TagPixelData, nil, stream))
	registry := pixeldata.NewMemoryRegistry()
	if err := Register(registry); err != nil {
		t.Fatal(err)
	}
	report := &pixeldata.DecompressReport{}
	out, syntax, err := pixeldata.DecompressDataSet(obj, transfer.JPEGLosslessSV1, pixeldata.DecompressOptions{Registry: registry, Report: report})
	if err != nil {
		t.Fatal(err)
	}
	if syntax.UID != transfer.ExplicitVRLittleEndian.UID {
		t.Fatalf("output syntax %s", syntax.UID)
	}
	if photometric, _ := out.GetString(tagPhotometric); photometric != "YBR_FULL" {
		t.Fatalf("output Photometric Interpretation %q, want YBR_FULL", photometric)
	}
	if raw, _ := out.GetRaw(core.TagPixelData); !bytes.Equal(raw, conformanceRGBReference) {
		t.Fatalf("output Pixel Data %v", raw)
	}
	if !slices.Contains(report.Diagnostics, pixeldata.DiagnosticJPEGLosslessYBRFull422FullResolution) {
		t.Fatalf("report %v", report.Diagnostics)
	}
}

// Real chroma subsampling in a SOF3 stays unsupported, whatever the label.
func TestJPEGLosslessRefusesSubsampledComponents(t *testing.T) {
	for _, sampling := range []byte{0x21, 0x22, 0x12} {
		for _, photometric := range []string{"YBR_FULL_422", "YBR_FULL", "RGB"} {
			t.Run(fmt.Sprintf("%s Y %dx%d", photometric, sampling>>4, sampling&0x0f), func(t *testing.T) {
				stream := conformanceDecodeBase64(t, conformanceInterleavedBase64)
				sof := conformanceSegmentPayload(t, stream, markerSOF3, 0)
				stream[sof+6+1] = sampling // component 0 (Y) Hi/Vi
				_, err := newSV1().Decode(encapsulated(stream), conformanceColorObject(3, 4, photometric, 0, 0))
				if !errors.Is(err, ErrUnsupportedScan) || !strings.Contains(err.Error(), "subsampled") {
					t.Fatalf("Decode() error = %v, want subsampled ErrUnsupportedScan", err)
				}
			})
		}
	}
}

// TestJPEGLosslessYBRFull422GDCMDataInterop decodes the gdcmData file whose
// SOF3 has three 1x1 components under a YBR_FULL_422 label. The samples match
// GDCM's pixel MD5 (gdcmMD5DataImages, decoded without colour conversion) and
// DCMTK dcmdjpeg +cn, which also writes YBR_FULL; dcmdjpeg's default instead
// converts to RGB. Opt-in: DICOMGO_GDCMDATA_ROOT at gdcmData 9b38ac7a.
func TestJPEGLosslessYBRFull422GDCMDataInterop(t *testing.T) {
	root := os.Getenv("DICOMGO_GDCMDATA_ROOT")
	if root == "" {
		t.Skip("set DICOMGO_GDCMDATA_ROOT to a separately obtained GDCMData checkout")
	}
	data := conformanceReadPinnedFixture(t, filepath.Join(root, "JPEGLosslessYBR_FULL_422.dcm"),
		"60eeb235fe980e8dc8310e280819659fb71656592c12c5fe1307233aea20adab")
	file, err := object.ReadFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	registry := pixeldata.NewMemoryRegistry()
	if err := Register(registry); err != nil {
		t.Fatal(err)
	}
	report := &pixeldata.DecompressReport{}
	out, err := pixeldata.DecompressFile(file, pixeldata.DecompressOptions{Registry: registry, Report: report})
	if err != nil {
		t.Fatal(err)
	}
	if photometric, _ := out.Dataset.GetString(tagPhotometric); photometric != "YBR_FULL" {
		t.Fatalf("output Photometric Interpretation %q, want YBR_FULL", photometric)
	}
	raw, _ := out.Dataset.GetRaw(core.TagPixelData)
	if len(raw) != 600*800*3 {
		t.Fatalf("decoded %d bytes, want %d", len(raw), 600*800*3)
	}
	if got := fmt.Sprintf("%x", md5.Sum(raw)); got != "cdfb5dd0c999d88badeb00a16f96554f" {
		t.Fatalf("pixel MD5 %s, want GDCM's cdfb5dd0c999d88badeb00a16f96554f", got)
	}
	if !slices.Contains(report.Diagnostics, pixeldata.DiagnosticJPEGLosslessYBRFull422FullResolution) {
		t.Fatalf("report %v", report.Diagnostics)
	}

	// With DICOMGO_DCMTK_INTEGRATION=1, dcmdjpeg +cn (no colour conversion)
	// is a second, independent decoder of the same file.
	dcmdjpeg, ok := dcmtkoracle.Lookup("dcmdjpeg")
	if !ok {
		t.Log("DCMTK dcmdjpeg not enabled: compared with GDCM's MD5 only")
		return
	}
	output := filepath.Join(t.TempDir(), "dcmdjpeg-cn.dcm")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if combined, err := dcmtkoracle.Command(ctx, t, dcmtkoracle.Exact, dcmdjpeg, "+cn", filepath.Join(root, "JPEGLosslessYBR_FULL_422.dcm"), output).CombinedOutput(); err != nil {
		t.Fatalf("dcmdjpeg +cn: %v: %s", err, combined)
	}
	reference, err := object.OpenFile(output)
	if err != nil {
		t.Fatal(err)
	}
	defer reference.Close()
	if photometric, _ := reference.Dataset.GetString(tagPhotometric); photometric != "YBR_FULL" {
		t.Fatalf("dcmdjpeg +cn Photometric Interpretation %q", photometric)
	}
	if want, _ := reference.Dataset.GetRaw(core.TagPixelData); !bytes.Equal(raw, want) {
		t.Fatal("samples differ from dcmdjpeg +cn")
	}
}
