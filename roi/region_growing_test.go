package roi

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
)

// gridVolume builds a volume from rows of text per slice: a digit d is the
// value 100·d, '.' is 0 and '#' is unavailable.
func gridVolume(t *testing.T, slices ...[]string) (RegionGrowVolume, *int) {
	t.Helper()
	rows, columns := len(slices[0]), len(slices[0][0])
	values := make([][][]float64, len(slices))
	valid := make([][][]bool, len(slices))
	for z, lines := range slices {
		if len(lines) != rows {
			t.Fatalf("slice %d has %d rows, want %d", z, len(lines), rows)
		}
		values[z] = make([][]float64, rows)
		valid[z] = make([][]bool, rows)
		for y, line := range lines {
			if len(line) != columns {
				t.Fatalf("slice %d row %d has %d columns, want %d", z, y, len(line), columns)
			}
			values[z][y] = make([]float64, columns)
			valid[z][y] = make([]bool, columns)
			for x, c := range line {
				switch {
				case c == '#':
				case c == '.':
					valid[z][y][x] = true
				default:
					values[z][y][x] = float64(c-'0') * 100
					valid[z][y][x] = true
				}
			}
		}
	}
	calls := new(int)
	return RegionGrowVolume{
		Columns: columns, Rows: rows, Slices: len(slices),
		ValueAt: func(x, y, z int) (float64, bool) {
			*calls++
			if x < 0 || y < 0 || z < 0 || x >= columns || y >= rows || z >= len(slices) {
				t.Fatalf("ValueAt(%d, %d, %d) outside the volume", x, y, z)
			}
			return values[z][y][x], valid[z][y][x]
		},
	}, calls
}

// maskText renders the masks of a region in the layout of gridVolume.
func maskText(masks map[int]*RasterMask, columns, rows, slices int) []string {
	out := make([]string, 0, rows*slices)
	for z := 0; z < slices; z++ {
		for y := 0; y < rows; y++ {
			var b strings.Builder
			for x := 0; x < columns; x++ {
				if masks[z].Get(x, y) {
					b.WriteByte('x')
				} else {
					b.WriteByte('.')
				}
			}
			out = append(out, b.String())
		}
	}
	return out
}

func assertRegion(t *testing.T, result *RegionGrowResult, volume RegionGrowVolume, want ...string) {
	t.Helper()
	got := maskText(result.Masks, volume.Columns, volume.Rows, volume.Slices)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("region:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	count := 0
	for _, line := range want {
		count += strings.Count(line, "x")
	}
	if result.VoxelCount != count {
		t.Fatalf("VoxelCount = %d, want %d", result.VoxelCount, count)
	}
}

func TestGrowConnectedThreshold(t *testing.T) {
	image := []string{
		"11..22",
		"11..2.",
		"...3..",
		"1.....",
	}
	for _, c := range []struct {
		name         string
		seeds        []RegionGrowSeed
		lower, upper float64
		connectivity Connectivity3D
		want         []string
		ignored      int
	}{
		{"single blob", []RegionGrowSeed{{0, 0, 0}}, 100, 100, 0, []string{
			"xx....", "xx....", "......", "......"}, 0},
		{"several seeds grow one region", []RegionGrowSeed{{0, 0, 0}, {0, 3, 0}, {4, 0, 0}}, 100, 200, 0, []string{
			"xx..xx", "xx..x.", "......", "x....."}, 0},
		{"8-connected reaches the diagonal", []RegionGrowSeed{{4, 1, 0}}, 200, 300, Connectivity26, []string{
			"....xx", "....x.", "...x..", "......"}, 0},
		{"4-connected stops at the diagonal", []RegionGrowSeed{{4, 1, 0}}, 200, 300, Connectivity6, []string{
			"....xx", "....x.", "......", "......"}, 0},
		{"seeds outside the interval or volume are ignored", []RegionGrowSeed{{2, 0, 0}, {9, 0, 0}, {0, 0, 1}, {0, 0, 0}}, 100, 100, 0, []string{
			"xx....", "xx....", "......", "......"}, 3},
		{"no usable seed gives an empty region", []RegionGrowSeed{{2, 0, 0}}, 100, 100, 0, []string{
			"......", "......", "......", "......"}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			volume, _ := gridVolume(t, image)
			result, err := GrowConnectedThreshold(context.Background(), volume, c.seeds, c.lower, c.upper, RegionGrowOptions{Connectivity: c.connectivity})
			if err != nil {
				t.Fatal(err)
			}
			assertRegion(t, result, volume, c.want...)
			if len(result.IgnoredSeeds) != c.ignored || result.Fills != 1 || result.Lower != c.lower || result.Upper != c.upper {
				t.Fatalf("result = %+v, want %d ignored seeds and one fill", result, c.ignored)
			}
		})
	}
}

