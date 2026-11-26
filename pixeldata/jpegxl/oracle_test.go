package jpegxl

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
)

// The libjxl oracle tests run only with DICOMGO_JXL_ORACLE=1 and the cjxl,
// djxl and cjpeg tools on PATH. They exchange synthetic images with the
// reference implementation in both directions.

func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

func requireOracle(t *testing.T, tools ...string) map[string]string {
	t.Helper()
	if os.Getenv("DICOMGO_JXL_ORACLE") != "1" {
		t.Skip("set DICOMGO_JXL_ORACLE=1 to run the libjxl oracle")
	}
	paths := map[string]string{}
	for _, tool := range tools {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Fatalf("%s not found on PATH", tool)
		}
		paths[tool] = p
	}
	return paths
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", filepath.Base(name), args, err, out)
	}
}

func writePNM(path string, img *Image, maxval int) error {
	magic := "P5"
	if img.Channels == 3 {
		magic = "P6"
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s\n%d %d\n%d\n", magic, img.Width, img.Height, maxval)
	for _, v := range img.Samples {
		if maxval > 255 {
			b.WriteByte(byte(v >> 8))
		}
		b.WriteByte(byte(v))
	}
	return os.WriteFile(path, b.Bytes(), 0o600)
}

func readPNMFile(path string) (*Image, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	var fields []string
	pos := 0
	for len(fields) < 4 {
		for pos < len(data) && (data[pos] == ' ' || data[pos] == '\n' || data[pos] == '\r' || data[pos] == '\t') {
			pos++
		}
		start := pos
		for pos < len(data) && !(data[pos] == ' ' || data[pos] == '\n' || data[pos] == '\r' || data[pos] == '\t') {
			pos++
		}
		if start == pos {
			return nil, 0, fmt.Errorf("bad PNM header")
		}
		fields = append(fields, string(data[start:pos]))
	}
	pos++
	w, _ := strconv.Atoi(fields[1])
	h, _ := strconv.Atoi(fields[2])
	maxval, _ := strconv.Atoi(fields[3])
	ch := 1
	if fields[0] == "P6" {
		ch = 3
	}
	img := &Image{Width: w, Height: h, Channels: ch, Samples: make([]int32, w*h*ch)}
	for i := range img.Samples {
		if maxval > 255 {
			img.Samples[i] = int32(data[pos+2*i])<<8 | int32(data[pos+2*i+1])
		} else {
			img.Samples[i] = int32(data[pos+i])
		}
	}
	return img, maxval, nil
}

func TestLibjxlOracleDecodesEncodeLossless(t *testing.T) {
	tools := requireOracle(t, "djxl")
	dir := t.TempDir()
	for _, tc := range []struct{ w, h, c, bits int }{
		{64, 48, 1, 8}, {33, 21, 3, 8}, {40, 30, 1, 12}, {50, 20, 1, 16}, {1100, 9, 1, 10}, {9, 1030, 3, 8}, {20, 20, 1, 5},
	} {
		img := syntheticImage(tc.w, tc.h, tc.c, tc.bits, 7)
		data, err := EncodeLossless(img, LosslessOptions{BitsPerSample: tc.bits})
		if err != nil {
			t.Fatal(err)
		}
		in := filepath.Join(dir, "in.jxl")
		out := filepath.Join(dir, "out.pnm")
		if err := os.WriteFile(in, data, 0o600); err != nil {
			t.Fatal(err)
		}
		run(t, tools["djxl"], in, out)
		got, maxval, err := readPNMFile(out)
		if err != nil {
			t.Fatal(err)
		}
		if maxval != 1<<uint(tc.bits)-1 {
			t.Fatalf("%+v: djxl maxval %d", tc, maxval)
		}
		for i := range img.Samples {
			if got.Samples[i] != img.Samples[i] {
				t.Fatalf("%+v: djxl sample %d = %d, want %d", tc, i, got.Samples[i], img.Samples[i])
			}
		}
	}
}

// oracleJPEGs writes synthetic JPEG variants with cjpeg and returns their
// paths keyed by name.
func oracleJPEGs(t *testing.T, cjpeg, dir string) map[string]string {
	t.Helper()
	grey := syntheticImage(300, 270, 1, 8, 11)
	colour := syntheticImage(300, 270, 3, 8, 12)
	small := syntheticImage(33, 17, 3, 8, 13)
	wide := syntheticImage(2600, 24, 1, 8, 14)
	sources := map[string]*Image{"grey": grey, "colour": colour, "small": small, "wide": wide}
	for name, img := range sources {
		if err := writePNM(filepath.Join(dir, name+".pnm"), img, 255); err != nil {
			t.Fatal(err)
		}
	}
	variants := []struct {
		name, src string
		args      []string
	}{
		{"grey", "grey", []string{"-grayscale"}},
		{"c420", "colour", nil},
		{"c444", "colour", []string{"-sample", "1x1"}},
		{"c422", "colour", []string{"-sample", "2x1"}},
		{"c440", "colour", []string{"-sample", "1x2"}},
		{"odd", "small", nil},
		{"optimized", "colour", []string{"-optimize"}},
		{"restart", "colour", []string{"-restart", "3"}},
		{"q10", "colour", []string{"-quality", "10"}},
		{"q98", "small", []string{"-quality", "98"}},
		{"rgb", "small", []string{"-rgb"}},
		{"wide", "wide", []string{"-grayscale"}},
	}
	if _, err := os.Stat("/System/Library/ColorSync/Profiles/Generic Gray Profile.icc"); err == nil {
		variants = append(variants, struct {
			name, src string
			args      []string
		}{"icc", "small", []string{"-icc", "/System/Library/ColorSync/Profiles/Generic RGB Profile.icc"}})
	}
	out := map[string]string{}
	for _, v := range variants {
		path := filepath.Join(dir, v.name+".jpg")
		args := append(append([]string{}, v.args...), "-outfile", path, filepath.Join(dir, v.src+".pnm"))
		run(t, cjpeg, args...)
		out[v.name] = path
	}
	return out
}

func TestLibjxlOracleJPEGRecompression(t *testing.T) {
	tools := requireOracle(t, "cjxl", "djxl", "cjpeg", "djpeg")
	dir := t.TempDir()
	jpegs := oracleJPEGs(t, tools["cjpeg"], dir)
	names := make([]string, 0, len(jpegs))
	for name := range jpegs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := jpegs[name]
		original, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// libjxl recompression, restored by this package.
		ref := filepath.Join(dir, name+".ref.jxl")
		run(t, tools["cjxl"], "--lossless_jpeg=1", path, ref)
		refData, _ := os.ReadFile(ref)
		restored, err := ReconstructJPEG(refData)
		if err != nil || !bytes.Equal(restored, original) {
			t.Fatalf("%s: ReconstructJPEG of cjxl output: %v", name, err)
		}
		// This package's recompression, restored and rendered by libjxl.
		mine, err := RecompressJPEG(original)
		if err != nil {
			t.Fatalf("%s: RecompressJPEG: %v", name, err)
		}
		minePath := filepath.Join(dir, name+".mine.jxl")
		if err := os.WriteFile(minePath, mine, 0o600); err != nil {
			t.Fatal(err)
		}
		back := filepath.Join(dir, name+".back.jpg")
		if msg, err := exec.Command(tools["djxl"], minePath, back).CombinedOutput(); err != nil {
			t.Errorf("%s: djxl failed on RecompressJPEG output: %v %s", name, err, msg)
			continue
		}
		backData, _ := os.ReadFile(back)
		if !bytes.Equal(backData, original) {
			n := 0
			for n < len(backData) && n < len(original) && backData[n] == original[n] {
				n++
			}
			t.Errorf("%s: djxl did not restore the original JPEG from RecompressJPEG output: first difference at %d of %d/%d", name, n, len(backData), len(original))
			continue
		}
		pixMine := filepath.Join(dir, name+".mine.pnm")
		pixRef := filepath.Join(dir, name+".ref.pnm")
		run(t, tools["djxl"], minePath, pixMine)
		run(t, tools["djxl"], ref, pixRef)
		a, _, errA := readPNMFile(pixMine)
		b, _, errB := readPNMFile(pixRef)
		if errA != nil || errB != nil || len(a.Samples) != len(b.Samples) {
			t.Fatalf("%s: unreadable djxl output", name)
		}
		maxDiff := int32(0)
		for i := range a.Samples {
			d := a.Samples[i] - b.Samples[i]
			if d < 0 {
				d = -d
			}
			maxDiff = max(maxDiff, d)
		}
		// cjxl stores 4:4:4 colour with chroma-from-luma, whose floating-
		// point rendering drifts from the JPEG decode; RecompressJPEG stores
		// plain coefficients. Accept a different render only when it is at
		// least as close to libjpeg's decode of the original as cjxl's.
		if maxDiff > 1 {
			pj := filepath.Join(dir, name+".djpeg.pnm")
			run(t, tools["djpeg"], "-outfile", pj, path)
			c, _, err := readPNMFile(pj)
			if err != nil || len(c.Samples) != len(a.Samples) {
				t.Fatalf("%s: unreadable djpeg output", name)
			}
			var dMine, dRef int32
			for i := range c.Samples {
				dMine = max(dMine, abs32(a.Samples[i]-c.Samples[i]))
				dRef = max(dRef, abs32(b.Samples[i]-c.Samples[i]))
			}
			if dMine > dRef {
				t.Errorf("%s: render is %d levels from libjpeg, cjxl's is %d", name, dMine, dRef)
				continue
			}
		}
		t.Logf("%s: JPEG %d bytes, cjxl %d, RecompressJPEG %d", name, len(original), len(refData), len(mine))
	}
}

