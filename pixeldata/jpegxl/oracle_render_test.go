package jpegxl

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// djxl renders EncodeLossy output and cjxl JPEG recompressions like Decode.
func TestLibjxlOracleRendersJPEGCompatibleFrames(t *testing.T) {
	tools := requireOracle(t, "cjxl", "djxl", "cjpeg", "djpeg")
	dir := t.TempDir()
	files := map[string][]byte{}
	sources := map[string]string{}
	for _, tc := range []struct{ w, h, c, quality int }{
		{64, 48, 1, 90}, {257, 129, 3, 95}, {33, 21, 3, 50}, {600, 300, 1, 75}, {300, 270, 3, 85},
	} {
		img := syntheticImage(tc.w, tc.h, tc.c, 8, int64(tc.w))
		data, err := EncodeLossy(img, LossyOptions{Quality: tc.quality})
		if err != nil {
			t.Fatal(err)
		}
		files[fmt.Sprintf("lossy-%dx%dx%d-q%d", tc.w, tc.h, tc.c, tc.quality)] = data
	}
	for name, path := range oracleJPEGs(t, tools["cjpeg"], dir) {
		ref := filepath.Join(dir, name+".ref.jxl")
		run(t, tools["cjxl"], "--lossless_jpeg=1", path, ref)
		data, err := os.ReadFile(ref)
		if err != nil {
			t.Fatal(err)
		}
		files["cjxl-"+name] = data
		sources["cjxl-"+name] = path
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		data := files[name]
		in := filepath.Join(dir, "render.jxl")
		out := filepath.Join(dir, "render.pnm")
		if err := os.WriteFile(in, data, 0o600); err != nil {
			t.Fatal(err)
		}
		run(t, tools["djxl"], in, out)
		ref, _, err := readPNMFile(out)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got.Samples) != len(ref.Samples) {
			t.Fatalf("%s: %d samples, djxl %d", name, len(got.Samples), len(ref.Samples))
		}
		d, p := maxDiff(got.Samples, ref.Samples), psnr(got.Samples, ref.Samples, 255)
		t.Logf("%s: max difference %d, PSNR %.1f dB", name, d, p)
		if d <= 4 || p >= 45 {
			continue
		}
		// libjxl renders 4:4:4 chroma-from-luma in floating point and drifts
		// from the JPEG decode; Decode renders the JPEG coefficients. Accept
		// the difference when Decode is at least as close to libjpeg.
		src, ok := sources[name]
		if !ok {
			t.Errorf("%s: render differs from djxl by %d levels (PSNR %.1f dB)", name, d, p)
			continue
		}
		pj := filepath.Join(dir, "render.djpeg.pnm")
		run(t, tools["djpeg"], "-outfile", pj, src)
		jp, _, err := readPNMFile(pj)
		if err != nil || len(jp.Samples) != len(got.Samples) {
			t.Fatalf("%s: unreadable djpeg output", name)
		}
		if mine, theirs := psnr(got.Samples, jp.Samples, 255), psnr(ref.Samples, jp.Samples, 255); mine < theirs {
			t.Errorf("%s: render is %.1f dB from libjpeg, djxl's %.1f dB", name, mine, theirs)
		}
	}
}
