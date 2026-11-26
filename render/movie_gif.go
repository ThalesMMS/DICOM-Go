package render

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"io"
	"sort"
)

// Adaptive-palette GIF output (docs/MOVIE_EXPORT.md). A fixed palette such as
// Plan9 has only a handful of levels along any smooth VR shading ramp, so
// nearest-colour mapping produces visible bands. EncodeAdaptiveGIF instead
// builds a median-cut palette from the movie's own colours and maps pixels
// with serpentine Floyd–Steinberg error diffusion, which trades bands for
// fine, low-amplitude noise.
//
// Palette choice: by default one global palette is built from a bounded sample
// of every frame and stored once as the GIF global colour table. Orbit and
// cine movies keep the same transfer function across frames, so a shared
// palette avoids frame-to-frame palette flicker (dither patterns re-shuffling
// between frames) and keeps files small. GIFPalettePerFrame builds one palette
// per frame (written as local colour tables) for movies whose colours change
// substantially between frames.

// GIFPaletteMode selects how EncodeAdaptiveGIF builds palettes.
type GIFPaletteMode int

const (
	// GIFPaletteGlobal builds one median-cut palette sampled from all frames.
	GIFPaletteGlobal GIFPaletteMode = iota
	// GIFPalettePerFrame builds a median-cut palette for every frame.
	GIFPalettePerFrame
)

const (
	// MaxGIFColors is the largest GIF colour table.
	MaxGIFColors = 256
	// gifPaletteSamples bounds the pixels examined to build one palette.
	gifPaletteSamples = 1 << 19
)

// GIFOptions configures EncodeAdaptiveGIF.
type GIFOptions struct {
	// DelayCs is the per-frame delay in hundredths of a second (minimum 1).
	DelayCs int
	// LoopCount follows image/gif: 0 loops forever, -1 plays once, n>0
	// repeats n extra times.
	LoopCount int
	// Colors bounds the palette size (2..256); zero selects 256.
	Colors int
	// Palette selects one global palette (default) or one per frame.
	Palette GIFPaletteMode
	// NoDither maps each pixel to its nearest palette colour without
	// Floyd–Steinberg error diffusion.
	NoDither bool
}

// EncodeAdaptiveGIF writes frames as an animated GIF with a median-cut palette
// and (unless disabled) Floyd–Steinberg dithering. Frames may differ in size;
// each is stored with its origin at (0, 0).
func EncodeAdaptiveGIF(w io.Writer, frames []image.Image, opts GIFOptions) error {
	if len(frames) == 0 {
		return fmt.Errorf("render: no frames to encode")
	}
	colors := opts.Colors
	if colors == 0 {
		colors = MaxGIFColors
	}
	if colors < 2 || colors > MaxGIFColors {
		return fmt.Errorf("render: GIF palette size %d outside 2..%d", opts.Colors, MaxGIFColors)
	}
	delay := opts.DelayCs
	if delay < 1 {
		delay = 1
	}
	rgba := make([]*image.RGBA, len(frames))
	for i, frame := range frames {
		if frame == nil || frame.Bounds().Empty() {
			return fmt.Errorf("render: empty GIF frame %d", i+1)
		}
		rgba[i] = toOpaqueRGBA(frame)
	}
	var global color.Palette
	if opts.Palette != GIFPalettePerFrame {
		global = medianCutRGBA(rgba, colors)
	}
	g := &gif.GIF{LoopCount: opts.LoopCount}
	if global != nil {
		// An explicit Config makes image/gif write the palette once as the
		// global colour table and omit identical local tables.
		for _, frame := range rgba {
			g.Config.Width = max(g.Config.Width, frame.Bounds().Dx())
			g.Config.Height = max(g.Config.Height, frame.Bounds().Dy())
		}
		g.Config.ColorModel = global
	}
	for _, frame := range rgba {
		p := global
		if p == nil {
			p = medianCutRGBA([]*image.RGBA{frame}, colors)
		}
		g.Image = append(g.Image, quantizeRGBA(frame, p, !opts.NoDither))
		g.Delay = append(g.Delay, delay)
	}
	return gif.EncodeAll(w, g)
}

// MedianCutPalette returns an adaptive palette of at most maxColors entries
// for the given images. When the (sampled) images hold no more distinct
// colours than maxColors the palette is exactly those colours; otherwise the
// colour histogram is split by variance-ordered median cut and each box
// contributes its population-weighted mean colour. Alpha is ignored (colours
// are composited over black).
func MedianCutPalette(images []image.Image, maxColors int) color.Palette {
	rgba := make([]*image.RGBA, 0, len(images))
	for _, img := range images {
		if img != nil && !img.Bounds().Empty() {
			rgba = append(rgba, toOpaqueRGBA(img))
		}
	}
	if maxColors < 1 {
		maxColors = 1
	}
	if maxColors > MaxGIFColors {
		maxColors = MaxGIFColors
	}
	return medianCutRGBA(rgba, maxColors)
}

