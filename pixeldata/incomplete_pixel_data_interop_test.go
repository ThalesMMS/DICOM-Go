package pixeldata_test

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpeg"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpeglossless"
	"github.com/ThalesMMS/dicom-go/render"
)

// External files remain outside this repository; their availability and hashes
// are recorded independently. This is functional equivalence, not a benchmark.
func TestIncompletePixelDataGDCMCompleteFrames(t *testing.T) {
	root := os.Getenv("DICOMGO_GDCMDATA_ROOT")
	if root == "" {
		t.Skip("DICOMGO_GDCMDATA_ROOT is unset; external issue corpus is optional")
	}
	cases := []struct{ name, source, want string }{
		{"JPEGInvalidSecondFrag.dcm", "f04c83a5d49359b92d7fbb7b063b359000442b668b479ec15bb9a525e6b55fc0", "f111ed4eea73f535261039d3f7b112e9"},
		{"gdcm-JPEG-LossLess3a.dcm", "356d7d692f62eb159245d30be7cd480579a29a84769dce6d99e64026bde3aa9d", "ad67e448e8923c34f1e019c3395e616c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(root, tc.name)
			data, err := os.ReadFile(path)
			if os.IsNotExist(err) {
				t.Skip("external fixture unavailable")
			}
			if err != nil {
				t.Fatal(err)
			}
			before := sha256.Sum256(data)
			if hex.EncodeToString(before[:]) != tc.source {
				t.Fatal("external fixture source SHA differs from qualified input")
			}
			if f, err := object.ReadFile(bytes.NewReader(data)); f != nil || err == nil {
				t.Fatal("strict admitted malformed external input")
			}
			// Both historical files also have a wrong File Meta Group Length.
			// This separate existing flag reads metadata by group; incomplete-data mode
			// alone intentionally does not repair the unrelated File Meta problem.
			if f, err := object.ReadFileWithOptions(bytes.NewReader(data), object.ReadFileOptions{AllowMissingMetaElementGroupLength: true}); f != nil || err == nil {
				t.Fatal("metadata tolerance admitted truncated Pixel Data")
			}
			f, err := object.OpenFileWithOptions(path, object.ReadFileOptions{AllowIncompletePixelData: true, AllowMissingMetaElementGroupLength: true})
			if f == nil || !errors.Is(err, object.ErrPartialRead) {
				t.Fatalf("recover read %v", err)
			}
			defer f.Close()
			registry := pixeldata.NewMemoryRegistry()
			if err := jpeg.Register(registry); err != nil {
				t.Fatal(err)
			}
			if err := jpeglossless.Register(registry); err != nil {
				t.Fatal(err)
			}
			var report pixeldata.DecompressReport
			out, err := pixeldata.DecompressFile(f, pixeldata.DecompressOptions{Registry: registry, Recovery: pixeldata.DecodeRecovery{IncompleteData: true}, Report: &report})
			if err != nil {
				t.Fatal(err)
			}
			pixel, err := pixeldata.Extract(out.Dataset)
			if err != nil {
				t.Fatal(err)
			}
			sum := md5.Sum(pixel.Raw)
			if hex.EncodeToString(sum[:]) != tc.want {
				t.Fatalf("native MD5=%x expected=%s", sum, tc.want)
			}
			if report.Incomplete == nil || report.Incomplete.RetainedFrames != 1 {
				t.Fatalf("complete count %+v", report.Incomplete)
			}
			if _, err := incompleteAcceptanceImages(out, report); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path)
			if err != nil || sha256.Sum256(after) != before {
				t.Fatal("external source changed")
			}
			t.Logf("source_sha256=%x native_md5=%x reason=%s declared=%d retained=%d dropped_bytes=%d dropped_items=%d", before, sum, report.Incomplete.Kind, report.Incomplete.DeclaredFrames, report.Incomplete.RetainedFrames, report.Incomplete.DiscardedBytes, report.Incomplete.DiscardedFragments)
		})
	}
	// Historical GDCM pixel hashes are expectations, not source file hashes or
	// proof that a complete frame exists. A padded result must fail this gate.
	for _, tc := range []struct{ name, want string }{
		{"SIEMENS-IncompletePixelData.dcm", "dc549f804f1f59bfee245ddf100ee26b"},
		{"PMS-IncompletePixelData.dcm", "92655f77a55eb9c316223b9e73d55800"},
		{"GEMS-IncompletePixelData.dcm", "098bba76d4e75a3148c9b62aee35f950"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(root, tc.name)
			data, err := os.ReadFile(path)
			if os.IsNotExist(err) {
				t.Skip("original native acceptance fixture unavailable; complete-frame behavior remains unqualified")
			}
			if err != nil {
				t.Fatal(err)
			}
			before := sha256.Sum256(data)
			// The source SHA is recorded, never confused with an upstream-pinned
			// source identity. Provenance/permission review is still independent.
			t.Logf("source_sha256=%x source_pin=unavailable", before)
			out, report, err := incompleteNativeAcceptance(data)
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			pixel, err := pixeldata.Extract(out.Dataset)
			if err != nil {
				t.Fatal(err)
			}
			sum := md5.Sum(pixel.Raw)
			t.Logf("native_md5=%x reason=%s declared=%d retained=%d dropped_bytes=%d", sum, report.Incomplete.Kind, report.Incomplete.DeclaredFrames, report.Incomplete.RetainedFrames, report.Incomplete.DiscardedBytes)
			if hex.EncodeToString(sum[:]) != tc.want {
				t.Errorf("complete-prefix MD5=%x differs from historical GDCM MD5=%s; zero-fill equivalence is not accepted", sum, tc.want)
			}
			after, err := os.ReadFile(path)
			if err != nil || sha256.Sum256(after) != before {
				t.Fatal("external source changed")
			}
		})
	}
}

