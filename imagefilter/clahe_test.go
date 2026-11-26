package imagefilter

import (
	"context"
	"errors"
	"image"
	"math"
	"math/rand"
	"sort"
	"testing"
)

// syntheticRadiograph builds a PHI-free, DX-like test image: a smooth
// low-contrast background, a bright rounded "body" with faint internal
// structure and deterministic noise, all within [0, maxValue].
func syntheticRadiograph(width, height, maxValue int, seed int64) []uint16 {
	rng := rand.New(rand.NewSource(seed))
	out := make([]uint16, width*height)
	cx, cy := float64(width)/2, float64(height)/2
	rx, ry := float64(width)*0.36, float64(height)*0.42
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			fx, fy := float64(x), float64(y)
			v := 0.08 + 0.04*fx/float64(width)
			d := math.Pow((fx-cx)/rx, 2) + math.Pow((fy-cy)/ry, 2)
			if d < 1 {
				v += 0.45 + 0.12*(1-d)
				v += 0.02 * math.Sin(fx/9) * math.Cos(fy/13)
				v += 0.015 * math.Sin((fx+fy)/5)
			}
			v += 0.004 * rng.NormFloat64()
			out[y*width+x] = uint16(math.Round(math.Max(0, math.Min(1, v)) * float64(maxValue)))
		}
	}
	return out
}

func runCLAHE(t testing.TB, src []uint16, width, height int, opts CLAHEOptions) []uint16 {
	t.Helper()
	dst := make([]uint16, len(src))
	if err := CLAHE(context.Background(), dst, src, width, height, opts); err != nil {
		t.Fatalf("CLAHE: %v", err)
	}
	return dst
}

func TestCLAHEConstantImageIsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name     string
		bitDepth int
		value    uint16
	}{
		{"16-bit zero", 16, 0},
		{"16-bit mid", 16, 32768},
		{"16-bit max", 16, 65535},
		{"12-bit", 12, 1234},
		{"8-bit", 8, 77},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const w, h = 67, 45
			src := make([]uint16, w*h)
			for i := range src {
				src[i] = tc.value
			}
			for _, clip := range []float64{0, 1, 4, math.Inf(1)} {
				dst := runCLAHE(t, src, w, h, CLAHEOptions{BitDepth: tc.bitDepth, ClipLimit: clip, TilesX: 4, TilesY: 3})
				for i, v := range dst {
					if v != tc.value {
						t.Fatalf("clip %v: pixel %d = %d, want %d", clip, i, v, tc.value)
					}
				}
			}
		})
	}
}

func TestCLAHEClipLimitOneIsNearIdentity(t *testing.T) {
	const maxValue = 65535
	for _, size := range [][2]int{{512, 384}, {500, 381}} {
		w, h := size[0], size[1]
		src := syntheticRadiograph(w, h, maxValue, 1)
		dst := runCLAHE(t, src, w, h, CLAHEOptions{ClipLimit: 1, TilesX: 4, TilesY: 4, Bins: 256})
		var sumAbs, maxAbs float64
		for i := range src {
			d := math.Abs(float64(dst[i]) - float64(src[i]))
			sumAbs += d
			maxAbs = math.Max(maxAbs, d)
		}
		meanAbs := sumAbs / float64(len(src))
		// Clipping at ceil(pixels/bins) flattens every tile histogram. When
		// the tile size is a multiple of the bin count the flat histogram is
		// exact and so is the identity; otherwise bins differ by at most one
		// count, so the mapping may deviate by at most bins/pixels of the
		// range (~2.2% for tiles of 125×95 pixels over 256 bins).
		if meanAbs > 0.005*maxValue || maxAbs > 0.022*maxValue {
			t.Fatalf("%dx%d: clip limit 1 moved values: mean |Δ|=%.1f max |Δ|=%.1f of %d", w, h, meanAbs, maxAbs, maxValue)
		}
		t.Logf("%dx%d clip limit 1: mean |Δ|=%.2f (%.4f%%), max |Δ|=%.0f (%.3f%%)", w, h, meanAbs, 100*meanAbs/maxValue, maxAbs, 100*maxAbs/maxValue)

		// A larger clip limit must move the image away from the identity:
		// that is the enhancement the filter exists for.
		enhanced := runCLAHE(t, src, w, h, CLAHEOptions{ClipLimit: 4, TilesX: 4, TilesY: 4, Bins: 256})
		var enhancedAbs float64
		for i := range src {
			enhancedAbs += math.Abs(float64(enhanced[i]) - float64(src[i]))
		}
		if enhancedAbs/float64(len(src)) < 0.01*maxValue {
			t.Fatalf("%dx%d: clip limit 4 barely changed the image: mean |Δ|=%.1f", w, h, enhancedAbs/float64(len(src)))
		}
	}
}

