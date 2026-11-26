package qualification

import (
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// squareFrame is a w×h RGBA8 frame with a bright square [x0, x0+size)².
func squareFrame(w, h, x0, size int) []byte {
	pix := make([]byte, 4*w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := 4 * (y*w + x)
			pix[i+3] = 255
			if x >= x0 && x < x0+size && y >= 10 && y < 10+size {
				pix[i], pix[i+1], pix[i+2] = 220, 180, 90
			}
		}
	}
	return pix
}

// TestToleranceRadiusAbsorbsOnePixelEdgeShift is the acceptance case: an
// edge moved by one pixel passes with radius 1 and fails without it; a
// two-pixel move still fails with radius 1.
func TestToleranceRadiusAbsorbsOnePixelEdgeShift(t *testing.T) {
	const w, h = 48, 40
	baseline := squareFrame(w, h, 12, 20)
	shifted := squareFrame(w, h, 13, 20)
	for _, tc := range []struct {
		name      string
		test      []byte
		radius    int
		different uint64
	}{
		{"exact", baseline, 0, 0},
		{"1 px, radius 0", shifted, 0, 40}, // one column gained, one lost
		{"1 px, radius 1", shifted, 1, 0},
		// The leading and trailing columns still differ, less the four
		// corner pixels that find a matching diagonal neighbour.
		{"2 px, radius 1", squareFrame(w, h, 14, 20), 1, 38},
		{"2 px, radius 2", squareFrame(w, h, 14, 20), 2, 0},
	} {
		tolerance := RGBAToleranceV1{ToleranceRadius: tc.radius}
		result, err := CompareRGBA8WithTolerance(tc.test, baseline, w, h, tolerance)
		if err != nil {
			t.Fatal(err)
		}
		if result.DifferentPixels != tc.different || result.Pass(tolerance) != (tc.different == 0) {
			t.Errorf("%s: %d different pixels, want %d", tc.name, result.DifferentPixels, tc.different)
		}
		if tc.different > 0 && (result.MaximumDifference != 220 || result.MeanDifference != 220) {
			t.Errorf("%s: difference max %d mean %g", tc.name, result.MaximumDifference, result.MeanDifference)
		}
	}
}

func TestToleranceThresholdAndBoundary(t *testing.T) {
	const w, h = 16, 12
	baseline := squareFrame(w, h, 4, 6)
	noisy := append([]byte(nil), baseline...)
	for i := 0; i < len(noisy); i += 4 {
		noisy[i] = byte(min(255, int(noisy[i])+3))
	}
	if result, _ := CompareRGBA8WithTolerance(noisy, baseline, w, h, RGBAToleranceV1{}); result.DifferentPixels != w*h || result.MaximumDifference != 3 {
		t.Fatalf("noise without threshold: %d different, max %d", result.DifferentPixels, result.MaximumDifference)
	}
	threshold := RGBAToleranceV1{DifferenceThreshold: [4]uint8{3, 0, 0, 0}}
	if result, _ := CompareRGBA8WithTolerance(noisy, baseline, w, h, threshold); result.DifferentPixels != 0 {
		t.Fatalf("noise within the threshold: %d different", result.DifferentPixels)
	}
	// A difference only on the border is skipped with IgnoreBoundaryPixels.
	edged := append([]byte(nil), baseline...)
	edged[0] = 99
	ignore := RGBAToleranceV1{ToleranceRadius: 1, IgnoreBoundaryPixels: true}
	result, _ := CompareRGBA8WithTolerance(edged, baseline, w, h, ignore)
	if result.ComparedPixels != (w-2)*(h-2) || result.DifferentPixels != 0 {
		t.Fatalf("boundary ignored: compared %d, different %d", result.ComparedPixels, result.DifferentPixels)
	}
	result, _ = CompareRGBA8WithTolerance(edged, baseline, w, h, RGBAToleranceV1{ToleranceRadius: 1})
	if result.ComparedPixels != w*h || result.DifferentPixels != 1 {
		t.Fatalf("boundary searched: compared %d, different %d", result.ComparedPixels, result.DifferentPixels)
	}
	// Tolerated differences can be allowed by count.
	if !result.Pass(RGBAToleranceV1{MaximumDifferentPixels: 1}) {
		t.Fatal("one different pixel refused under a limit of one")
	}
	for _, bad := range []RGBAToleranceV1{{ToleranceRadius: -1}, {ToleranceRadius: MaxToleranceRadius + 1}} {
		if _, err := CompareRGBA8WithTolerance(baseline, baseline, w, h, bad); !errors.Is(err, ErrInvalidFrameMetrics) {
			t.Errorf("radius %d: %v", bad.ToleranceRadius, err)
		}
	}
	if _, err := CompareRGBA8WithTolerance(baseline[:8], baseline, w, h, RGBAToleranceV1{}); !errors.Is(err, ErrInvalidFrameMetrics) {
		t.Errorf("short frame: %v", err)
	}
}

// TestAlternativeBaselinesAndFailureImages accepts a rendering matching any
// baseline of a case, and writes test, baseline and difference PNGs for a
// failure.
func TestAlternativeBaselinesAndFailureImages(t *testing.T) {
	const w, h = 48, 40
	first, second := squareFrame(w, h, 12, 20), squareFrame(w, h, 20, 20)
	test := squareFrame(w, h, 21, 20)
	tolerance := RGBAToleranceV1{ToleranceRadius: 1}
	result, index, err := CompareRGBA8WithBaselines(test, [][]byte{first, second}, w, h, tolerance)
	if err != nil || index != 1 || !result.Pass(tolerance) {
		t.Fatalf("best baseline %d (%d different): %v", index, result.DifferentPixels, err)
	}
	result, index, _ = CompareRGBA8WithBaselines(squareFrame(w, h, 30, 12), [][]byte{first, second}, w, h, tolerance)
	if result.Pass(tolerance) || index != 1 {
		t.Fatalf("unmatched test passed against baseline %d", index)
	}
	directory := t.TempDir()
	paths, err := WriteComparisonPNGs(directory, "case-a", squareFrame(w, h, 30, 12), second, result)
	if err != nil || len(paths) != 3 {
		t.Fatalf("%v %v", paths, err)
	}
	for i, suffix := range []string{"test", "baseline", "diff"} {
		if paths[i] != filepath.Join(directory, "case-a."+suffix+".png") {
			t.Fatalf("path %s", paths[i])
		}
		file, err := os.Open(paths[i])
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(file)
		file.Close()
		if err != nil || img.Bounds().Dx() != w || img.Bounds().Dy() != h {
			t.Fatalf("%s: %v %v", suffix, img.Bounds(), err)
		}
		if suffix == "diff" {
			if r, _, _, _ := img.At(25, 15).RGBA(); r>>8 != 255 {
				t.Fatalf("largest difference drawn as %d, want white", r>>8)
			}
			if r, _, _, _ := img.At(1, 1).RGBA(); r != 0 {
				t.Fatalf("matching pixel drawn as %d, want black", r>>8)
			}
		}
	}
	if _, _, err := CompareRGBA8WithBaselines(first, nil, w, h, tolerance); !errors.Is(err, ErrInvalidFrameMetrics) {
		t.Fatalf("no baselines: %v", err)
	}
}
