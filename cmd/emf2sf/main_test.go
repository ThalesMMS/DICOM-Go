package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/imagebuild"
	"github.com/ThalesMMS/dicom-go/internal/clifile"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/validation/iod"
)

// writeSeries writes n synthetic 4x5 CT slices 2 mm apart and returns
// their paths.
func writeSeries(t *testing.T, dir string, n int) []string {
	t.Helper()
	geometry := imagebuild.Geometry{Rows: 4, Columns: 5, PixelSpacing: [2]float64{0.8, 0.8},
		RowDirection: [3]float64{1, 0, 0}, ColumnDirection: [3]float64{0, 1, 0}, SliceThickness: 2}
	slices := make([]imagebuild.Slice, n)
	for z := range slices {
		slices[z].Position = [3]float64{-10, -20, 30 + 2*float64(z)}
		for p := 0; p < 20; p++ {
			slices[z].Values = append(slices[z].Values, float64(-500+p*37+z*101))
		}
	}
	files, err := imagebuild.Build(geometry, slices, imagebuild.Options{IOD: imagebuild.CT, RescaleType: "HU", ContentTime: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for z, file := range files {
		var buf bytes.Buffer
		if err := object.WriteFile(&buf, file); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "slice"+string(rune('a'+z))+".dcm")
		if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	return paths
}

func runTool(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func readFile(t *testing.T, path string) *object.File {
	t.Helper()
	file, err := clifile.ReadPart10(path)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func pixels(t *testing.T, file *object.File) []byte {
	t.Helper()
	frames, err := pixeldata.ExtractNativeFramesView(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Join(frames.Data, nil)
}

func positionZ(t *testing.T, file *object.File) float64 {
	t.Helper()
	geometry := file.FrameGeometry()
	if len(geometry.ImagePositionPatient) != 3 {
		t.Fatalf("no Image Position (Patient)")
	}
	return geometry.ImagePositionPatient[2]
}

// TestRoundTrip: classic slices become one Legacy Converted Enhanced image
// without IOD errors, and back into classic slices with the same pixels and
// positions.
func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	sources := writeSeries(t, dir, 3)
	enhanced := filepath.Join(dir, "enhanced.dcm")
	code, stdout, stderr := runTool("-reverse", "-o", enhanced, sources[2], sources[0], sources[1])
	if code != 0 || !strings.Contains(stdout, "3 frame(s) written") {
		t.Fatalf("-reverse: code %d %q %q", code, stdout, stderr)
	}
	report, err := iod.Validate(readFile(t, enhanced).Dataset, iod.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range report.Errors() {
		t.Errorf("%s: %s", report.IOD, f)
	}

	outdir := filepath.Join(dir, "classic")
	if err := os.Mkdir(outdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if code, stdout, stderr := runTool("-o", outdir, enhanced); code != 0 || !strings.Contains(stdout, "3 of 3 frame(s)") {
		t.Fatalf("emf2sf: code %d %q %q", code, stdout, stderr)
	}
	sourceSeries := readFile(t, sources[0]).Dataset
	seriesUID, _ := sourceSeries.GetString(core.NewTag(0x0020, 0x000E))
	for z, source := range sources {
		want := readFile(t, source)
		got := readFile(t, filepath.Join(outdir, "enhanced-000"+string(rune('1'+z))+".dcm"))
		if !bytes.Equal(pixels(t, got), pixels(t, want)) {
			t.Fatalf("slice %d: pixels differ", z)
		}
		if positionZ(t, got) != positionZ(t, want) {
			t.Fatalf("slice %d: position %v, want %v", z, positionZ(t, got), positionZ(t, want))
		}
		if uid, _ := got.Dataset.GetString(core.NewTag(0x0020, 0x000E)); uid == seriesUID || !core.IsValidUID(uid) {
			t.Fatalf("slice %d: Series Instance UID %q is not a new series", z, uid)
		}
	}

	// -frames selects frames and keeps their numbers.
	selected := filepath.Join(dir, "selected")
	if err := os.Mkdir(selected, 0o700); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runTool("-frames", "2-3", "-series-uid", "1.2.826.0.1.3680043.10.1.1193.80", "-o", selected, enhanced); code != 0 {
		t.Fatal(stderr)
	}
	entries, _ := os.ReadDir(selected)
	if len(entries) != 2 || entries[0].Name() != "enhanced-0002.dcm" || entries[1].Name() != "enhanced-0003.dcm" {
		t.Fatalf("selected outputs %v", entries)
	}
	if uid, _ := readFile(t, filepath.Join(selected, "enhanced-0002.dcm")).Dataset.GetString(core.NewTag(0x0020, 0x000E)); uid != "1.2.826.0.1.3680043.10.1.1193.80" {
		t.Fatalf("-series-uid not applied: %q", uid)
	}
}

func TestErrors(t *testing.T) {
	dir := t.TempDir()
	sources := writeSeries(t, dir, 2)
	for _, args := range [][]string{{sources[0]}, {"-o", dir, sources[0], sources[1]}, {"-frames", "0", "-o", dir, sources[0]},
		{"-frames", "3-1", "-o", dir, sources[0]}, {"-series-uid", "not-a-uid", "-o", dir, sources[0]}, {"-reverse", "-frames", "1", "-o", "x.dcm", sources[0]}} {
		if code, _, _ := runTool(args...); code != 2 {
			t.Fatalf("%v: code %d, want 2", args, code)
		}
	}
	// A classic image is not an Enhanced one; -o must be a directory.
	if code, _, stderr := runTool("-o", dir, sources[0]); code != 1 || !strings.Contains(stderr, "emf2sf: ") {
		t.Fatalf("classic input: code %d %q", code, stderr)
	}
	if code, _, _ := runTool("-o", sources[1], sources[0]); code != 1 {
		t.Fatalf("file as -o: code %d", code)
	}
	if code, _, _ := runTool("-reverse", "-o", sources[0], sources[0], sources[1]); code != 1 {
		t.Fatal("the input was replaced")
	}
	if code, _, stderr := runTool("-h"); code != 0 || !strings.Contains(stderr, "Usage: emf2sf") {
		t.Fatalf("-h: %d %q", code, stderr)
	}
}
