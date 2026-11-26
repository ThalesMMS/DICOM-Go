package pixeldata_test

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/render"
)

// The corpus remains external: gdcmData has no license file. Its pinned
// dimension/pixel oracles are in testdata/upstream/gdcm/md5_data_images.json.
func TestLIBIDOGDCMDataDimensionsAndRender(t *testing.T) {
	root := os.Getenv("DICOMGO_GDCMDATA_ROOT")
	if root == "" {
		t.Skip("set DICOMGO_GDCMDATA_ROOT to a separately obtained GDCMData checkout")
	}
	for _, tc := range []struct {
		name    string
		rows    uint16
		columns uint16
		samples uint16
		md5     string
	}{
		{"LIBIDO-24-ACR_NEMA-Rectangle.dcm", 100, 400, 3, "81a40454eec2b18f4331cfd1ba4e501e"},
		{"gdcm-ACR-LibIDO.acr", 301, 512, 1, "59d9851ca0f214d57fdfd6a8c13bc91c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, _, err := object.OpenFileWithTransferSyntaxRecovery(filepath.Join(root, tc.name), object.ReadFileOptions{}, object.TransferSyntaxRecoveryOptions{
				AllowMissingPreamble: true, AllowMissingFileMeta: true, AllowLegacySyntaxes: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			original, _ := file.Dataset.GetRaw(core.TagPixelData)
			report := &pixeldata.DecompressReport{}
			recovered, err := pixeldata.DecompressFile(file, pixeldata.DecompressOptions{
				Recovery: pixeldata.DecodeRecovery{DefaultImagePixelModule: true}, Report: report,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(report.Diagnostics, pixeldata.DiagnosticLIBIDODimensions) || tc.samples == 3 && !slices.Contains(report.Diagnostics, pixeldata.DiagnosticLIBIDORGB24) {
				t.Fatalf("missing LIBIDO diagnostics %v", report.Diagnostics)
			}
			upgraded, upgradeReport, err := object.UpgradeLegacy(file)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(upgradeReport.Diagnostics, pixeldata.DiagnosticLIBIDODimensions) || tc.samples == 3 && !slices.Contains(upgradeReport.Diagnostics, pixeldata.DiagnosticLIBIDORGB24) {
				t.Fatalf("missing upgrade diagnostics %+v", upgradeReport)
			}
			var encoded bytes.Buffer
			if err := object.WriteFile(&encoded, upgraded); err != nil {
				t.Fatal(err)
			}
			strict, err := object.ReadFile(bytes.NewReader(encoded.Bytes()))
			if err != nil {
				t.Fatalf("strict upgraded Part 10 read: %v", err)
			}
			strictNative, err := pixeldata.DecompressFile(strict, pixeldata.DecompressOptions{})
			if err != nil {
				t.Fatalf("strict upgraded pixel decode: %v", err)
			}
			var recoveredMetadata pixeldata.Metadata
			for name, output := range map[string]*object.File{"decode recovery": recovered, "upgrade round trip": strictNative} {
				native, err := pixeldata.ExtractNativeFramesView(output.Dataset)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				metadata := native.Metadata
				if metadata.Rows != tc.rows || metadata.Columns != tc.columns || metadata.NumberOfFrames != 1 || metadata.BitsAllocated != 8 || metadata.BitsStored != 8 || metadata.HighBit != 7 || metadata.SamplesPerPixel != tc.samples {
					t.Fatalf("%s metadata %+v", name, metadata)
				}
				if recoveredMetadata != (pixeldata.Metadata{}) && recoveredMetadata != metadata {
					t.Fatalf("recovery/upgrade metadata differ: %+v / %+v", recoveredMetadata, metadata)
				}
				recoveredMetadata = metadata
				if !bytes.Equal(native.Data[0], original) {
					t.Fatalf("%s reordered native pixels", name)
				}
				sum := md5.Sum(native.Data[0])
				if got := hex.EncodeToString(sum[:]); got != tc.md5 {
					t.Fatalf("%s pixel MD5 %s, want %s", name, got, tc.md5)
				}
				assertLIBIDORenderRowMajor(t, metadata, native.Data[0])
				// The consumed marker cannot make another opt-in decode transpose
				// this already-normalized file.
				againReport := &pixeldata.DecompressReport{}
				again, err := pixeldata.DecompressFile(output, pixeldata.DecompressOptions{
					Recovery: pixeldata.DetectableDecodeRecovery(), Report: againReport,
				})
				if err != nil || len(againReport.Diagnostics) != 0 {
					t.Fatalf("%s repeat recovery: %v %v", name, err, againReport.Diagnostics)
				}
				againMetadata, _ := pixeldata.ExtractMetadata(again.Dataset)
				if againMetadata != metadata {
					t.Fatalf("%s repeat recovery changed metadata", name)
				}
			}
			rows, _ := file.Dataset.GetInt(core.NewTag(0x0028, 0x0010))
			columns, _ := file.Dataset.GetInt(core.NewTag(0x0028, 0x0011))
			if rows != int64(tc.columns) || columns != int64(tc.rows) {
				t.Fatal("source metadata mutated")
			}
		})
	}
}

func assertLIBIDORenderRowMajor(t *testing.T, metadata pixeldata.Metadata, raw []byte) {
	t.Helper()
	img, err := render.RenderFrame(&render.Frame{
		Metadata: metadata, PixelBytes: raw, ByteOrder: binary.LittleEndian,
		Rescale: render.Rescale{Slope: 1},
	}, render.WindowLevel{Center: 128, Width: 256})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if img.Bounds().Dx() != int(metadata.Columns) || img.Bounds().Dy() != int(metadata.Rows) {
		t.Fatalf("rendered bounds %v", img.Bounds())
	}
	for y := 0; y < int(metadata.Rows); y++ {
		for x := 0; x < int(metadata.Columns); x++ {
			i := (y*int(metadata.Columns) + x) * int(metadata.SamplesPerPixel)
			wantR, wantG, wantB := uint32(raw[i])*257, uint32(raw[i])*257, uint32(raw[i])*257
			if metadata.SamplesPerPixel == 3 {
				wantG, wantB = uint32(raw[i+1])*257, uint32(raw[i+2])*257
			}
			r, g, b, a := img.At(x, y).RGBA()
			if r != wantR || g != wantG || b != wantB || a != 65535 {
				t.Fatalf("render (%d,%d)=(%d,%d,%d,%d), want row-major (%d,%d,%d,65535)", x, y, r, g, b, a, wantR, wantG, wantB)
			}
		}
	}
}
