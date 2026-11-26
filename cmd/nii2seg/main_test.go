package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/imagebuild"
	"github.com/ThalesMMS/dicom-go/internal/clifile"
	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/seg"
	"github.com/ThalesMMS/dicom-go/validation/iod"
)

// writeSeries writes three synthetic 5x4 CT slices 2 mm apart with a marker
// patient name.
func writeSeries(t *testing.T, dir string) {
	t.Helper()
	geometry := imagebuild.Geometry{Rows: 4, Columns: 5, PixelSpacing: [2]float64{0.75, 0.5},
		RowDirection: [3]float64{1, 0, 0}, ColumnDirection: [3]float64{0, 1, 0}, SliceThickness: 2, FrameOfReferenceUID: "1.2.826.0.1.3680043.10.1.1195.9"}
	slices := make([]imagebuild.Slice, 3)
	for z := range slices {
		slices[z] = imagebuild.Slice{Values: make([]float64, 20), Position: [3]float64{-10, -20, 30 + 2*float64(z)}}
	}
	files, err := imagebuild.Build(geometry, slices, imagebuild.Options{IOD: imagebuild.CT, ContentTime: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for z, file := range files {
		file.Dataset.Put(core.NewRawElement(core.NewTag(0x0010, 0x0010), core.VRPN, []byte("SYNTHETIC^PATIENT ")))
		var buf bytes.Buffer
		if err := object.WriteFile(&buf, file); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, string(rune('a'+z))+".dcm"), buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// writeLabelmap writes a uint8 labelmap on the series grid, or shifted by
// half a voxel.
func writeLabelmap(t *testing.T, path string, labels []byte, shift float64) {
	t.Helper()
	affine := render.GeometryAffine{0.5, 0, 0, -10 + shift, 0, 0.75, 0, -20, 0, 0, 2, 30, 0, 0, 0, 1}
	volume, err := nifti.NewVolume(context.Background(), bytes.NewReader(labels), nifti.NewVolumeOptions{Datatype: nifti.DatatypeUint8, Dimensions: [4]int{5, 4, 3, 1}, IndexToLPS: affine})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := nifti.WriteVolume(&buf, volume, nifti.CompressionGZIP); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runTool(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestLabelmapToSEG(t *testing.T) {
	root := t.TempDir()
	series := filepath.Join(root, "series")
	writeSeries(t, series)
	labels := make([]byte, 60)
	labels[1], labels[2], labels[20+7], labels[40+19] = 1, 1, 4, 1
	labelmap := filepath.Join(root, "labels.nii.gz")
	writeLabelmap(t, labelmap, labels, 0)
	table := filepath.Join(root, "labels.ctbl")
	if err := os.WriteFile(table, []byte("# synthetic\n1 Left_lobe 255 0 0 255\n4 Lesion 0 255 0 255\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "seg.dcm")
	code, stdout, stderr := runTool("-labels", table, "-o", out, labelmap, series)
	if code != 0 || !strings.Contains(stdout, "2 segment(s), 3 frame(s)") {
		t.Fatalf("code %d stdout %q stderr %q", code, stdout, stderr)
	}
	file, err := clifile.ReadPart10(out)
	if err != nil {
		t.Fatal(err)
	}
	report, err := iod.Validate(file.Dataset, iod.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range report.Errors() {
		t.Errorf("%s: %s", report.IOD, f)
	}
	document, err := seg.Read(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	names := map[int]string{}
	for _, segment := range document.Segments {
		names[segment.Number] = segment.Label
	}
	if names[1] != "Left lobe" || names[4] != "Lesion" || len(names) != 2 {
		t.Fatalf("segments %v", names)
	}
	if name, _ := file.Dataset.GetString(core.NewTag(0x0010, 0x0010)); !strings.HasPrefix(name, "SYNTHETIC^PATIENT") {
		t.Fatalf("patient module not inherited: %q", name)
	}
	if forUID, _ := file.GetUID(core.NewTag(0x0020, 0x0052)); forUID != "1.2.826.0.1.3680043.10.1.1195.9" {
		t.Fatalf("Frame of Reference %q", forUID)
	}

	// A labelmap half a voxel off the series grid is refused.
	shifted := filepath.Join(root, "shifted.nii.gz")
	writeLabelmap(t, shifted, labels, 0.25)
	if code, _, stderr := runTool("-o", filepath.Join(root, "x.dcm"), shifted, series); code != 1 || !strings.Contains(stderr, "nii2seg: ") {
		t.Fatalf("off-grid labelmap: code %d %q", code, stderr)
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{{}, {"-o", "x.dcm", "labels.nii"}, {"labels.nii", "series"}, {"-tolerance", "0.5", "-o", "x.dcm", "l.nii", "s"}} {
		if code, _, _ := runTool(args...); code != 2 {
			t.Fatalf("%v: code %d, want 2", args, code)
		}
	}
	if code, _, stderr := runTool("-h"); code != 0 || !strings.Contains(stderr, "Usage: nii2seg") {
		t.Fatalf("-h: %d %q", code, stderr)
	}
}