// QuantizeFloydSteinberg maps img onto palette p with serpentine
// Floyd–Steinberg error diffusion. The result's bounds start at (0, 0).
func QuantizeFloydSteinberg(img image.Image, p color.Palette) *image.Paletted {
	return quantizeRGBA(toOpaqueRGBA(img), p, true)
}

// toOpaqueRGBA returns a zero-origin RGBA copy (or the image itself when it is
// already a zero-origin RGBA).
func toOpaqueRGBA(img image.Image) *image.RGBA {
	if r, ok := img.(*image.RGBA); ok && r.Bounds().Min == (image.Point{}) {
		return r
	}
	b := img.Bounds()
	r := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(r, r.Bounds(), img, b.Min, draw.Src)
	return r
}

type colorCount struct {
	c [3]uint8
	n uint32
}

type cutBox struct {
	lo, hi int // entries[lo:hi]
	score  float64
	axis   int
	mean   [3]float64
}

func medianCutRGBA(images []*image.RGBA, maxColors int) color.Palette {
	entries := sampleHistogram(images)
	if len(entries) == 0 {
		return color.Palette{color.RGBA{A: 0xff}}
	}
	if len(entries) <= maxColors {
		p := make(color.Palette, len(entries))
		for i, e := range entries {
			p[i] = color.RGBA{R: e.c[0], G: e.c[1], B: e.c[2], A: 0xff}
		}
		return p
	}
	boxes := []cutBox{makeCutBox(entries, 0, len(entries))}
	for len(boxes) < maxColors {
		best := -1
		for i, b := range boxes {
			if b.hi-b.lo >= 2 && b.score > 0 && (best < 0 || b.score > boxes[best].score) {
				best = i
			}
		}
		if best < 0 {
			break
		}
		b := boxes[best]
		span := entries[b.lo:b.hi]
		axis := b.axis
		sort.Slice(span, func(i, j int) bool { return span[i].c[axis] < span[j].c[axis] })
		var total uint64
		for _, e := range span {
			total += uint64(e.n)
		}
		var acc uint64
		cut := 1
		for i, e := range span {
			acc += uint64(e.n)
			if acc*2 >= total {
				cut = i + 1
				break
			}
		}
		if cut >= len(span) {
			cut = len(span) - 1
		}
		mid := b.lo + cut
		boxes[best] = makeCutBox(entries, b.lo, mid)
		boxes = append(boxes, makeCutBox(entries, mid, b.hi))
	}
	p := make(color.Palette, len(boxes))
	for i, b := range boxes {
		p[i] = color.RGBA{R: roundByte(b.mean[0]), G: roundByte(b.mean[1]), B: roundByte(b.mean[2]), A: 0xff}
	}
	return p
}

// makeCutBox computes the population-weighted mean, the channel with the
// largest variance (the split axis) and the box's total squared error, which
// orders splits so the worst-represented region is refined first.
func makeCutBox(entries []colorCount, lo, hi int) cutBox {
	var n float64
	var sum, sum2 [3]float64
	for _, e := range entries[lo:hi] {
		w := float64(e.n)
		n += w
		for c := 0; c < 3; c++ {
			v := float64(e.c[c])
			sum[c] += w * v
			sum2[c] += w * v * v
		}
	}
	box := cutBox{lo: lo, hi: hi}
	bestVar := -1.0
	for c := 0; c < 3; c++ {
		box.mean[c] = sum[c] / n
		sse := sum2[c] - sum[c]*sum[c]/n
		if sse < 0 {
			sse = 0
		}
		box.score += sse
		if sse > bestVar {
			bestVar = sse
			box.axis = c
		}
	}
	return box
}

// sampleHistogram counts exact 24-bit colours over a bounded, staggered pixel
// sample of all images, returned in a deterministic order.
func sampleHistogram(images []*image.RGBA) []colorCount {
	var pixels int64
	for _, img := range images {
		b := img.Bounds()
		pixels += int64(b.Dx()) * int64(b.Dy())
	}
	step := 1
	for pixels/int64(step*step) > gifPaletteSamples {
		step++
	}
	counts := make(map[uint32]uint32)
	for _, img := range images {
		b := img.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y += step {
			row := img.Pix[(y-b.Min.Y)*img.Stride:]
			// Stagger columns per row so a stride does not alias with image
			// structure.
			start := (y / step * 7) % step
			for x := start; x < b.Dx(); x += step {
				i := x * 4
				key := uint32(row[i])<<16 | uint32(row[i+1])<<8 | uint32(row[i+2])
				counts[key]++
			}
		}
	}
	entries := make([]colorCount, 0, len(counts))
	for key, n := range counts {
		entries = append(entries, colorCount{c: [3]uint8{uint8(key >> 16), uint8(key >> 8), uint8(key)}, n: n})
	}
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i].c, entries[j].c
		return uint32(a[0])<<16|uint32(a[1])<<8|uint32(a[2]) < uint32(b[0])<<16|uint32(b[1])<<8|uint32(b[2])
	})
	return entries
}

