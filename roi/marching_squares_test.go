package roi

import (
	"errors"
	"math"
	"math/rand"
	"testing"
)

// fillEvenOdd rasterizes contours by pixel centre under the even-odd rule.
func fillEvenOdd(contours []IsoContour, columns, rows int) *RasterMask {
	out := NewRasterMask(columns, rows)
	for _, contour := range contours {
		out.XOR(RasterizePolygon(contour.Points, columns, rows))
	}
	return out
}

func TestMarchingSquaresMaskRoundTripWithHolesAndSaddles(t *testing.T) {
	// A ring (outer boundary and a hole), a lone pixel, pixels touching
	// diagonally (saddles) and a region on the border.
	mask := NewRasterMask(24, 20)
	for y := 2; y < 12; y++ {
		for x := 2; x < 12; x++ {
			if x < 5 || x > 8 || y < 5 || y > 8 {
				mask.Set(x, y, true)
			}
		}
	}
	mask.Set(15, 3, true)
	mask.Set(17, 7, true)
	mask.Set(18, 8, true)
	mask.Set(19, 7, true)
	for y := 14; y < 20; y++ {
		mask.SetRun(y, 18, 24)
	}
	rng := rand.New(rand.NewSource(1075))
	for i := 0; i < 40; i++ {
		mask.Set(rng.Intn(24), 12+rng.Intn(8), true)
	}
	contours, err := mask.SubpixelContours(SubpixelContourOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !sameMask(fillEvenOdd(contours, 24, 20), mask) {
		t.Fatal("even-odd filling of the contours does not give the mask back")
	}
	// Outer boundaries are clockwise on screen, holes counter-clockwise; the
	// contours through the edge midpoints cut the corners of the pixel
	// squares, so the signed area is close to, not equal to, the count.
	total, holes := 0.0, 0
	for _, contour := range contours {
		area := ClosedPolygonArea(contour.Points)
		total += area
		if area < 0 {
			holes++
		}
	}
	if holes != 1 || math.Abs(total-float64(mask.Count())) > 0.1*float64(mask.Count()) {
		t.Fatalf("signed area %.3f for %d pixels, %d holes", total, mask.Count(), holes)
	}
	// A lone pixel is the diamond through its edge midpoints... of area ½.
	lone := NewRasterMask(3, 3)
	lone.Set(1, 1, true)
	single, err := lone.SubpixelContours(SubpixelContourOptions{})
	if err != nil || len(single) != 1 || len(single[0].Points) != 4 || ClosedPolygonArea(single[0].Points) != 0.5 {
		t.Fatalf("lone pixel: %+v %v", single, err)
	}
}

func TestMarchingSquaresInterpolatesACircleSubpixel(t *testing.T) {
	// A radial field: the iso-line at r = 7.3 px is a circle.
	const cx, cy, r = 15.4, 14.2, 7.3
	field := func(x, y int) float64 { return -math.Hypot(float64(x)-cx, float64(y)-cy) }
	contours, err := IsoContours(32, 30, field, -r)
	if err != nil || len(contours) != 1 {
		t.Fatalf("%d contours: %v", len(contours), err)
	}
	worst := 0.0
	for _, p := range contours[0].Points {
		worst = math.Max(worst, math.Abs(math.Hypot(p.X-cx, p.Y-cy)-r))
	}
	// Linear interpolation of a distance field is exact along the grid
	// lines up to the curvature term, well below a tenth of a pixel.
	if worst > 0.05 {
		t.Fatalf("points up to %.3f px off the circle", worst)
	}
	if area := ClosedPolygonArea(contours[0].Points); math.Abs(area-math.Pi*r*r)/(math.Pi*r*r) > 0.01 {
		t.Fatalf("area %.2f, circle %.2f", area, math.Pi*r*r)
	}
	// NaN and the border count as below: a field above the level everywhere
	// is one contour around the grid edge.
	all, err := IsoContours(4, 3, func(x, y int) float64 {
		if x == 3 && y == 0 {
			return math.NaN()
		}
		return 1
	}, 0)
	if err != nil || len(all) != 1 || math.Abs(ClosedPolygonArea(all[0].Points)-(12-1)) > 0.5 {
		t.Fatalf("full field: %+v %v", all, err)
	}
	if _, err := IsoContours(0, 3, field, 0); !errors.Is(err, ErrIsoContour) {
		t.Fatalf("empty field: %v", err)
	}
}

func TestMarchingSquaresRejectsOversizedGridBeforeAllocation(t *testing.T) {
	// Sparse masks allocate per row. These dimensions need no dense test
	// fixture, and the overflowing product would be negative before make.
	empty := NewRasterMask(maxIsoContourSamples+1, 1)
	if _, err := empty.SubpixelContours(SubpixelContourOptions{}); !errors.Is(err, ErrIsoContour) {
		t.Fatalf("oversized empty mask: %v", err)
	}

	overflowingWidth := int(^uint(0)>>1)/2 + 2
	sparse := NewRasterMask(overflowingWidth, 2)
	sparse.Set(0, 0, true)
	if _, err := sparse.SubpixelContours(SubpixelContourOptions{}); !errors.Is(err, ErrIsoContour) {
		t.Fatalf("overflowing sparse mask: %v", err)
	}
	if _, err := IsoContours(overflowingWidth, 2, func(int, int) float64 {
		t.Fatal("oversized scalar source was sampled")
		return 0
	}, 0); !errors.Is(err, ErrIsoContour) {
		t.Fatalf("overflowing scalar grid: %v", err)
	}
	if _, err := LabelIsoContours(overflowingWidth, 2, func(int, int) int {
		t.Fatal("oversized label source was sampled")
		return 0
	}); !errors.Is(err, ErrIsoContour) {
		t.Fatalf("overflowing label grid: %v", err)
	}
}

func TestMarchingSquaresSmoothsTheStaircase(t *testing.T) {
	// A pixelated disc: smoothing brings the contour closer to the circle.
	const c, r = 20.0, 9.0
	mask := NewRasterMask(41, 41)
	for y := 0; y < 41; y++ {
		for x := 0; x < 41; x++ {
			if math.Hypot(float64(x)-c, float64(y)-c) <= r {
				mask.Set(x, y, true)
			}
		}
	}
	deviation := func(sigma float64) float64 {
		contours, err := mask.SubpixelContours(SubpixelContourOptions{SmoothSigma: sigma})
		if err != nil || len(contours) != 1 {
			t.Fatalf("σ %v: %d contours %v", sigma, len(contours), err)
		}
		// Roundness: the RMS deviation of the radius from its mean.
		points := contours[0].Points
		mean := 0.0
		for _, p := range points {
			mean += math.Hypot(p.X-c, p.Y-c)
		}
		mean /= float64(len(points))
		sum := 0.0
		for _, p := range points {
			d := math.Hypot(p.X-c, p.Y-c) - mean
			sum += d * d
		}
		return math.Sqrt(sum / float64(len(points)))
	}
	raw, smooth := deviation(0), deviation(1.5)
	t.Logf("RMS radial deviation %.3f px raw, %.3f px smoothed", raw, smooth)
	if smooth >= 0.6*raw || smooth > 0.15 {
		t.Fatalf("RMS radial deviation %.3f px raw, %.3f px smoothed", raw, smooth)
	}
	if _, err := mask.SubpixelContours(SubpixelContourOptions{SmoothSigma: -1}); !errors.Is(err, ErrIsoContour) {
		t.Fatalf("negative σ: %v", err)
	}
}

func TestMarchingSquaresLabelFieldContoursEachLabel(t *testing.T) {
	// Label 2 surrounds a hole filled by label 1, and label 3 touches label
	// 2: every label must come back exactly, holes included.
	const w, h = 12, 10
	labels := make([]int, w*h)
	for y := 1; y < 9; y++ {
		for x := 1; x < 8; x++ {
			labels[y*w+x] = 2
		}
	}
	for y := 3; y < 6; y++ {
		for x := 3; x < 6; x++ {
			labels[y*w+x] = 1
		}
	}
	for y := 2; y < 7; y++ {
		labels[y*w+8], labels[y*w+9] = 3, 3
	}
	got, err := LabelIsoContours(w, h, func(x, y int) int { return labels[y*w+x] })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Label != 1 || got[1].Label != 2 || got[2].Label != 3 {
		t.Fatalf("labels = %+v, want 1, 2, 3", got)
	}
	for _, entry := range got {
		want := NewRasterMask(w, h)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				if labels[y*w+x] == entry.Label {
					want.Set(x, y, true)
				}
			}
		}
		if !sameMask(fillEvenOdd(entry.Contours, w, h), want) {
			t.Fatalf("label %d does not round-trip", entry.Label)
		}
	}
	if len(got[1].Contours) != 2 {
		t.Fatalf("label 2 contours = %d, want outer boundary and hole", len(got[1].Contours))
	}
	if _, err := LabelIsoContours(0, 1, func(int, int) int { return 0 }); !errors.Is(err, ErrIsoContour) {
		t.Fatalf("empty field error = %v", err)
	}
	if empty, err := LabelIsoContours(3, 3, func(int, int) int { return 0 }); err != nil || len(empty) != 0 {
		t.Fatalf("all-background field = %+v, %v", empty, err)
	}
}
