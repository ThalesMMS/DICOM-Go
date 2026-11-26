package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/imagebuild"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/multiframe"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// ctSeries builds n synthetic 4x5 CT slices whose values run from -500 by
// 37 per pixel and 101 per slice, with an optional window.
func ctSeries(t *testing.T, n int, window *imagebuild.Window) []*object.File {
	t.Helper()
	geometry := imagebuild.Geometry{Rows: 4, Columns: 5, PixelSpacing: [2]float64{0.8, 0.8},
		RowDirection: [3]float64{1, 0, 0}, ColumnDirection: [3]float64{0, 1, 0}, SliceThickness: 2, FrameOfReferenceUID: "1.2.826.0.1.3680043.10.1.1193.9"}
	slices := make([]imagebuild.Slice, n)
	for z := range slices {
		slices[z].Position = [3]float64{-10, -20, 30 + 2*float64(z)}
		for p := 0; p < 20; p++ {
			slices[z].Values = append(slices[z].Values, float64(-500+p*37+z*101))
		}
	}
	files, err := imagebuild.Build(geometry, slices, imagebuild.Options{IOD: imagebuild.CT, RescaleType: "HU", Window: window,
		SeriesInstanceUID: "1.2.826.0.1.3680043.10.1.1193.2", ContentTime: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func writeFile(t *testing.T, dir, name string, file *object.File) string {
	t.Helper()
	path := filepath.Join(dir, name)
	var buf bytes.Buffer
	if err := object.WriteFile(&buf, file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runTool(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func readPNG(t *testing.T, path string) image.Image {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func grayValues(t *testing.T, img image.Image) []uint8 {
	t.Helper()
	gray, ok := img.(*image.Gray)
	if !ok {
		t.Fatalf("image is %T, want *image.Gray", img)
	}
	return gray.Pix
}

// TestWindowSelection: the file's window, an explicit -window and the
// min-max window each map the modality values as stated.
func TestWindowSelection(t *testing.T) {
	dir := t.TempDir()
	in := writeFile(t, dir, "ct.dcm", ctSeries(t, 1, &imagebuild.Window{Center: 0, Width: 1})[0])
	out := filepath.Join(dir, "out.png")

	// The file's window is a threshold at -0.5.
	if code, stdout, stderr := runTool(in, out); code != 0 || !strings.Contains(stdout, "1 frame(s) written") {
		t.Fatalf("code %d stdout %q stderr %q", code, stdout, stderr)
	}
	for p, v := range grayValues(t, readPNG(t, out)) {
		want := uint8(0)
		if -500+p*37 >= 0 {
			want = 255
		}
		if v != want {
			t.Fatalf("file window: pixel %d = %d, want %d", p, v, want)
		}
	}
	// -window overrides it: centre 203, width 1 thresholds at 202.5.
	if code, _, stderr := runTool("-window", "203,1", in, out); code != 0 {
		t.Fatal(stderr)
	}
	for p, v := range grayValues(t, readPNG(t, out)) {
		if want := -500+p*37 >= 203; (v == 255) != want {
			t.Fatalf("-window: pixel %d = %d", p, v)
		}
	}
	// -min-max spans -500..203: first pixel black, last white, increasing.
	if code, _, stderr := runTool("-min-max", in, out); code != 0 {
		t.Fatal(stderr)
	}
	values := grayValues(t, readPNG(t, out))
	if values[0] != 0 || values[19] != 255 {
		t.Fatalf("min-max ends %d, %d", values[0], values[19])
	}
	for p := 1; p < len(values); p++ {
		if values[p] < values[p-1] {
			t.Fatalf("min-max not monotonic at %d: %v", p, values)
		}
	}
	// MONOCHROME1 is inverted.
	mono1 := ctSeries(t, 1, nil)[0]
	mono1.Dataset.Put(dicomtest.NewStringElement(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME1"))
	inverted := filepath.Join(dir, "inverted.png")
	if code, _, stderr := runTool(writeFile(t, dir, "mono1.dcm", mono1), inverted); code != 0 {
		t.Fatal(stderr)
	}
	for p, v := range grayValues(t, readPNG(t, inverted)) {
		if v != 255-values[p] {
			t.Fatalf("MONOCHROME1 pixel %d = %d, want %d", p, v, 255-values[p])
		}
	}
}

// TestMultiFrameAndFormats: every frame of a multi-frame image is written
// to a numbered file, -frame selects one, and JPEG is the default.
func TestMultiFrameAndFormats(t *testing.T) {
	dir := t.TempDir()
	enhanced, _, err := multiframe.ConvertToLegacyEnhanced(context.Background(), ctSeries(t, 3, nil), multiframe.LegacyOptions{
		SOPInstanceUID: "1.2.826.0.1.3680043.10.1.1193.50", SeriesInstanceUID: "1.2.826.0.1.3680043.10.1.1193.51",
		Equipment: multiframe.ContributingEquipment{Manufacturer: "dicom-go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	in := writeFile(t, dir, "enhanced.dcm", enhanced)
	if code, stdout, stderr := runTool(in, filepath.Join(dir, "frame.jpg")); code != 0 || !strings.Contains(stdout, "3 frame(s)") {
		t.Fatalf("code %d %q %q", code, stdout, stderr)
	}
	for i := 1; i <= 3; i++ {
		data, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("frame-%04d.jpg", i)))
		if err != nil || !bytes.HasPrefix(data, []byte{0xFF, 0xD8}) {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
	if code, _, stderr := runTool("-frame", "2", "-format", "png", in, filepath.Join(dir, "two.out")); code != 0 {
		t.Fatal(stderr)
	}
	if img := readPNG(t, filepath.Join(dir, "two.out")); img.Bounds().Dx() != 5 || img.Bounds().Dy() != 4 {
		t.Fatalf("bounds %v", img.Bounds())
	}
	if code, _, _ := runTool("-frame", "4", in, filepath.Join(dir, "x.png")); code != 1 {
		t.Fatalf("frame beyond the image: code %d", code)
	}
}

// TestColor: RGB samples are written as they are.
func TestColor(t *testing.T) {
	dir := t.TempDir()
	pixels := []byte{255, 0, 0, 0, 255, 0, 0, 0, 255, 10, 20, 30}
	data, err := dicomtest.Part10File(transfer.ExplicitVRLittleEndian,
		dicomtest.NewUIElement(core.NewTag(0x0008, 0x0016), "1.2.840.10008.5.1.4.1.1.7"),
		dicomtest.NewUIElement(core.NewTag(0x0008, 0x0018), "1.2.826.0.1.3680043.10.1.1193.70"),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0002), 3),
		dicomtest.NewStringElement(core.NewTag(0x0028, 0x0004), core.VRCS, "RGB"),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0006), 0),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0010), 2),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0011), 2),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0100), 8),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0101), 8),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0102), 7),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0103), 0),
		dicomtest.NewOBElement(core.TagPixelData, pixels),
	)
	if err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(dir, "rgb.dcm")
	if err := os.WriteFile(in, data, 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "rgb.png")
	if code, _, stderr := runTool(in, out); code != 0 {
		t.Fatal(stderr)
	}
	img := readPNG(t, out)
	for i := 0; i < 4; i++ {
		r, g, b, _ := img.At(i%2, i/2).RGBA()
		if got := []byte{byte(r >> 8), byte(g >> 8), byte(b >> 8)}; !bytes.Equal(got, pixels[3*i:3*i+3]) {
			t.Fatalf("pixel %d = %v, want %v", i, got, pixels[3*i:3*i+3])
		}
	}
}