func TestLibjxlOracleLosslessFilesDecodeExactly(t *testing.T) {
	tools := requireOracle(t, "cjxl")
	dir := t.TempDir()
	for _, tc := range []struct{ w, h, c, bits int }{
		{97, 61, 1, 8}, {300, 270, 1, 12}, {70, 50, 1, 16}, {80, 45, 3, 8}, {600, 300, 1, 8},
	} {
		img := syntheticImage(tc.w, tc.h, tc.c, tc.bits, 3)
		src := filepath.Join(dir, "src.pnm")
		if err := writePNM(src, img, 1<<uint(tc.bits)-1); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"-e", "1"}, {"-e", "3"}, {"-e", "7"}, {"-e", "9"}, {"-e", "7", "-R", "1"}} {
			out := filepath.Join(dir, "out.jxl")
			run(t, tools["cjxl"], append(append([]string{"-d", "0"}, args...), src, out)...)
			data, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Decode(data)
			if err != nil {
				t.Fatalf("%+v %v: %v", tc, args, err)
			}
			for i := range img.Samples {
				if got.Samples[i] != img.Samples[i] {
					t.Fatalf("%+v %v: sample %d = %d, want %d", tc, args, i, got.Samples[i], img.Samples[i])
				}
			}
		}
	}
}