// Exercise the same read/decode/display acceptance path for the external native
// inputs and separately identified synthetic contract cases below.
func incompleteNativeAcceptance(data []byte) (*object.File, pixeldata.DecompressReport, error) {
	var report pixeldata.DecompressReport
	strict, err := object.ReadFileWithOptions(bytes.NewReader(data), object.ReadFileOptions{AllowMissingMetaElementGroupLength: true})
	if strict != nil || err == nil {
		if strict != nil {
			_ = strict.Close()
		}
		return nil, report, fmt.Errorf("strict reading admitted a supposedly incomplete fixture")
	}
	f, err := object.ReadFileWithOptions(bytes.NewReader(data), object.ReadFileOptions{AllowIncompletePixelData: true, AllowMissingMetaElementGroupLength: true})
	if f == nil || !errors.Is(err, object.ErrPartialRead) {
		if f != nil {
			_ = f.Close()
		}
		return nil, report, fmt.Errorf("incomplete read did not retain a diagnosed partial file: %w", err)
	}
	defer f.Close()
	info, certified := f.IncompletePixelData()
	if !certified || info.Kind != "native-value-eof" || f.TransferSyntax.Encapsulated {
		return nil, report, fmt.Errorf("fixture is not certified incomplete native Pixel Data")
	}
	metadata, err := pixeldata.ExtractMetadata(f.Dataset)
	if err != nil {
		return nil, report, err
	}
	if metadata.FrameSize() <= 0 {
		return nil, report, fmt.Errorf("fixture has invalid frame size")
	}
	wantFrames := int(info.AvailableBytes / metadata.FrameSize())
	out, err := pixeldata.DecompressFile(f, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{IncompleteData: true}, Report: &report})
	if err != nil {
		if out != nil {
			_ = out.Close()
		}
		return nil, report, fmt.Errorf("complete-frame recovery (declared=%d available=%d complete=%d): %w", metadata.NumberOfFrames, info.AvailableBytes, wantFrames, err)
	}
	if report.Incomplete == nil || report.Incomplete.RetainedFrames != wantFrames || report.Incomplete.DeclaredFrames != metadata.NumberOfFrames || report.Incomplete.AvailableBytes != info.AvailableBytes || report.Incomplete.DiscardedBytes != info.AvailableBytes-int64(wantFrames)*metadata.FrameSize() {
		_ = out.Close()
		return nil, report, fmt.Errorf("recovery count/byte accounting differs from actual native prefix: %+v", report.Incomplete)
	}
	if _, err := incompleteAcceptanceImages(out, report); err != nil {
		_ = out.Close()
		return nil, report, err
	}
	return out, report, nil
}

// Render every retained frame through render -> frame -> display, with a
// deterministic full stored-range window. This is headless 2D evidence only.
func incompleteAcceptanceImages(out *object.File, report pixeldata.DecompressReport) ([]image.Image, error) {
	if report.Incomplete == nil || !slices.Contains(report.Diagnostics, pixeldata.DiagnosticIncompletePixelData) {
		return nil, fmt.Errorf("recovery omitted incomplete-data diagnostic")
	}
	native, err := pixeldata.ExtractNativeFramesView(out.Dataset)
	if err != nil {
		return nil, err
	}
	if len(native.Data) == 0 || len(native.Data) != report.Incomplete.RetainedFrames || native.Metadata.NumberOfFrames != len(native.Data) {
		return nil, fmt.Errorf("recovered frame metadata/count differs from diagnostic")
	}
	width := math.Exp2(float64(native.Metadata.BitsStored))
	center := width / 2
	if native.Metadata.PixelRepresentation != 0 {
		center = 0
	}
	images := make([]image.Image, 0, len(native.Data))
	for i, raw := range native.Data {
		if int64(len(raw)) != native.Metadata.FrameSize() {
			return nil, fmt.Errorf("retained frame %d is not complete", i)
		}
		img, err := render.RenderFrame(&render.Frame{Metadata: native.Metadata, ByteOrder: out.Dataset.ValueByteOrder(), PixelBytes: raw, TransferSyntaxUID: out.TransferSyntax.UID}, render.WindowLevel{Center: center, Width: width})
		if err != nil {
			return nil, fmt.Errorf("display retained frame %d: %w", i, err)
		}
		if img == nil || img.Bounds().Dx() != int(native.Metadata.Columns) || img.Bounds().Dy() != int(native.Metadata.Rows) {
			return nil, fmt.Errorf("display retained frame %d has wrong dimensions", i)
		}
		images = append(images, img)
	}
	return images, nil
}