// paletteSearch finds exact nearest palette colours (squared RGB distance) by
// scanning outward from the query's green value in a green-sorted copy of the
// palette and stopping once the green distance alone exceeds the best match.
type paletteSearch struct {
	rgb   [][3]int32
	index []uint8
}

func newPaletteSearch(p color.Palette) *paletteSearch {
	s := &paletteSearch{rgb: make([][3]int32, len(p)), index: make([]uint8, len(p))}
	order := make([]int, len(p))
	for i := range order {
		order[i] = i
	}
	values := make([][3]int32, len(p))
	for i, c := range p {
		r, g, b, _ := c.RGBA()
		values[i] = [3]int32{int32(r >> 8), int32(g >> 8), int32(b >> 8)}
	}
	sort.SliceStable(order, func(i, j int) bool { return values[order[i]][1] < values[order[j]][1] })
	for k, i := range order {
		s.rgb[k] = values[i]
		s.index[k] = uint8(i)
	}
	return s
}

func (s *paletteSearch) nearest(r, g, b int32) int {
	start := sort.Search(len(s.rgb), func(i int) bool { return s.rgb[i][1] >= g })
	best, bestDist := 0, int32(1<<30)
	visit := func(k int) bool {
		c := s.rgb[k]
		dg := c[1] - g
		if dg*dg >= bestDist {
			return false
		}
		dr, db := c[0]-r, c[2]-b
		if d := dr*dr + dg*dg + db*db; d < bestDist {
			best, bestDist = k, d
		}
		return true
	}
	up, down := start, start-1
	for up < len(s.rgb) || down >= 0 {
		if up < len(s.rgb) {
			if visit(up) {
				up++
			} else {
				up = len(s.rgb)
			}
		}
		if down >= 0 {
			if visit(down) {
				down--
			} else {
				down = -1
			}
		}
	}
	return best
}

// quantizeRGBA maps src onto p, optionally with serpentine Floyd–Steinberg
// error diffusion (weights 7/16, 3/16, 5/16, 1/16).
func quantizeRGBA(src *image.RGBA, p color.Palette, dither bool) *image.Paletted {
	dst, _ := quantizeRGBAContext(context.Background(), src, p, dither)
	return dst
}

func quantizeRGBAContext(ctx context.Context, src *image.RGBA, p color.Palette, dither bool) (*image.Paletted, error) {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dst := image.NewPaletted(image.Rect(0, 0, w, h), p)
	search := newPaletteSearch(p)
	if !dither {
		for y := 0; y < h; y++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			row := src.Pix[y*src.Stride:]
			out := dst.Pix[y*dst.Stride:]
			for x := 0; x < w; x++ {
				i := x * 4
				k := search.nearest(int32(row[i]), int32(row[i+1]), int32(row[i+2]))
				out[x] = search.index[k]
			}
		}
		return dst, nil
	}
	// Error rows carry one guard cell on each side so neighbours never need a
	// bounds check.
	cur := make([][3]float32, w+2)
	next := make([][3]float32, w+2)
	for y := 0; y < h; y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		row := src.Pix[y*src.Stride:]
		out := dst.Pix[y*dst.Stride:]
		leftToRight := y%2 == 0
		for n := 0; n < w; n++ {
			x, dir := n, 1
			if !leftToRight {
				x, dir = w-1-n, -1
			}
			i := x * 4
			var want [3]int32
			for c := 0; c < 3; c++ {
				v := float32(row[i+c]) + cur[x+1][c]
				want[c] = clampInt32(int32(v+0.5), 0, 255)
			}
			k := search.nearest(want[0], want[1], want[2])
			out[x] = search.index[k]
			got := search.rgb[k]
			for c := 0; c < 3; c++ {
				e := float32(want[c] - got[c])
				cur[x+1+dir][c] += e * 7 / 16
				next[x+1-dir][c] += e * 3 / 16
				next[x+1][c] += e * 5 / 16
				next[x+1+dir][c] += e * 1 / 16
			}
		}
		cur, next = next, cur
		for i := range next {
			next[i] = [3]float32{}
		}
	}
	return dst, ctx.Err()
}

func clampInt32(v, lo, hi int32) int32 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func roundByte(v float64) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(v + 0.5)
}
