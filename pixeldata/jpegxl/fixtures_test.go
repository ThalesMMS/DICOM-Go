package jpegxl

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Committed fixtures in testdata/ were made by libjxl 0.12.0 cjxl and
// libjpeg-turbo 3.2.0 cjpeg from the deterministic synthetic images below
// (no patient data). TestGenerateFixtures regenerates them with
// DICOMGO_JXL_GENERATE=1.

type jpegFixture struct {
	name string
	img  *Image
	args []string // cjpeg arguments
}

func jpegFixtures() []jpegFixture {
	grey := syntheticImage(48, 40, 1, 8, 21)
	colour := syntheticImage(40, 32, 3, 8, 22)
	odd := syntheticImage(37, 23, 3, 8, 23)
	return []jpegFixture{
		{"grey", grey, []string{"-grayscale", "-quality", "85"}},
		{"c420-restart", colour, []string{"-restart", "1"}},
		{"c444-optimized", colour, []string{"-sample", "1x1", "-optimize"}},
		{"c422-odd", odd, []string{"-sample", "2x1", "-quality", "60"}},
		{"progressive", odd, []string{"-progressive"}},
		{"rgb", colour, []string{"-rgb", "-quality", "95"}},
	}
}

type losslessFixture struct {
	name string
	img  *Image
	args []string // cjxl arguments after -d 0
}

func losslessFixtures() []losslessFixture {
	palette := syntheticImage(41, 29, 3, 8, 31)
	for i := range palette.Samples {
		palette.Samples[i] &= 0xC0
	}
	return []losslessFixture{
		{"grey8-e7", syntheticImage(45, 33, 1, 8, 32), []string{"-e", "7"}},
		{"grey12-e3", syntheticImage(40, 30, 1, 12, 33), []string{"-e", "3"}},
		{"grey16-e9", syntheticImage(30, 20, 1, 16, 34), []string{"-e", "9"}},
		{"rgb8-e7", syntheticImage(33, 27, 3, 8, 35), []string{"-e", "7"}},
		{"grey10-squeeze", syntheticImage(36, 28, 1, 10, 36), []string{"-e", "7", "-R", "1"}},
		{"rgb-palette", palette, []string{"-e", "9"}},
	}
}

func TestFixturesReconstructOriginalJPEGs(t *testing.T) {
	for _, f := range jpegFixtures() {
		jpg, err := os.ReadFile(filepath.Join("testdata", f.name+".jpg"))
		if err != nil {
			t.Fatal(err)
		}
		jxl, err := os.ReadFile(filepath.Join("testdata", f.name+".jxl"))
		if err != nil {
			t.Fatal(err)
		}
		got, err := ReconstructJPEG(jxl)
		if err != nil {
			t.Fatalf("%s: %v", f.name, err)
		}
		if !bytes.Equal(got, jpg) {
			t.Fatalf("%s: reconstructed JPEG differs from the cjpeg original", f.name)
		}
		mine, err := RecompressJPEG(jpg)
		if f.name == "progressive" {
			if !errors.Is(err, ErrNotReversible) {
				t.Fatalf("%s: RecompressJPEG err = %v, want ErrNotReversible", f.name, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: RecompressJPEG: %v", f.name, err)
		}
		if back, err := ReconstructJPEG(mine); err != nil || !bytes.Equal(back, jpg) {
			t.Fatalf("%s: RecompressJPEG output does not restore the JPEG: %v", f.name, err)
		}
	}
}

func TestFixturesDecodeLosslessExactly(t *testing.T) {
	for _, f := range losslessFixtures() {
		data, err := os.ReadFile(filepath.Join("testdata", f.name+".jxl"))
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(data)
		if err != nil {
			t.Fatalf("%s: %v", f.name, err)
		}
		if got.Width != f.img.Width || got.Height != f.img.Height || got.Channels != f.img.Channels || got.BitsPerSample != f.img.BitsPerSample {
			t.Fatalf("%s: %dx%dx%d %d bits", f.name, got.Width, got.Height, got.Channels, got.BitsPerSample)
		}
		for i, want := range f.img.Samples {
			if got.Samples[i] != want {
				t.Fatalf("%s: sample %d = %d, want %d", f.name, i, got.Samples[i], want)
			}
		}
	}
}

func TestGenerateFixtures(t *testing.T) {
	if os.Getenv("DICOMGO_JXL_GENERATE") != "1" {
		t.Skip("set DICOMGO_JXL_GENERATE=1 (with cjxl and cjpeg on PATH) to regenerate testdata")
	}
	t.Setenv("DICOMGO_JXL_ORACLE", "1")
	tools := requireOracle(t, "cjxl", "cjpeg")
	dir := t.TempDir()
	for _, f := range jpegFixtures() {
		src := filepath.Join(dir, f.name+".pnm")
		if err := writePNM(src, f.img, 255); err != nil {
			t.Fatal(err)
		}
		jpg := filepath.Join("testdata", f.name+".jpg")
		run(t, tools["cjpeg"], append(append([]string{}, f.args...), "-outfile", jpg, src)...)
		run(t, tools["cjxl"], "--lossless_jpeg=1", jpg, filepath.Join("testdata", f.name+".jxl"))
	}
	for _, f := range losslessFixtures() {
		src := filepath.Join(dir, f.name+".pnm")
		if err := writePNM(src, f.img, 1<<f.img.BitsPerSample-1); err != nil {
			t.Fatal(err)
		}
		run(t, tools["cjxl"], append(append([]string{"-d", "0"}, f.args...), src, filepath.Join("testdata", f.name+".jxl"))...)
	}
}