func TestGrowConnectedThresholdMatchesFloodFill3D(t *testing.T) {
	volume, _ := gridVolume(t,
		[]string{"11.1", "1#11", "..1."},
		[]string{"1..1", "1111", "1..."},
		[]string{"..11", "....", "1111"},
	)
	inside := func(x, y, z int) bool {
		v, ok := volume.ValueAt(x, y, z)
		return ok && v == 100
	}
	for _, connectivity := range []Connectivity3D{Connectivity6, Connectivity18, Connectivity26} {
		want, err := FloodFill3D(4, 3, 3, 0, 0, 0, connectivity, inside)
		if err != nil {
			t.Fatal(err)
		}
		got, err := GrowConnectedThreshold(nil, volume, []RegionGrowSeed{{0, 0, 0}}, 50, 150, RegionGrowOptions{Connectivity: connectivity})
		if err != nil {
			t.Fatal(err)
		}
		if a, b := maskText(got.Masks, 4, 3, 3), maskText(want, 4, 3, 3); strings.Join(a, "\n") != strings.Join(b, "\n") {
			t.Fatalf("connectivity %d: %v, FloodFill3D %v", connectivity, a, b)
		}
	}
}

func TestGrowNeighborhoodConnectedBlocksOneVoxelBridges(t *testing.T) {
	// Two 4×4 squares joined by a one-pixel bridge; boxes are clamped at
	// the image border.
	image := []string{
		"1111.1111",
		"111111111",
		"1111.1111",
		"1111.1111",
		".........",
	}
	volume, _ := gridVolume(t, image)
	threshold, err := GrowConnectedThreshold(context.Background(), volume, []RegionGrowSeed{{1, 1, 0}}, 100, 100, RegionGrowOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if threshold.VoxelCount != 33 {
		t.Fatalf("connected threshold grew %d voxels, want both squares and the bridge (33)", threshold.VoxelCount)
	}
	result, err := GrowNeighborhoodConnected(context.Background(), volume, []RegionGrowSeed{{1, 1, 0}}, 100, 100, UniformNeighborhoodRadius(1), RegionGrowOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertRegion(t, result, volume,
		"xxx......",
		"xxx......",
		"xxx......",
		".........",
		".........",
	)
	// A second seed grows the right square too; a seed whose box reaches
	// the gap starts nothing.
	result, err = GrowNeighborhoodConnected(context.Background(), volume, []RegionGrowSeed{{1, 1, 0}, {7, 1, 0}, {5, 2, 0}}, 100, 100, UniformNeighborhoodRadius(1), RegionGrowOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertRegion(t, result, volume,
		"xxx...xxx",
		"xxx...xxx",
		"xxx...xxx",
		".........",
		".........",
	)
	if len(result.IgnoredSeeds) != 1 || result.IgnoredSeeds[0] != (RegionGrowSeed{5, 2, 0}) {
		t.Fatalf("ignored seeds = %v, want the seed whose box leaves the square", result.IgnoredSeeds)
	}
}

func TestGrowNeighborhoodConnectedUsesTheSliceRadiusAndUnavailableVoxels(t *testing.T) {
	slice := []string{"11111", "11111", "11111", "11111", "11111"}
	volume, _ := gridVolume(t, slice, slice, []string{"11111", "11111", "11#11", "11111", "11111"})
	seed := []RegionGrowSeed{{2, 2, 0}}
	inPlane, err := GrowNeighborhoodConnected(context.Background(), volume, seed, 100, 100, NeighborhoodRadius{X: 1, Y: 1}, RegionGrowOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Slices 0 and 1 are complete (boxes are clamped); on slice 2 only the
	// 16 border centres have boxes without the unavailable voxel.
	if inPlane.VoxelCount != 66 || inPlane.Masks[2].Count() != 16 {
		t.Fatalf("in-plane radius grew %d voxels, want 66", inPlane.VoxelCount)
	}
	full, err := GrowNeighborhoodConnected(context.Background(), volume, seed, 100, 100, UniformNeighborhoodRadius(1), RegionGrowOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Boxes of slices 1 and 2 reach the unavailable voxel unless they are
	// on the border.
	if full.VoxelCount != 57 || full.Masks[0].Count() != 25 || full.Masks[1].Count() != 16 {
		t.Fatalf("3D radius grew %d voxels, want 25 + 16 + 16", full.VoxelCount)
	}
}

func TestGrowConfidenceConnectedStatistics(t *testing.T) {
	// A 4×4 lesion alternating 100/200 inside a background of 900.
	image := []string{
		"999999",
		"912129",
		"921219",
		"912129",
		"921219",
		"999999",
	}
	volume, _ := gridVolume(t, image)
	seed := []RegionGrowSeed{{2, 2, 0}}
	result, err := GrowConfidenceConnected(context.Background(), volume, seed, ConfidenceConnectedParams{Multiplier: 2.5, Iterations: 3, InitialRadius: 1}, RegionGrowOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertRegion(t, &result.RegionGrowResult, volume,
		"......",
		".xxxx.",
		".xxxx.",
		".xxxx.",
		".xxxx.",
		"......",
	)
	// Over the lesion: 8 × 100 and 8 × 200, mean 150, variance
	// 16·50²/15.
	wantVariance := 16.0 * 2500 / 15
	if result.Mean != 150 || math.Abs(result.Variance-wantVariance) > 1e-9 || result.Iterations != 3 || result.Stop != ConfidenceStopIterations {
		t.Fatalf("statistics = mean %g variance %g after %d iterations (%s), want 150, %g, 3", result.Mean, result.Variance, result.Iterations, result.Stop, wantVariance)
	}
	spread := 2.5 * math.Sqrt(wantVariance)
	if math.Abs(result.Lower-(150-spread)) > 1e-9 || math.Abs(result.Upper-(150+spread)) > 1e-9 {
		t.Fatalf("interval = [%g, %g], want 150 ± %g", result.Lower, result.Upper, spread)
	}
	// The initial 3×3 box around (2,2) holds five 100s and four 200s.
	if result.InitialSamples != 9 || result.Fills != 4 {
		t.Fatalf("initial samples %d, fills %d, want 9 and 4", result.InitialSamples, result.Fills)
	}
}

func TestGrowConfidenceConnectedKeepsTheSeedAndStopsOnZeroVariance(t *testing.T) {
	image := []string{
		"1112",
		"1113",
		"5555",
	}
	volume, _ := gridVolume(t, image)
	// Radius 0 with one seed: no variance, so the interval is the seed
	// value and the uniform region stops the iterations.
	result, err := GrowConfidenceConnected(context.Background(), volume, []RegionGrowSeed{{0, 0, 0}}, ConfidenceConnectedParams{Multiplier: 2.5, Iterations: 4}, RegionGrowOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertRegion(t, &result.RegionGrowResult, volume, "xxx.", "xxx.", "....")
	if result.Stop != ConfidenceStopZeroVariance || result.Iterations != 0 || result.Lower != 100 || result.Upper != 100 || result.Variance != 0 {
		t.Fatalf("result = %+v, want a zero-variance stop at [100, 100]", result)
	}
	// Two seeds with different values: the interval widens to both even
	// with a zero multiplier.
	result, err = GrowConfidenceConnected(context.Background(), volume, []RegionGrowSeed{{0, 0, 0}, {3, 1, 0}}, ConfidenceConnectedParams{Iterations: 0}, RegionGrowOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Lower != 100 || result.Upper != 300 || result.VoxelCount != 8 {
		t.Fatalf("result = [%g, %g] with %d voxels, want [100, 300] with 8", result.Lower, result.Upper, result.VoxelCount)
	}
	// Seeds outside the volume or unavailable give an empty region.
	holes, _ := gridVolume(t, []string{"#1"})
	result, err = GrowConfidenceConnected(context.Background(), holes, []RegionGrowSeed{{0, 0, 0}, {5, 0, 0}}, DefaultConfidenceConnectedParams(), RegionGrowOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.VoxelCount != 0 || result.Stop != ConfidenceStopEmptyRegion || len(result.IgnoredSeeds) != 2 || !math.IsNaN(result.Mean) {
		t.Fatalf("result = %+v, want an empty region with both seeds ignored", result)
	}
}

func TestGrowIsolatedConnectedSeparatesVesselFromBone(t *testing.T) {
	// Vessel (3) reaches bone (9) through a bridge of 5. As in ITK, the
	// threshold separates the seeds, so the bridge joins the side that
	// keeps it.
	image := []string{
		"333....999",
		"333....999",
		"3335555999",
	}
	vessel := []RegionGrowSeed{{0, 0, 0}}
	bone := []RegionGrowSeed{{9, 0, 0}}
	for _, c := range []struct {
		name     string
		seeds1   []RegionGrowSeed
		seeds2   []RegionGrowSeed
		params   IsolatedConnectedParams
		isolated func(float64) bool
		want     []string
		failed   bool
	}{
		{
			name: "upper threshold keeps the vessel", seeds1: vessel, seeds2: bone,
			params:   IsolatedConnectedParams{Lower: 200, Upper: 1000, FindUpperThreshold: true},
			isolated: func(v float64) bool { return v >= 898 && v < 900 },
			want:     []string{"xxx.......", "xxx.......", "xxxxxxx..."},
		},
		{
			name: "integer bisection", seeds1: vessel, seeds2: bone,
			params: IsolatedConnectedParams{Lower: 200, Upper: 1000, FindUpperThreshold: true, integerMidpoint: true},
			// 1000, 600, 800, 900, 850, 875, 887, 893, 896, 898: ITK's
			// integer bisection stops two levels below the bone.
			isolated: func(v float64) bool { return v == 898 },
			want:     []string{"xxx.......", "xxx.......", "xxxxxxx..."},
		},
		{
			name: "lower threshold keeps the bone", seeds1: bone, seeds2: vessel,
			params:   IsolatedConnectedParams{Lower: 200, Upper: 1000, Tolerance: 0.5},
			isolated: func(v float64) bool { return v > 300 && v <= 301 },
			want:     []string{".......xxx", ".......xxx", "...xxxxxxx"},
		},
		{
			name: "inseparable seeds are reported", seeds1: vessel, seeds2: []RegionGrowSeed{{2, 2, 0}},
			params:   IsolatedConnectedParams{Lower: 200, Upper: 1000, FindUpperThreshold: true},
			isolated: func(v float64) bool { return v < 300 },
			want:     []string{"..........", "..........", ".........."},
			failed:   true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			volume, _ := gridVolume(t, image)
			result, err := GrowIsolatedConnected(context.Background(), volume, c.seeds1, c.seeds2, c.params, RegionGrowOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !c.isolated(result.IsolatedValue) {
				t.Fatalf("isolated value %g outside the expected range", result.IsolatedValue)
			}
			assertRegion(t, &result.RegionGrowResult, volume, c.want...)
			if result.ThresholdingFailed != c.failed {
				t.Fatalf("ThresholdingFailed = %v, want %v", result.ThresholdingFailed, c.failed)
			}
		})
	}
}

func TestGrowIsolatedConnectedStopsFillsAtTheSecondSeed(t *testing.T) {
	// A long corridor (one column) with the second seed next to the first:
	// the bisection fills stop at the second seed instead of flooding the
	// corridor.
	column := []string{"1"}
	for i := 0; i < 200; i++ {
		column = append(column, "2")
	}
	volume, calls := gridVolume(t, column)
	result, err := GrowIsolatedConnected(context.Background(), volume, []RegionGrowSeed{{0, 0, 0}}, []RegionGrowSeed{{0, 1, 0}},
		IsolatedConnectedParams{Lower: 0, Upper: 1024, FindUpperThreshold: true}, RegionGrowOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.ThresholdingFailed || result.VoxelCount != 1 || result.IsolatedValue < 198 || result.IsolatedValue >= 200 {
		t.Fatalf("result = %+v", result)
	}
	if *calls > 40*result.Fills {
		t.Fatalf("%d value reads over %d fills: the bisection fills did not stop early", *calls, result.Fills)
	}
}

func TestRegionGrowingRejectsInvalidInput(t *testing.T) {
	volume, _ := gridVolume(t, []string{"11", "11"})
	seeds := []RegionGrowSeed{{0, 0, 0}}
	ctx := context.Background()
	for _, c := range []struct {
		name string
		run  func() error
		want error
	}{
		{"nil value source", func() error {
			_, err := GrowConnectedThreshold(ctx, RegionGrowVolume{Columns: 2, Rows: 2, Slices: 1}, seeds, 0, 1, RegionGrowOptions{})
			return err
		}, ErrRegionGrowInvalidVolume},
		{"no slices", func() error {
			v := volume
			v.Slices = 0
			_, err := GrowConnectedThreshold(ctx, v, seeds, 0, 1, RegionGrowOptions{})
			return err
		}, ErrRegionGrowInvalidVolume},
		{"no seeds", func() error {
			_, err := GrowConnectedThreshold(ctx, volume, nil, 0, 1, RegionGrowOptions{})
			return err
		}, ErrRegionGrowNoSeeds},
		{"inverted interval", func() error {
			_, err := GrowConnectedThreshold(ctx, volume, seeds, 2, 1, RegionGrowOptions{})
			return err
		}, ErrRegionGrowInvalidParameters},
		{"NaN interval", func() error {
			_, err := GrowNeighborhoodConnected(ctx, volume, seeds, math.NaN(), 1, NeighborhoodRadius{}, RegionGrowOptions{})
			return err
		}, ErrRegionGrowInvalidParameters},
		{"bad connectivity", func() error {
			_, err := GrowConnectedThreshold(ctx, volume, seeds, 0, 1, RegionGrowOptions{Connectivity: 8})
			return err
		}, ErrInvalidConnectivity},
		{"negative limit", func() error {
			_, err := GrowConnectedThreshold(ctx, volume, seeds, 0, 1, RegionGrowOptions{Limits: RegionGrowLimits{MaxFills: -1}})
			return err
		}, ErrRegionGrowInvalidParameters},
		{"negative radius", func() error {
			_, err := GrowNeighborhoodConnected(ctx, volume, seeds, 0, 1, NeighborhoodRadius{X: -1}, RegionGrowOptions{})
			return err
		}, ErrRegionGrowInvalidParameters},
		{"negative multiplier", func() error {
			_, err := GrowConfidenceConnected(ctx, volume, seeds, ConfidenceConnectedParams{Multiplier: -1}, RegionGrowOptions{})
			return err
		}, ErrRegionGrowInvalidParameters},
		{"isolated seed outside", func() error {
			_, err := GrowIsolatedConnected(ctx, volume, seeds, []RegionGrowSeed{{5, 5, 0}}, IsolatedConnectedParams{Upper: 1}, RegionGrowOptions{})
			return err
		}, ErrRegionGrowInvalidParameters},
		{"isolated without second seeds", func() error {
			_, err := GrowIsolatedConnected(ctx, volume, seeds, nil, IsolatedConnectedParams{Upper: 1}, RegionGrowOptions{})
			return err
		}, ErrRegionGrowNoSeeds},
		{"isolated infinite range", func() error {
			_, err := GrowIsolatedConnected(ctx, volume, seeds, seeds, IsolatedConnectedParams{Upper: math.Inf(1)}, RegionGrowOptions{})
			return err
		}, ErrRegionGrowInvalidParameters},
		{"isolated negative tolerance", func() error {
			_, err := GrowIsolatedConnected(ctx, volume, seeds, seeds, IsolatedConnectedParams{Upper: 1, Tolerance: -1}, RegionGrowOptions{})
			return err
		}, ErrRegionGrowInvalidParameters},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := c.run(); !errors.Is(err, c.want) {
				t.Fatalf("error = %v, want %v", err, c.want)
			}
		})
	}
}

func TestRegionGrowingLimitsAndCancellation(t *testing.T) {
	image := []string{strings.Repeat("1", 64)}
	for i := 0; i < 5; i++ {
		image = append(image, image[0])
	}
	volume, _ := gridVolume(t, image)
	seeds := []RegionGrowSeed{{0, 0, 0}}
	ctx := context.Background()
	if _, err := GrowConnectedThreshold(ctx, volume, seeds, 0, 200, RegionGrowOptions{Limits: RegionGrowLimits{MaxRegionVoxels: 100}}); !errors.Is(err, ErrRegionGrowResourceLimit) {
		t.Fatalf("region budget error = %v", err)
	}
	if _, err := GrowNeighborhoodConnected(ctx, volume, seeds, 0, 200, UniformNeighborhoodRadius(2), RegionGrowOptions{Limits: RegionGrowLimits{MaxNeighborhoodVoxels: 24}}); !errors.Is(err, ErrRegionGrowResourceLimit) {
		t.Fatalf("neighbourhood budget error = %v", err)
	}
	if _, err := GrowConfidenceConnected(ctx, volume, seeds, ConfidenceConnectedParams{Multiplier: 1, Iterations: 8}, RegionGrowOptions{Limits: RegionGrowLimits{MaxFills: 8}}); !errors.Is(err, ErrRegionGrowResourceLimit) {
		t.Fatalf("iteration budget error = %v", err)
	}
	// A tolerance far below the value spacing needs more fills than allowed.
	if _, err := GrowIsolatedConnected(ctx, volume, seeds, []RegionGrowSeed{{63, 5, 0}}, IsolatedConnectedParams{Upper: 1e6, FindUpperThreshold: true, Tolerance: 1e-9}, RegionGrowOptions{Limits: RegionGrowLimits{MaxFills: 10}}); !errors.Is(err, ErrRegionGrowResourceLimit) {
		t.Fatalf("bisection budget error = %v", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := GrowConnectedThreshold(cancelled, volume, seeds, 0, 200, RegionGrowOptions{}); !errors.Is(err, ErrRegionGrowCancelled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled error = %v", err)
	}
	// Cancellation while filling: the value source cancels after a few
	// reads of a large region.
	big := RegionGrowVolume{Columns: 256, Rows: 256, Slices: 64}
	running, stop := context.WithCancel(context.Background())
	defer stop()
	reads := 0
	big.ValueAt = func(x, y, z int) (float64, bool) {
		if reads++; reads == 5000 {
			stop()
		}
		return 1, true
	}
	if _, err := GrowConfidenceConnected(running, big, seeds, DefaultConfidenceConnectedParams(), RegionGrowOptions{}); !errors.Is(err, ErrRegionGrowCancelled) {
		t.Fatalf("mid-fill cancellation error = %v", err)
	}
	if reads > 256*256*64 {
		t.Fatalf("cancellation read %d values, more than the whole volume", reads)
	}
}
