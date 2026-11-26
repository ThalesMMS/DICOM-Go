package main

import (
	"bytes"
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/imagebuild"
	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/object"
)

// writeSeries writes four synthetic 5x4 CT slices of an oblique series
// (rows along (0.6, 0.8, 0), columns along (0, 0, -1)), 2.5 mm apart.
func writeSeries(t *testing.T, dir string) {
	t.Helper()
	geometry := imagebuild.Geometry{Rows: 4, Columns: 5, PixelSpacing: [2]float64{0.75, 0.5},
		RowDirection: [3]float64{0.6, 0.8, 0}, ColumnDirection: [3]float64{0, 0, -1}, SliceThickness: 2.5}
	slices := make([]imagebuild.Slice, 4)
	for z := range slices {
		// The normal is RowDirection x ColumnDirection = (-0.8, 0.6, 0).
		slices[z] = imagebuild.Slice{Values: make([]float64, 20), Position: [3]float64{12 - 0.8*2.5*float64(z), -7 + 0.6*2.5*float64(z), 40}}
		for p := range slices[z].Values {
			slices[z].Values[p] = float64(p + 10*z)
		}
	}
	files, err := imagebuild.Build(geometry, slices, imagebuild.Options{IOD: imagebuild.CT, ContentTime: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)})
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
		if err := os.WriteFile(filepath.Join(dir, string(rune('a'+z))+".dcm"), buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// buildTool compiles a sibling command for the round trip.
func buildTool(t *testing.T, name string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), name)
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	if output, err := exec.Command("go", "build", "-o", out, "../"+name).CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", name, err, output)
	}
	return out
}

func runTool(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func readVolume(t *testing.T, path string) *nifti.Volume {
	t.Helper()
	volume, err := nifti.ReadFile(context.Background(), path, nifti.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return volume
}

func sameGeometry(t *testing.T, label string, got, want *nifti.Volume) {
	t.Helper()
	if got.Dimensions != want.Dimensions {
		t.Fatalf("%s: dims %v, want %v", label, got.Dimensions, want.Dimensions)
	}
	for i := range want.Geometry.IndexToLPS {
		if math.Abs(got.Geometry.IndexToLPS[i]-want.Geometry.IndexToLPS[i]) > 1e-4 {
			t.Fatalf("%s: IndexToLPS %v, want %v", label, got.Geometry.IndexToLPS, want.Geometry.IndexToLPS)
		}
	}
}

// TestRoundTrip: DICOM -> NIfTI (dcm2nii) -> labelmap on that grid -> SEG
// (nii2seg) -> NIfTI (seg2nii) keeps the geometry and the labels, with the
// reference series and from the Segmentation's own planes.
func TestRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("builds dcm2nii and nii2seg")
	}
	dcm2nii, nii2seg := buildTool(t, "dcm2nii"), buildTool(t, "nii2seg")
	root := t.TempDir()
	series := filepath.Join(root, "series")
	writeSeries(t, series)
	image := filepath.Join(root, "image.nii.gz")
	if output, err := exec.Command(dcm2nii, "-o", image, series).CombinedOutput(); err != nil {
		t.Fatalf("dcm2nii: %v\n%s", err, output)
	}
	reference := readVolume(t, image)

	// Labels 1 and 2 on the first and last slices, 3 on slice 1.
	labels := make([]byte, 80)
	labels[0], labels[6], labels[20+12], labels[60+19] = 1, 2, 3, 1
	volume, err := nifti.NewVolume(context.Background(), bytes.NewReader(labels), nifti.NewVolumeOptions{
		Datatype: nifti.DatatypeUint8, Dimensions: reference.Dimensions, IndexToLPS: reference.Geometry.IndexToLPS})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := nifti.WriteVolume(&buf, volume, nifti.CompressionNone); err != nil {
		t.Fatal(err)
	}
	labelmap := filepath.Join(root, "labels.nii")
	if err := os.WriteFile(labelmap, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	segFile := filepath.Join(root, "seg.dcm")
	if output, err := exec.Command(nii2seg, "-o", segFile, labelmap, series).CombinedOutput(); err != nil {
		t.Fatalf("nii2seg: %v\n%s", err, output)
	}

	for _, withReference := range []bool{true, false} {
		out := filepath.Join(root, "back.nii.gz")
		args := []string{"-o", out, segFile}
		if withReference {
			args = append(args, series)
		}
		if code, stdout, stderr := runTool(args...); code != 0 || !strings.Contains(stdout, "5x4x4 voxels, 3 segment(s), 1 volume(s)") {
			t.Fatalf("reference %v: code %d %q %q", withReference, code, stdout, stderr)
		}
		back := readVolume(t, out)
		sameGeometry(t, "seg2nii", back, reference)
		for i, want := range labels {
			if back.Value(i) != float64(want) {
				t.Fatalf("reference %v: voxel %d = %v, want %d", withReference, i, back.Value(i), want)
			}
		}
	}

	// -split writes a binary volume per segment, and -labels a table that
	// reads back.
	split := filepath.Join(root, "segment.nii")
	table := filepath.Join(root, "segments.ctbl")
	if code, stdout, stderr := runTool("-split", "-labels", table, "-o", split, segFile, series); code != 0 || !strings.Contains(stdout, "3 volume(s)") {
		t.Fatalf("-split: code %d %q %q", code, stdout, stderr)
	}
	for number := 1; number <= 3; number++ {
		layer := readVolume(t, filepath.Join(root, "segment-"+string(rune('0'+number))+".nii"))
		for i, label := range labels {
			if want := float64(map[bool]int{true: 1}[int(label) == number]); layer.Value(i) != want {
				t.Fatalf("segment %d voxel %d = %v", number, i, layer.Value(i))
			}
		}
	}
	source, err := os.Open(table)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.Close() }()
	read, err := nifti.ReadLabelTable(source)
	if err != nil || read[3].Name != "Label 3" || len(read) != 3 {
		t.Fatalf("label table %v %v", read, err)
	}
}

func TestErrors(t *testing.T) {
	root := t.TempDir()
	series := filepath.Join(root, "series")
	writeSeries(t, series)
	if code, _, stderr := runTool("-o", filepath.Join(root, "x.nii"), filepath.Join(series, "a.dcm")); code != 1 || !strings.Contains(stderr, "seg2nii: ") {
		t.Fatalf("a CT image as SEG: code %d %q", code, stderr)
	}
	for _, args := range [][]string{{}, {"seg.dcm"}, {"-threshold", "0", "-o", "x.nii", "seg.dcm"}, {"-series", "1.2.3", "-o", "x.nii", "seg.dcm"}} {
		if code, _, _ := runTool(args...); code != 2 {
			t.Fatalf("%v: code %d, want 2", args, code)
		}
	}
	if code, _, stderr := runTool("-h"); code != 0 || !strings.Contains(stderr, "Usage: seg2nii") {
		t.Fatalf("-h: %d %q", code, stderr)
	}
	if got := splitName("dir/a.NII.gz", 3); got != "dir/a-3.NII.gz" {
		t.Fatalf("splitName %q", got)
	}
}