func TestCLAHESingleTileMappingIsMonotonic(t *testing.T) {
	const w, h = 300, 200
	src := syntheticRadiograph(w, h, 4095, 2)
	for _, clip := range []float64{1, 2.5, 8, math.Inf(1)} {
		dst := runCLAHE(t, src, w, h, CLAHEOptions{BitDepth: 12, TilesX: 1, TilesY: 1, ClipLimit: clip, Bins: 512})
		order := make([]int, len(src))
		for i := range order {
			order[i] = i
		}
		sort.Slice(order, func(a, b int) bool { return src[order[a]] < src[order[b]] })
		for i := 1; i < len(order); i++ {
			a, b := order[i-1], order[i]
			if src[a] < src[b] && dst[a] > dst[b] {
				t.Fatalf("clip %v: input %d→%d but %d→%d", clip, src[a], dst[a], src[b], dst[b])
			}
			if src[a] == src[b] && dst[a] != dst[b] {
				t.Fatalf("clip %v: equal inputs %d mapped to %d and %d", clip, src[a], dst[a], dst[b])
			}
		}
	}
}

// TestCLAHEMappingIsMonotonicAtEveryPosition checks that, with the tile
// weights of any fixed pixel position, a higher input value never maps to a
// lower output: every tile mapping is non-decreasing and the bilinear blend
// uses non-negative weights.
func TestCLAHEMappingIsMonotonicAtEveryPosition(t *testing.T) {
	const w, h, maxValue = 257, 190, 4095
	src := syntheticRadiograph(w, h, maxValue, 3)
	cfg, err := CLAHEOptions{BitDepth: 12, TilesX: 5, TilesY: 3, ClipLimit: 3, Bins: 300}.resolve(w, h)
	if err != nil {
		t.Fatal(err)
	}
	model, err := buildCLAHEModel(context.Background(), src, w, h, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for tile, mapping := range model.maps {
		for k := 1; k < len(mapping); k++ {
			if mapping[k] < mapping[k-1] {
				t.Fatalf("tile %d mapping decreases at bin %d", tile, k)
			}
		}
	}
	colLo, colHi, colW := axisWeights(model.colStart)
	rowLo, rowHi, rowW := axisWeights(model.rowStart)
	for _, p := range [][2]int{{0, 0}, {w - 1, h - 1}, {w / 2, h / 2}, {31, 150}, {200, 17}} {
		x, y := p[0], p[1]
		prev := float32(-1)
		for v := 0; v <= maxValue; v++ {
			got := referenceValue(model, colLo[x], colHi[x], colW[x], rowLo[y], rowHi[y], rowW[y], v)
			if got < prev-1e-3 {
				t.Fatalf("pixel (%d,%d): value %d maps to %.3f below %.3f", x, y, v, got, prev)
			}
			prev = got
		}
	}
}

func referenceValue(m *claheModel, xl, xh int, wx float32, yl, yh int, wy float32, value int) float32 {
	at := func(tx, ty int) float32 {
		mapping := m.maps[ty*m.cfg.tilesX+tx]
		b := int(m.valueBin[value])
		f := m.valueFrac[value]
		return mapping[b] + f*(mapping[b+1]-mapping[b])
	}
	top := at(xl, yl) + wx*(at(xh, yl)-at(xl, yl))
	bottom := at(xl, yh) + wx*(at(xh, yh)-at(xl, yh))
	return top + wy*(bottom-top)
}

func TestCLAHEOutputStaysInRange(t *testing.T) {
	for _, bitDepth := range []int{8, 12, 14, 16} {
		maxValue := 1<<bitDepth - 1
		const w, h = 211, 157
		rng := rand.New(rand.NewSource(int64(bitDepth)))
		src := make([]uint16, w*h)
		for i := range src {
			src[i] = uint16(rng.Intn(maxValue + 1))
		}
		// Out-of-domain inputs are treated as the maximum, never wrapped.
		src[0], src[1] = 65535, uint16(maxValue)
		for _, clip := range []float64{1, 2, 40, math.Inf(1)} {
			dst := runCLAHE(t, src, w, h, CLAHEOptions{BitDepth: bitDepth, ClipLimit: clip, TilesX: 7, TilesY: 5})
			var lo, hi uint16 = math.MaxUint16, 0
			for _, v := range dst {
				lo, hi = min(lo, v), max(hi, v)
			}
			if int(hi) > maxValue {
				t.Fatalf("%d-bit clip %v: output max %d above %d", bitDepth, clip, hi, maxValue)
			}
			if hi == lo {
				t.Fatalf("%d-bit clip %v: output collapsed to %d", bitDepth, clip, lo)
			}
		}
	}
}

// TestCLAHETileBoundariesAreContinuous compares the largest step between
// neighbouring pixels across tile boundaries with the largest step inside
// tiles. Bilinear interpolation must keep them comparable; the per-tile
// mapping without interpolation (the block artefact CLAHE avoids) must not.
func TestCLAHETileBoundariesAreContinuous(t *testing.T) {
	const w, h, maxValue = 480, 360, 65535
	src := make([]uint16, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := 0.3 + 0.2*float64(x)/w + 0.15*math.Sin(float64(y)/37) + 0.1*math.Cos(float64(x)/23)
			src[y*w+x] = uint16(v * maxValue)
		}
	}
	opts := CLAHEOptions{TilesX: 6, TilesY: 4, ClipLimit: 3, Bins: 512}
	dst := runCLAHE(t, src, w, h, opts)
	cfg, _ := opts.resolve(w, h)
	model, err := buildCLAHEModel(context.Background(), src, w, h, cfg)
	if err != nil {
		t.Fatal(err)
	}
	boundaryCol := map[int]bool{}
	for _, s := range model.colStart[1 : len(model.colStart)-1] {
		boundaryCol[s] = true
	}
	boundaryRow := map[int]bool{}
	for _, s := range model.rowStart[1 : len(model.rowStart)-1] {
		boundaryRow[s] = true
	}
	nearest := func(x, y int) float64 {
		tx := sort.SearchInts(model.colStart, x+1) - 1
		ty := sort.SearchInts(model.rowStart, y+1) - 1
		mapping := model.maps[ty*cfg.tilesX+tx]
		v := int(src[y*w+x])
		b, f := int(model.valueBin[v]), model.valueFrac[v]
		return float64(mapping[b] + f*(mapping[b+1]-mapping[b]))
	}
	var interior, boundary, blockyBoundary float64
	for y := 0; y < h; y++ {
		for x := 1; x < w; x++ {
			step := math.Abs(float64(dst[y*w+x]) - float64(dst[y*w+x-1]))
			if boundaryCol[x] {
				boundary = math.Max(boundary, step)
				blockyBoundary = math.Max(blockyBoundary, math.Abs(nearest(x, y)-nearest(x-1, y)))
			} else {
				interior = math.Max(interior, step)
			}
		}
	}
	for y := 1; y < h; y++ {
		for x := 0; x < w; x++ {
			step := math.Abs(float64(dst[y*w+x]) - float64(dst[(y-1)*w+x]))
			if boundaryRow[y] {
				boundary = math.Max(boundary, step)
				blockyBoundary = math.Max(blockyBoundary, math.Abs(nearest(x, y)-nearest(x, y-1)))
			} else {
				interior = math.Max(interior, step)
			}
		}
	}
	t.Logf("max step: interior %.0f, tile boundary %.0f, uninterpolated boundary %.0f", interior, boundary, blockyBoundary)
	if boundary > 1.5*interior+2 {
		t.Fatalf("tile boundary step %.0f exceeds interior step %.0f", boundary, interior)
	}
	if blockyBoundary < 3*interior {
		t.Fatalf("test image does not expose block artefacts (uninterpolated %.0f, interior %.0f)", blockyBoundary, interior)
	}
}

