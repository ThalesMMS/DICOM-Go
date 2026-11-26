package roi

import (
	"context"
	"image"
	"image/color"
	_ "image/png"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// TestRegionGrowingITKOracleOnCTHead1 is opt-in: set DICOMGO_CTHEAD1_PNG to
// a local copy of ITK's Testing/Data/Input/cthead1.png and, to compare the
// regions, DICOMGO_ITK_REGIONGROWING_BASELINES to a directory holding the
// Testing/Data/Baseline/BasicFilters/*ConnectedImageFilterTest*.png
// baselines (all fetched by ITK's ExternalData; none is part of this
// repository). The seeds and parameters are those of ITK 35b196e1's
// Modules/Segmentation/RegionGrowing/test/CMakeLists.txt and test sources,
// run on an 8-bit image: the confidence bounds are clamped to [0, 255] and
// truncated as ITK casts them to unsigned char, and the isolated bisection
// uses integer midpoints as ITK's integer accumulate type does. Without the
// baselines the region sizes are only logged.
func TestRegionGrowingITKOracleOnCTHead1(t *testing.T) {
	path := os.Getenv("DICOMGO_CTHEAD1_PNG")
	if path == "" {
		t.Skip("set DICOMGO_CTHEAD1_PNG to ITK's cthead1.png (and DICOMGO_ITK_REGIONGROWING_BASELINES to its baselines) to run the ITK region growing oracle")
	}
	input := readITKGrayPNG(t, path)
	volume := RegionGrowVolume{Columns: input.columns, Rows: input.rows, Slices: 1, ValueAt: func(x, y, _ int) (float64, bool) {
		return input.values[y*input.columns+x], true
	}}
	ctx := context.Background()
	uchar := func(lower, upper float64) (float64, float64) {
		return math.Trunc(math.Max(lower, 0)), math.Trunc(math.Min(upper, 255))
	}
	seed := func(x, y int) RegionGrowSeed { return RegionGrowSeed{X: x, Y: y} }
	cases := []struct {
		baseline string
		grow     func() (*RegionGrowResult, error)
	}{
		{"ConnectedThresholdImageFilterTest.png", func() (*RegionGrowResult, error) {
			return GrowConnectedThreshold(ctx, volume, []RegionGrowSeed{seed(165, 165)}, 0, 150, RegionGrowOptions{})
		}},
		{"ConfidenceConnectedImageFilterTest.png", func() (*RegionGrowResult, error) {
			params := ConfidenceConnectedParams{Multiplier: 2.5, Iterations: 10, InitialRadius: 3, quantizeBounds: uchar}
			r, err := GrowConfidenceConnected(ctx, volume, []RegionGrowSeed{seed(165, 165)}, params, RegionGrowOptions{})
			if err != nil {
				return nil, err
			}
			t.Logf("confidence: mean %g variance %g, %d iterations (%s)", r.Mean, r.Variance, r.Iterations, r.Stop)
			return &r.RegionGrowResult, nil
		}},
		{"NeighborhoodConnectedImageFilterTest.png", func() (*RegionGrowResult, error) {
			return GrowNeighborhoodConnected(ctx, volume, []RegionGrowSeed{seed(146, 167)}, 0, 210, UniformNeighborhoodRadius(5), RegionGrowOptions{})
		}},
		{"IsolatedConnectedImageFilterTest.png", func() (*RegionGrowResult, error) {
			params := IsolatedConnectedParams{Lower: 0, Upper: 255, FindUpperThreshold: true, Tolerance: 1, integerMidpoint: true}
			r, err := GrowIsolatedConnected(ctx, volume, []RegionGrowSeed{seed(115, 85)}, []RegionGrowSeed{seed(107, 110)}, params, RegionGrowOptions{})
			if err != nil {
				return nil, err
			}
			t.Logf("isolated (upper): value %g, failed %v", r.IsolatedValue, r.ThresholdingFailed)
			return &r.RegionGrowResult, nil
		}},
		{"IsolatedConnectedImageFilterTest2.png", func() (*RegionGrowResult, error) {
			params := IsolatedConnectedParams{Lower: 0, Upper: 255, Tolerance: 1, integerMidpoint: true}
			r, err := GrowIsolatedConnected(ctx, volume,
				[]RegionGrowSeed{seed(175, 125), seed(176, 125)}, []RegionGrowSeed{seed(100, 170), seed(101, 170)}, params, RegionGrowOptions{})
			if err != nil {
				return nil, err
			}
			t.Logf("isolated (lower): value %g, failed %v", r.IsolatedValue, r.ThresholdingFailed)
			return &r.RegionGrowResult, nil
		}},
	}
	baselines := os.Getenv("DICOMGO_ITK_REGIONGROWING_BASELINES")
	for _, c := range cases {
		t.Run(c.baseline, func(t *testing.T) {
			result, err := c.grow()
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%d voxels in [%g, %g]", result.VoxelCount, result.Lower, result.Upper)
			if baselines == "" {
				return
			}
			want := readITKGrayPNG(t, filepath.Join(baselines, c.baseline))
			if want.columns != input.columns || want.rows != input.rows {
				t.Fatalf("baseline is %d×%d, input %d×%d", want.columns, want.rows, input.columns, input.rows)
			}
			differ := 0
			for y := 0; y < want.rows; y++ {
				for x := 0; x < want.columns; x++ {
					if (want.values[y*want.columns+x] != 0) != result.Masks[0].Get(x, y) {
						differ++
					}
				}
			}
			if differ != 0 {
				t.Fatalf("%d pixels differ from ITK's baseline", differ)
			}
		})
	}
	// ConfidenceConnectedImageFilterTest2: a seed outside the image gives an
	// empty region.
	empty, err := GrowConfidenceConnected(ctx, volume, []RegionGrowSeed{seed(9999, 9999)}, ConfidenceConnectedParams{Multiplier: 2.5, Iterations: 10, InitialRadius: 3, quantizeBounds: uchar}, RegionGrowOptions{})
	if err != nil || empty.VoxelCount != 0 {
		t.Fatalf("out-of-image seed: %v, %v", empty, err)
	}
}

type itkGrayImage struct {
	columns, rows int
	values        []float64
}

// readITKGrayPNG reads a PNG as ITK's reader does into unsigned char: 8-bit
// samples as they are, 16-bit samples cast (modulo 256).
func readITKGrayPNG(t *testing.T, path string) itkGrayImage {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	img, _, err := image.Decode(file)
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	out := itkGrayImage{columns: b.Dx(), rows: b.Dy(), values: make([]float64, b.Dx()*b.Dy())}
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			var v uint8
			switch m := img.(type) {
			case *image.Gray:
				v = m.GrayAt(b.Min.X+x, b.Min.Y+y).Y
			case *image.Gray16:
				v = uint8(m.Gray16At(b.Min.X+x, b.Min.Y+y).Y)
			default:
				v = color.GrayModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.Gray).Y
			}
			out.values[y*out.columns+x] = float64(v)
		}
	}
	return out
}