func TestErrors(t *testing.T) {
	dir := t.TempDir()
	in := writeFile(t, dir, "ct.dcm", ctSeries(t, 1, nil)[0])
	noPixels := ctSeries(t, 1, nil)[0]
	noPixels.Dataset.Remove(core.TagPixelData)
	for _, args := range [][]string{{in}, {"-q", "0", in, "x.jpg"}, {"-format", "gif", in, "x.gif"}, {"-window", "1", in, "x.jpg"}, {"-window", "1,1", "-min-max", in, "x.jpg"}} {
		if code, _, _ := runTool(args...); code != 2 {
			t.Fatalf("%v: code %d, want 2", args, code)
		}
	}
	if code, _, stderr := runTool(writeFile(t, dir, "nopixels.dcm", noPixels), filepath.Join(dir, "x.jpg")); code != 1 || !strings.Contains(stderr, "dcm2jpg: codec:") {
		t.Fatalf("no Pixel Data: code %d %q", code, stderr)
	}
	if code, _, _ := runTool(in, in); code != 1 {
		t.Fatal("the input was accepted as output")
	}
	if code, _, stderr := runTool("-h"); code != 0 || !strings.Contains(stderr, "Usage: dcm2jpg") {
		t.Fatalf("-h: %d %q", code, stderr)
	}
}

// TestDCMTKMinMaxWindow (opt-in) compares the min-max rendering with DCMTK
// dcm2pnm +Wm, within one grey level.
func TestDCMTKMinMaxWindow(t *testing.T) {
	tool := dcmtkoracle.Tool(t, "dcm2pnm")
	t.Logf("tolerance: 1 grey level")
	dir := t.TempDir()
	in := writeFile(t, dir, "ct.dcm", ctSeries(t, 1, nil)[0])
	ours := filepath.Join(dir, "ours.png")
	if code, _, stderr := runTool("-min-max", in, ours); code != 0 {
		t.Fatal(stderr)
	}
	theirs := filepath.Join(dir, "theirs.png")
	if output, err := exec.Command(tool, "+Wm", "+on", in, theirs).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	a, b := readPNG(t, ours), readPNG(t, theirs)
	for y := 0; y < 4; y++ {
		for x := 0; x < 5; x++ {
			ga, _, _, _ := a.At(x, y).RGBA()
			gb, _, _, _ := b.At(x, y).RGBA()
			if d := int(ga>>8) - int(gb>>8); d < -1 || d > 1 {
				t.Fatalf("(%d,%d): %d vs DCMTK %d", x, y, ga>>8, gb>>8)
			}
		}
	}
}