func TestCLAHEIncreasesLocalContrast(t *testing.T) {
	const w, h = 512, 512
	src := syntheticRadiograph(w, h, 65535, 4)
	dst := runCLAHE(t, src, w, h, CLAHEOptions{})
	// A 32×32 patch inside the body carries faint structure.
	std := func(values []uint16) float64 {
		var sum, sq float64
		n := 0
		for y := 240; y < 272; y++ {
			for x := 240; x < 272; x++ {
				v := float64(values[y*w+x])
				sum += v
				sq += v * v
				n++
			}
		}
		mean := sum / float64(n)
		return math.Sqrt(sq/float64(n) - mean*mean)
	}
	if before, after := std(src), std(dst); after < 2*before {
		t.Fatalf("local standard deviation %.0f → %.0f; expected a clear increase", before, after)
	}
}

func TestCLAHEWorkersInPlaceAndWrappersAgree(t *testing.T) {
	const w, h = 333, 222
	src := syntheticRadiograph(w, h, 65535, 5)
	serial := runCLAHE(t, src, w, h, CLAHEOptions{Workers: 1, TilesX: 5, TilesY: 7})
	parallel := runCLAHE(t, src, w, h, CLAHEOptions{Workers: 8, TilesX: 5, TilesY: 7})
	inPlace := append([]uint16(nil), src...)
	if err := CLAHE(context.Background(), inPlace, inPlace, w, h, CLAHEOptions{TilesX: 5, TilesY: 7}); err != nil {
		t.Fatal(err)
	}
	gray16 := image.NewGray16(image.Rect(10, 20, 10+w, 20+h))
	for i, v := range src {
		gray16.Pix[2*i], gray16.Pix[2*i+1] = uint8(v>>8), uint8(v)
	}
	wrapped, err := CLAHEGray16(context.Background(), gray16, CLAHEOptions{TilesX: 5, TilesY: 7})
	if err != nil {
		t.Fatal(err)
	}
	if wrapped.Bounds() != gray16.Bounds() {
		t.Fatalf("bounds %v, want %v", wrapped.Bounds(), gray16.Bounds())
	}
	for i := range src {
		w16 := uint16(wrapped.Pix[2*i])<<8 | uint16(wrapped.Pix[2*i+1])
		if serial[i] != parallel[i] || serial[i] != inPlace[i] || serial[i] != w16 {
			t.Fatalf("pixel %d: serial %d parallel %d in-place %d Gray16 %d", i, serial[i], parallel[i], inPlace[i], w16)
		}
	}

	gray := image.NewGray(image.Rect(0, 0, w, h))
	for i, v := range src {
		gray.Pix[i] = uint8(v >> 8)
	}
	enhanced, err := CLAHEGray(context.Background(), gray, CLAHEOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if enhanced.Bounds() != gray.Bounds() {
		t.Fatalf("8-bit bounds %v", enhanced.Bounds())
	}
	if _, err := CLAHEGray(context.Background(), gray, CLAHEOptions{BitDepth: 12}); !errors.Is(err, ErrInvalidCLAHEOptions) {
		t.Fatalf("8-bit image with BitDepth 12: %v", err)
	}
}

func TestCLAHESmallImagesAndUnevenTiles(t *testing.T) {
	for _, size := range [][2]int{{1, 1}, {1, 9}, {3, 2}, {17, 5}, {101, 37}} {
		w, h := size[0], size[1]
		src := make([]uint16, w*h)
		for i := range src {
			src[i] = uint16(i * 997 % 65536)
		}
		dst := runCLAHE(t, src, w, h, CLAHEOptions{TilesX: 8, TilesY: 8})
		if len(dst) != w*h {
			t.Fatalf("%dx%d: %d outputs", w, h, len(dst))
		}
	}
}

func TestCLAHERejectsInvalidInput(t *testing.T) {
	src := make([]uint16, 16)
	dst := make([]uint16, 16)
	for _, tc := range []struct {
		name string
		dst  []uint16
		w, h int
		opts CLAHEOptions
	}{
		{"zero width", dst, 0, 4, CLAHEOptions{}},
		{"short dst", dst[:8], 4, 4, CLAHEOptions{}},
		{"clip below one", dst, 4, 4, CLAHEOptions{ClipLimit: 0.5}},
		{"negative clip", dst, 4, 4, CLAHEOptions{ClipLimit: -1}},
		{"NaN clip", dst, 4, 4, CLAHEOptions{ClipLimit: math.NaN()}},
		{"one bin", dst, 4, 4, CLAHEOptions{Bins: 1}},
		{"bins above levels", dst, 4, 4, CLAHEOptions{BitDepth: 8, Bins: 257}},
		{"bit depth 17", dst, 4, 4, CLAHEOptions{BitDepth: 17}},
		{"negative tiles", dst, 4, 4, CLAHEOptions{TilesX: -1}},
	} {
		if err := CLAHE(context.Background(), tc.dst, src, tc.w, tc.h, tc.opts); !errors.Is(err, ErrInvalidCLAHEOptions) {
			t.Errorf("%s: err = %v, want ErrInvalidCLAHEOptions", tc.name, err)
		}
	}
}

func TestCLAHEHonoursCancellation(t *testing.T) {
	const w, h = 256, 256
	src := syntheticRadiograph(w, h, 65535, 6)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := CLAHE(ctx, make([]uint16, len(src)), src, w, h, CLAHEOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context: %v", err)
	}
	// Cancel from inside the work: the per-worker constructor runs once the
	// scheduler starts, so the remaining tiles and rows are skipped.
	ctx, cancel = context.WithCancel(context.Background())
	calls := 0
	err := runParallel(ctx, 100, 1, func() func(int) {
		return func(int) {
			calls++
			if calls == 3 {
				cancel()
			}
		}
	})
	if !errors.Is(err, context.Canceled) || calls != 3 {
		t.Fatalf("runParallel after cancel: err=%v calls=%d", err, calls)
	}
}

func TestClipHistogramPreservesCountsAndRespectsLimit(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for trial := 0; trial < 200; trial++ {
		bins := 2 + rng.Intn(300)
		hist := make([]uint32, bins)
		pixels := 0
		for i := 0; i < 1+rng.Intn(5000); i++ {
			// Skewed fills put most counts in a few bins.
			b := int(math.Abs(rng.NormFloat64()) * float64(bins) / 6)
			if b >= bins {
				b = bins - 1
			}
			hist[b]++
			pixels++
		}
		clip := 1 + rng.Float64()*6
		limit := clipCount(clip, pixels, bins)
		clipHistogram(hist, limit)
		total := 0
		for _, h := range hist {
			if h > limit {
				t.Fatalf("trial %d: bin %d above limit %d", trial, h, limit)
			}
			total += int(h)
		}
		if total != pixels {
			t.Fatalf("trial %d: %d counts after clipping, want %d", trial, total, pixels)
		}
	}
}

// BenchmarkCLAHE4kDX measures a 4096×4096 16-bit DX-sized synthetic image
// with the default 8×8 tiles, clip limit 2.5 and 1024 bins.
func BenchmarkCLAHE4kDX(b *testing.B) {
	const w, h = 4096, 4096
	src := syntheticRadiograph(w, h, 65535, 8)
	dst := make([]uint16, len(src))
	b.SetBytes(int64(len(src) * 2))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := CLAHE(context.Background(), dst, src, w, h, CLAHEOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCLAHE4kDXSerial is BenchmarkCLAHE4kDX on one worker.
func BenchmarkCLAHE4kDXSerial(b *testing.B) {
	const w, h = 4096, 4096
	src := syntheticRadiograph(w, h, 65535, 8)
	dst := make([]uint16, len(src))
	b.SetBytes(int64(len(src) * 2))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := CLAHE(context.Background(), dst, src, w, h, CLAHEOptions{Workers: 1}); err != nil {
			b.Fatal(err)
		}
	}
}
