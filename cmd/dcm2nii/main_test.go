package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/imagebuild"
	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/object"
)

// writeSeries writes n synthetic 5x4 CT slices 2 mm apart into dir.
func writeSeries(t *testing.T, dir, seriesUID string, n int) {
	t.Helper()
	geometry := imagebuild.Geometry{Rows: 4, Columns: 5, PixelSpacing: [2]float64{0.75, 0.5},
		RowDirection: [3]float64{1, 0, 0}, ColumnDirection: [3]float64{0, 1, 0}, SliceThickness: 2}
	slices := make([]imagebuild.Slice, n)
	for z := range slices {
		slices[z].Position = [3]float64{-10, -20, 30 + 2*float64(z)}
		for p := 0; p < 20; p++ {
			slices[z].Values = append(slices[z].Values, float64(-1000+p*37+z*101))
		}
	}
	files, err := imagebuild.Build(geometry, slices, imagebuild.Options{IOD: imagebuild.CT, RescaleType: "HU", SeriesInstanceUID: seriesUID,
		ContentTime: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for z, file := range files {
		var buf bytes.Buffer
		if err := object.WriteFile(&buf, file); err != nil {
			t.Fatal(err)
		}
		// Reverse file order so the tool must sort the slices.
		if err := os.WriteFile(filepath.Join(dir, string(rune('z'-z))+".dcm"), buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func runTool(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestConvert(t *testing.T) {
	root := t.TempDir()
	writeSeries(t, filepath.Join(root, "a"), "1.2.826.0.1.3680043.10.1.1195.1", 3)
	if err := os.WriteFile(filepath.Join(root, "a", "notes.txt"), []byte("not DICOM"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "ct.nii.gz")
	sidecar := filepath.Join(root, "ct.json")
	code, stdout, stderr := runTool("-sidecar", sidecar, "-o", out, filepath.Join(root, "a"))
	if code != 0 || !strings.Contains(stdout, "dcm2nii: 5x4x3 voxels") {
		t.Fatalf("code %d stdout %q stderr %q", code, stdout, stderr)
	}
	data, err := os.ReadFile(out)
	if err != nil || !bytes.HasPrefix(data, []byte{0x1f, 0x8b}) {
		t.Fatalf("output is not gzip: %v", err)
	}
	volume, err := nifti.ReadFile(context.Background(), out, nifti.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if volume.Dimensions != [4]int{5, 4, 3, 1} {
		t.Fatalf("dims %v", volume.Dimensions)
	}
	// i runs along x at 0.5 mm, j along y at 0.75 mm, k along z at 2 mm.
	want := [16]float64{0.5, 0, 0, -10, 0, 0.75, 0, -20, 0, 0, 2, 30, 0, 0, 0, 1}
	for i := range want {
		if math.Abs(volume.Geometry.IndexToLPS[i]-want[i]) > 1e-5 {
			t.Fatalf("IndexToLPS %v, want %v", volume.Geometry.IndexToLPS, want)
		}
	}
	for k := 0; k < 3; k++ {
		for p := 0; p < 20; p++ {
			if got := volume.Value(volume.Index(p%5, p/5, k, 0)); got != float64(-1000+p*37+k*101) {
				t.Fatalf("voxel %d of slice %d = %v", p, k, got)
			}
		}
	}
	var meta map[string]any
	if raw, err := os.ReadFile(sidecar); err != nil || json.Unmarshal(raw, &meta) != nil || meta["Datatype"] == nil && meta["datatype"] == nil {
		t.Fatalf("sidecar %v %v", err, meta)
	}

	// -scaling float32 writes rescaled values.
	plain := filepath.Join(root, "ct.nii")
	if code, _, stderr := runTool("-scaling", "float32", "-o", plain, filepath.Join(root, "a")); code != 0 {
		t.Fatal(stderr)
	}
	if volume, err := nifti.ReadFile(context.Background(), plain, nifti.ReadOptions{}); err != nil || volume.Header.Datatype != nifti.DatatypeFloat32 {
		t.Fatalf("float32: %v", err)
	}
}

func TestSeriesSelection(t *testing.T) {
	root := t.TempDir()
	writeSeries(t, filepath.Join(root, "a"), "1.2.826.0.1.3680043.10.1.1195.1", 2)
	writeSeries(t, filepath.Join(root, "b"), "1.2.826.0.1.3680043.10.1.1195.2", 3)
	out := filepath.Join(root, "out.nii")
	if code, _, stderr := runTool("-o", out, root); code != 1 || !strings.Contains(stderr, "2 series") || strings.Contains(stderr, "1.2.826") {
		t.Fatalf("two series: code %d %q", code, stderr)
	}
	if code, stdout, stderr := runTool("-series", "1.2.826.0.1.3680043.10.1.1195.2", "-o", out, root); code != 0 || !strings.Contains(stdout, "5x4x3") {
		t.Fatalf("-series: code %d %q %q", code, stdout, stderr)
	}
	if code, _, _ := runTool("-series", "1.2.3.4", "-o", out, root); code != 1 {
		t.Fatalf("unknown series: code %d", code)
	}
	single := filepath.Join(root, "single")
	writeSeries(t, single, "1.2.826.0.1.3680043.10.1.1195.3", 1)
	if code, _, stderr := runTool("-o", out, single); code != 1 || !strings.Contains(stderr, "dcm2nii: ") {
		t.Fatalf("single slice rejected by default: code %d %q", code, stderr)
	}
	if code, stdout, stderr := runTool("-single-slice", "patient", "-o", out, single); code != 0 || !strings.Contains(stdout, "5x4x1") {
		t.Fatalf("-single-slice patient: code %d %q %q", code, stdout, stderr)
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{{}, {"x"}, {"-o", "x.nii"}, {"-scaling", "int8", "-o", "x.nii", "y"}, {"-single-slice", "maybe", "-o", "x.nii", "y"}} {
		if code, _, _ := runTool(args...); code != 2 {
			t.Fatalf("%v: code %d, want 2", args, code)
		}
	}
	if code, _, stderr := runTool("-h"); code != 0 || !strings.Contains(stderr, "Usage: dcm2nii") {
		t.Fatalf("-h: %d %q", code, stderr)
	}
}
