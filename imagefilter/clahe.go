package imagefilter

import (
	"context"
	"errors"
	"fmt"
	"image"
	"math"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

const (
	// DefaultCLAHETiles is the default number of contextual regions along
	// each image axis (an 8×8 grid).
	DefaultCLAHETiles = 8
	// DefaultCLAHEClipLimit is the default normalized clip limit: each bin
	// may hold at most 2.5 times the mean bin count of its tile.
	DefaultCLAHEClipLimit = 2.5
	// DefaultCLAHEBins is the default number of histogram bins, reduced to
	// the number of representable values for bit depths below 10.
	DefaultCLAHEBins = 1024
	// DefaultCLAHEBitDepth is the default bit depth of the value domain
	// (16-bit P-values, 0..65535).
	DefaultCLAHEBitDepth = 16
)

// ErrInvalidCLAHEOptions reports options or buffers CLAHE cannot use.
var ErrInvalidCLAHEOptions = errors.New("imagefilter: invalid CLAHE options")

// CLAHEOptions configures CLAHE. The zero value selects every default.
type CLAHEOptions struct {
	// TilesX and TilesY are the numbers of contextual regions along the
	// columns and rows (0 selects DefaultCLAHETiles). They are reduced to the
	// image width and height when the image is smaller than the grid.
	TilesX, TilesY int
	// ClipLimit is Zuiderveld's normalized clip limit: a bin of a tile
	// histogram is clipped at ClipLimit times the tile's mean bin count and
	// the excess is redistributed over all bins. 0 selects
	// DefaultCLAHEClipLimit; 1 flattens every histogram, so the mapping is
	// close to the identity; larger values allow more contrast; +Inf disables
	// clipping (plain adaptive histogram equalization). Values in (0, 1),
	// negative values and NaN are rejected.
	ClipLimit float64
	// Bins is the number of histogram bins covering the value domain (0
	// selects DefaultCLAHEBins, capped at 2^BitDepth). Values inside one bin
	// are mapped by linear interpolation of that bin's cumulative counts, so
	// fewer bins than gray levels does not posterize the output.
	Bins int
	// BitDepth is the width of the value domain: input and output values lie
	// in [0, 2^BitDepth-1] (0 selects DefaultCLAHEBitDepth). Input values
	// above the maximum are treated as the maximum.
	BitDepth int
	// Workers bounds the goroutines used (0 selects the shared concurrency budget). The result
	// does not depend on the worker count.
	Workers int
}

type claheConfig struct {
	tilesX, tilesY int
	clipLimit      float64
	bins           int
	maxValue       int
	workers        int
}

func (o CLAHEOptions) resolve(width, height int) (claheConfig, error) {
	cfg := claheConfig{tilesX: o.TilesX, tilesY: o.TilesY, clipLimit: o.ClipLimit, bins: o.Bins, workers: o.Workers}
	bitDepth := o.BitDepth
	if bitDepth == 0 {
		bitDepth = DefaultCLAHEBitDepth
	}
	if bitDepth < 1 || bitDepth > 16 {
		return cfg, fmt.Errorf("%w: BitDepth=%d outside 1..16", ErrInvalidCLAHEOptions, o.BitDepth)
	}
	cfg.maxValue = 1<<bitDepth - 1
	if cfg.tilesX == 0 {
		cfg.tilesX = DefaultCLAHETiles
	}
	if cfg.tilesY == 0 {
		cfg.tilesY = DefaultCLAHETiles
	}
	if cfg.tilesX < 0 || cfg.tilesY < 0 {
		return cfg, fmt.Errorf("%w: tiles %d×%d", ErrInvalidCLAHEOptions, o.TilesX, o.TilesY)
	}
	cfg.tilesX = min(cfg.tilesX, width)
	cfg.tilesY = min(cfg.tilesY, height)
	switch {
	case cfg.clipLimit == 0:
		cfg.clipLimit = DefaultCLAHEClipLimit
	case math.IsNaN(cfg.clipLimit) || cfg.clipLimit < 1:
		return cfg, fmt.Errorf("%w: ClipLimit=%v (want 0, >= 1 or +Inf)", ErrInvalidCLAHEOptions, o.ClipLimit)
	}
	if cfg.bins == 0 {
		cfg.bins = min(DefaultCLAHEBins, cfg.maxValue+1)
	}
	if cfg.bins < 2 || cfg.bins > cfg.maxValue+1 {
		return cfg, fmt.Errorf("%w: Bins=%d outside 2..%d", ErrInvalidCLAHEOptions, o.Bins, cfg.maxValue+1)
	}
	if cfg.workers <= 0 {
		cfg.workers = parallel.DefaultWorkers()
	}
	return cfg, nil
}

// CLAHE equalizes a row-major width×height image of values in
// [0, 2^BitDepth-1] into dst. dst and src must hold width*height values; dst
// may be src for an in-place update. An image without contrast (every value
// equal) has nothing to equalize and is copied unchanged. ctx is checked
// between tiles and row chunks; on cancellation ctx.Err() is returned and dst
// may be partially written.
func CLAHE(ctx context.Context, dst, src []uint16, width, height int, opts CLAHEOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if width <= 0 || height <= 0 {
		return fmt.Errorf("%w: size %d×%d", ErrInvalidCLAHEOptions, width, height)
	}
	n := width * height
	if n/width != height || len(src) != n || len(dst) != n {
		return fmt.Errorf("%w: buffers of %d and %d values for %d×%d", ErrInvalidCLAHEOptions, len(dst), len(src), width, height)
	}
	cfg, err := opts.resolve(width, height)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	model, err := buildCLAHEModel(ctx, src, width, height, cfg)
	if err != nil {
		return err
	}
	if model.constant {
		if &dst[0] != &src[0] {
			copy(dst, src)
		}
		return ctx.Err()
	}
	return model.apply(ctx, dst, src)
}

// CLAHEGray16 equalizes a 16-bit image (for example the P-values of
// display.RenderGray16) and returns a new image with the same bounds.
// opts.BitDepth defaults to 16 as for CLAHE.
func CLAHEGray16(ctx context.Context, img *image.Gray16, opts CLAHEOptions) (*image.Gray16, error) {
	if img == nil {
		return nil, fmt.Errorf("%w: nil image", ErrInvalidCLAHEOptions)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	workers := opts.Workers
	if workers <= 0 {
		workers = parallel.DefaultWorkers()
	}
	values := make([]uint16, w*h)
	err := runParallel(ctx, h, workers, func() func(int) {
		return func(y int) {
			row := img.Pix[img.PixOffset(b.Min.X, b.Min.Y+y):]
			for x, dst := 0, values[y*w:(y+1)*w]; x < w; x++ {
				dst[x] = uint16(row[2*x])<<8 | uint16(row[2*x+1])
			}
		}
	})
	if err != nil {
		return nil, err
	}
	if err := CLAHE(ctx, values, values, w, h, opts); err != nil {
		return nil, err
	}
	out := image.NewGray16(b)
	err = runParallel(ctx, h, workers, func() func(int) {
		return func(y int) {
			pix := out.Pix[y*out.Stride : (y+1)*out.Stride]
			for x, v := range values[y*w : (y+1)*w] {
				pix[2*x], pix[2*x+1] = uint8(v>>8), uint8(v)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CLAHEGray equalizes an 8-bit image and returns a new image with the same
// bounds. opts.BitDepth must be 0 or 8.
func CLAHEGray(ctx context.Context, img *image.Gray, opts CLAHEOptions) (*image.Gray, error) {
	if img == nil {
		return nil, fmt.Errorf("%w: nil image", ErrInvalidCLAHEOptions)
	}
	if opts.BitDepth != 0 && opts.BitDepth != 8 {
		return nil, fmt.Errorf("%w: BitDepth=%d for an 8-bit image", ErrInvalidCLAHEOptions, opts.BitDepth)
	}
	opts.BitDepth = 8
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	values := make([]uint16, w*h)
	for y := 0; y < h; y++ {
		row := img.Pix[img.PixOffset(b.Min.X, b.Min.Y+y):]
		for x := 0; x < w; x++ {
			values[y*w+x] = uint16(row[x])
		}
	}
	if err := CLAHE(ctx, values, values, w, h, opts); err != nil {
		return nil, err
	}
	out := image.NewGray(b)
	for i, v := range values {
		out.Pix[i] = uint8(v)
	}
	return out, nil
}

// claheModel is the per-image state shared by the mapping phase: the tile
// grid, one cumulative mapping per tile and the per-value bin position.
type claheModel struct {
	width, height int
	cfg           claheConfig
	// colStart/rowStart hold the first pixel of each tile plus the image size
	// as a final sentinel.
	colStart, rowStart []int
	// maps[t] has bins+1 entries: maps[t][k] is the output value reached
	// after the first k clipped bins of tile t, scaled to [0, maxValue].
	maps [][]float32
	// valueBin/valueFrac locate the centre of each input value inside the
	// bins, so a value maps by interpolating its bin's cumulative counts.
	valueBin  []uint16
	valueFrac []float32
	constant  bool
}

func buildCLAHEModel(ctx context.Context, src []uint16, width, height int, cfg claheConfig) (*claheModel, error) {
	m := &claheModel{
		width:    width,
		height:   height,
		cfg:      cfg,
		colStart: tileStarts(width, cfg.tilesX),
		rowStart: tileStarts(height, cfg.tilesY),
		maps:     make([][]float32, cfg.tilesX*cfg.tilesY),
	}
	levels := cfg.maxValue + 1
	m.valueBin = make([]uint16, levels)
	m.valueFrac = make([]float32, levels)
	for v := 0; v < levels; v++ {
		pos := (float64(v) + 0.5) * float64(cfg.bins) / float64(levels)
		bin := int(pos)
		if bin >= cfg.bins {
			bin = cfg.bins - 1
		}
		m.valueBin[v] = uint16(bin)
		m.valueFrac[v] = float32(pos - float64(bin))
	}

	tiles := len(m.maps)
	type extrema struct{ lo, hi int }
	ranges := make([]extrema, tiles)
	err := runParallel(ctx, tiles, cfg.workers, func() func(int) {
		hist := make([]uint32, cfg.bins)
		return func(t int) {
			tx, ty := t%cfg.tilesX, t/cfg.tilesX
			x0, x1 := m.colStart[tx], m.colStart[tx+1]
			y0, y1 := m.rowStart[ty], m.rowStart[ty+1]
			clear(hist)
			lo, hi := cfg.maxValue, 0
			for y := y0; y < y1; y++ {
				for _, v := range src[y*width+x0 : y*width+x1] {
					value := min(int(v), cfg.maxValue)
					lo, hi = min(lo, value), max(hi, value)
					hist[m.valueBin[value]]++
				}
			}
			ranges[t] = extrema{lo, hi}
			pixels := (x1 - x0) * (y1 - y0)
			clipHistogram(hist, clipCount(cfg.clipLimit, pixels, cfg.bins))
			m.maps[t] = cumulativeMap(hist, pixels, cfg.maxValue)
		}
	})
	if err != nil {
		return nil, err
	}
	lo, hi := ranges[0].lo, ranges[0].hi
	for _, r := range ranges[1:] {
		lo, hi = min(lo, r.lo), max(hi, r.hi)
	}
	m.constant = lo == hi
	return m, nil
}

// tileStarts splits n pixels into count nearly equal tiles and returns the
// first pixel of each tile followed by n.
func tileStarts(n, count int) []int {
	starts := make([]int, count+1)
	for i := 0; i <= count; i++ {
		starts[i] = i * n / count
	}
	return starts
}

// clipCount converts the normalized clip limit into a per-bin count. It is
// never below ceil(pixels/bins), so the clipped bins can always hold every
// pixel again after redistribution.
func clipCount(clipLimit float64, pixels, bins int) uint32 {
	floor := (pixels + bins - 1) / bins
	if math.IsInf(clipLimit, 1) {
		return uint32(pixels)
	}
	limit := int(clipLimit * float64(pixels) / float64(bins))
	if limit < floor {
		limit = floor
	}
	if limit > pixels {
		limit = pixels
	}
	return uint32(limit)
}

// clipHistogram clips every bin at limit and redistributes the clipped
// counts evenly over the bins still below the limit, repeating while whole
// increments remain and spreading the final remainder one count per bin at a
// regular stride. The total count is preserved whenever
// limit*len(hist) >= total, which clipCount guarantees.
func clipHistogram(hist []uint32, limit uint32) {
	var excess uint64
	for i, h := range hist {
		if h > limit {
			excess += uint64(h - limit)
			hist[i] = limit
		}
	}
	for excess > 0 {
		var below uint64
		for _, h := range hist {
			if h < limit {
				below++
			}
		}
		if below == 0 {
			return
		}
		increment := excess / below
		if increment == 0 {
			step := below / excess
			var seen uint64
			for i, h := range hist {
				if h >= limit {
					continue
				}
				if seen%step == 0 && excess > 0 {
					hist[i]++
					excess--
				}
				seen++
			}
			continue
		}
		for i, h := range hist {
			if h >= limit {
				continue
			}
			add := min(increment, uint64(limit-h))
			hist[i] = h + uint32(add)
			excess -= add
		}
	}
}

// cumulativeMap turns a clipped histogram into cumulative output levels:
// entry k is the fraction of the tile's pixels in bins below k, scaled to
// [0, maxValue].
func cumulativeMap(hist []uint32, pixels, maxValue int) []float32 {
	out := make([]float32, len(hist)+1)
	scale := float64(maxValue) / float64(pixels)
	var sum uint64
	for i, h := range hist {
		sum += uint64(h)
		out[i+1] = float32(math.Min(float64(sum)*scale, float64(maxValue)))
	}
	return out
}

// axisWeights locates every coordinate between the two nearest tile centres
// along one axis: lo/hi are tile indices and w the weight of hi. Coordinates
// before the first centre or after the last one use that tile alone, as in
// Zuiderveld's border and corner regions.
func axisWeights(starts []int) (lo, hi []int, w []float32) {
	tiles := len(starts) - 1
	size := starts[tiles]
	centres := make([]float64, tiles)
	for i := range centres {
		centres[i] = float64(starts[i]+starts[i+1]-1) / 2
	}
	lo = make([]int, size)
	hi = make([]int, size)
	w = make([]float32, size)
	t := 0
	for p := 0; p < size; p++ {
		pos := float64(p)
		for t+1 < tiles && pos >= centres[t+1] {
			t++
		}
		switch {
		case pos <= centres[0]:
			lo[p], hi[p] = 0, 0
		case t+1 >= tiles:
			lo[p], hi[p] = tiles-1, tiles-1
		default:
			lo[p], hi[p] = t, t+1
			w[p] = float32((pos - centres[t]) / (centres[t+1] - centres[t]))
		}
	}
	return lo, hi, w
}

// apply maps every pixel through the bilinear blend of its four nearest tile
// mappings. Each row is processed in runs of columns that share the same pair
// of tiles, so the inner loop only indexes four mapping tables.
func (m *claheModel) apply(ctx context.Context, dst, src []uint16) error {
	colLo, colHi, colW := axisWeights(m.colStart)
	rowLo, rowHi, rowW := axisWeights(m.rowStart)
	type run struct{ x0, x1, lo, hi int }
	var runs []run
	for x := 0; x < m.width; {
		end := x + 1
		for end < m.width && colLo[end] == colLo[x] && colHi[end] == colHi[x] {
			end++
		}
		runs = append(runs, run{x, end, colLo[x], colHi[x]})
		x = end
	}
	const rowsPerChunk = 16
	chunks := (m.height + rowsPerChunk - 1) / rowsPerChunk
	maxValue := float32(m.cfg.maxValue)
	tilesX := m.cfg.tilesX
	return runParallel(ctx, chunks, m.cfg.workers, func() func(int) {
		return func(chunk int) {
			yEnd := min((chunk+1)*rowsPerChunk, m.height)
			for y := chunk * rowsPerChunk; y < yEnd; y++ {
				wy := rowW[y]
				top, bottom := rowLo[y]*tilesX, rowHi[y]*tilesX
				in := src[y*m.width : (y+1)*m.width]
				out := dst[y*m.width : (y+1)*m.width]
				for _, r := range runs {
					m00, m01 := m.maps[top+r.lo], m.maps[top+r.hi]
					m10, m11 := m.maps[bottom+r.lo], m.maps[bottom+r.hi]
					for x := r.x0; x < r.x1; x++ {
						value := int(in[x])
						if value > m.cfg.maxValue {
							value = m.cfg.maxValue
						}
						b := int(m.valueBin[value])
						f := m.valueFrac[value]
						v00 := m00[b] + f*(m00[b+1]-m00[b])
						v01 := m01[b] + f*(m01[b+1]-m01[b])
						v10 := m10[b] + f*(m10[b+1]-m10[b])
						v11 := m11[b] + f*(m11[b+1]-m11[b])
						wx := colW[x]
						topValue := v00 + wx*(v01-v00)
						bottomValue := v10 + wx*(v11-v10)
						v := topValue + wy*(bottomValue-topValue)
						if v < 0 {
							v = 0
						} else if v > maxValue {
							v = maxValue
						}
						out[x] = uint16(v + 0.5)
					}
				}
			}
		}
	})
}

// runParallel runs items 0..count-1 on the calling goroutine plus helpers
// from the module's shared concurrency budget, up to workers in total, that
// claim items from a shared cursor. newWorker is called once per worker for
// its scratch state. Cancellation stops workers between items; ctx.Err() is
// returned after every started worker has stopped.
func runParallel(ctx context.Context, count, workers int, newWorker func() func(int)) error {
	return parallel.Run(ctx, count, workers, 1, newWorker)
}
