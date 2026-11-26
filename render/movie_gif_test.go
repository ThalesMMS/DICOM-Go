package render

import (
	"bytes"
	"image"
	"image/color"
	"image/color/palette"
	"image/draw"
	"image/gif"
	"math"
	"math/rand"
	"testing"
)

// testVRShadedFrame is a synthetic VR-like frame: a Lambert-plus-specular
// shaded sphere whose tint drifts from bone to soft tissue across the surface,
// on a two-axis backdrop gradient, lit from a direction that rotates with phase
// (0..1). It has thousands of distinct colours along long, slow ramps, the
// content that bands under a fixed palette.
func testVRShadedFrame(w, h int, phase float64) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	lx, ly, lz := math.Cos(2*math.Pi*phase)*0.6, -0.4, 0.0
	lz = math.Sqrt(1 - lx*lx - ly*ly)
	cx, cy, rad := float64(w)/2, float64(h)/2, 0.45*math.Min(float64(w), float64(h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			fx, fy := float64(x)/float64(w-1), float64(y)/float64(h-1)
			nx, ny := (float64(x)+0.5-cx)/rad, (float64(y)+0.5-cy)/rad
			c := [3]float64{10 + 30*fy, 12 + 20*fx, 30 + 40*fx*fy}
			if d := nx*nx + ny*ny; d < 1 {
				nz := math.Sqrt(1 - d)
				lambert := math.Max(0, nx*lx+ny*ly+nz*lz)
				// Blinn-Phong highlight with the viewer on +z.
				hx, hy, hz := lx, ly, lz+1
				hn := math.Sqrt(hx*hx + hy*hy + hz*hz)
				spec := math.Pow(math.Max(0, (nx*hx+ny*hy+nz*hz)/hn), 24)
				tint := 0.5 + 0.5*ny // bone (top) to soft tissue (bottom)
				base := [3]float64{236 - 20*tint, 214 - 90*tint, 178 - 100*tint}
				shade := 0.1 + 0.8*lambert
				for i := range c {
					c[i] = base[i]*shade + 60*spec
				}
			}
			img.SetRGBA(x, y, color.RGBA{R: roundByte(c[0]), G: roundByte(c[1]), B: roundByte(c[2]), A: 255})
		}
	}
	return img
}

// testWarmRamp is a two-axis VR-tone gradient: luminance rises from near black
// to bone white along x while the tint drifts from warm to cool along y.
func testWarmRamp(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			t := float64(x) / float64(w-1)
			u := float64(y) / float64(max(h-1, 1))
			img.SetRGBA(x, y, color.RGBA{
				R: roundByte(20 + (216-60*u)*t),
				G: roundByte(12 + (202-20*u)*t),
				B: roundByte(8 + (170+60*u)*t),
				A: 255,
			})
		}
	}
	return img
}

type bandingStats struct {
	meanAbs    float64 // per-pixel mean absolute error per channel
	lowpass    float64 // mean absolute error per channel after an 8x8 box blur
	longestRun int     // longest run of one output colour along the middle row
	rowColors  int     // distinct output colours along the middle row
}

func measureBanding(src *image.RGBA, got image.Image) bandingStats {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	const k = 8
	// Integral images of the source and output per channel.
	integral := func(img image.Image) [3][]float64 {
		var out [3][]float64
		for c := range out {
			out[c] = make([]float64, (w+1)*(h+1))
		}
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				r, g, bb, _ := img.At(x, y).RGBA()
				v := [3]float64{float64(r >> 8), float64(g >> 8), float64(bb >> 8)}
				for c := 0; c < 3; c++ {
					out[c][(y+1)*(w+1)+x+1] = v[c] + out[c][y*(w+1)+x+1] + out[c][(y+1)*(w+1)+x] - out[c][y*(w+1)+x]
				}
			}
		}
		return out
	}
	is, ig := integral(src), integral(got)
	var stats bandingStats
	var abs float64
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r1, g1, b1, _ := src.At(x, y).RGBA()
			r2, g2, b2, _ := got.At(x, y).RGBA()
			abs += math.Abs(float64(r1>>8)-float64(r2>>8)) + math.Abs(float64(g1>>8)-float64(g2>>8)) + math.Abs(float64(b1>>8)-float64(b2>>8))
		}
	}
	stats.meanAbs = abs / float64(3*w*h)
	var lp float64
	var n int
	for y := 0; y+k <= h; y++ {
		for x := 0; x+k <= w; x++ {
			for c := 0; c < 3; c++ {
				box := func(t []float64) float64 {
					return t[(y+k)*(w+1)+x+k] - t[y*(w+1)+x+k] - t[(y+k)*(w+1)+x] + t[y*(w+1)+x]
				}
				lp += math.Abs(box(is[c])-box(ig[c])) / (k * k)
				n++
			}
		}
	}
	stats.lowpass = lp / float64(n)
	seen := map[color.RGBA]bool{}
	run := 0
	var prev color.RGBA
	for x := 0; x < w; x++ {
		r, g, bb, _ := got.At(x, h/2).RGBA()
		c := color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(bb >> 8), A: 255}
		seen[c] = true
		if x > 0 && c == prev {
			run++
		} else {
			run = 1
		}
		if run > stats.longestRun {
			stats.longestRun = run
		}
		prev = c
	}
	stats.rowColors = len(seen)
	return stats
}

// encodePlan9GIF reproduces the previous EncodeGIF: nearest-colour mapping to
// the fixed Plan9 palette without dithering.
func encodePlan9GIF(t *testing.T, frames []image.Image) []byte {
	t.Helper()
	g := &gif.GIF{}
	for _, frame := range frames {
		b := frame.Bounds()
		p := image.NewPaletted(b, palette.Plan9)
		draw.Draw(p, b, frame, b.Min, draw.Src)
		g.Image = append(g.Image, p)
		g.Delay = append(g.Delay, 10)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decodeGIF(t *testing.T, data []byte) *gif.GIF {
	t.Helper()
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("image/gif cannot decode output: %v", err)
	}
	return g
}

// TestAdaptiveGIFReducesBanding compares the adaptive median-cut +
// Floyd–Steinberg path against the previous Plan9 quantization on smooth
// synthetic gradients, after a real encode/decode round trip through
// image/gif. The 8x8 low-pass error is the banding measure: it is what remains
// visible once the eye integrates dither noise.
func TestAdaptiveGIFReducesBanding(t *testing.T) {
	cases := []struct {
		name  string
		image *image.RGBA
	}{
		{"two-axis warm ramp 512x128", testWarmRamp(512, 128)},
		{"shaded sphere 256x256", testVRShadedFrame(256, 256, 0.15)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frames := []image.Image{tc.image}
			old := measureBanding(tc.image, decodeGIF(t, encodePlan9GIF(t, frames)).Image[0])

			var buf bytes.Buffer
			if err := EncodeAdaptiveGIF(&buf, frames, GIFOptions{DelayCs: 10}); err != nil {
				t.Fatal(err)
			}
			adaptive := measureBanding(tc.image, decodeGIF(t, buf.Bytes()).Image[0])

			buf.Reset()
			if err := EncodeAdaptiveGIF(&buf, frames, GIFOptions{DelayCs: 10, NoDither: true}); err != nil {
				t.Fatal(err)
			}
			undithered := measureBanding(tc.image, decodeGIF(t, buf.Bytes()).Image[0])

			t.Logf("Plan9 (old):              mean|e|=%.2f lowpass8=%.2f longestRun=%d rowColors=%d", old.meanAbs, old.lowpass, old.longestRun, old.rowColors)
			t.Logf("median cut, no dither:    mean|e|=%.2f lowpass8=%.2f longestRun=%d rowColors=%d", undithered.meanAbs, undithered.lowpass, undithered.longestRun, undithered.rowColors)
			t.Logf("median cut + FS (new):    mean|e|=%.2f lowpass8=%.2f longestRun=%d rowColors=%d", adaptive.meanAbs, adaptive.lowpass, adaptive.longestRun, adaptive.rowColors)

			if adaptive.lowpass*4 > old.lowpass {
				t.Fatalf("low-pass (banding) error %.2f is not at least 4x below Plan9's %.2f", adaptive.lowpass, old.lowpass)
			}
			if adaptive.meanAbs >= old.meanAbs {
				t.Fatalf("mean error %.2f is not below Plan9's %.2f", adaptive.meanAbs, old.meanAbs)
			}
			if adaptive.longestRun*2 > old.longestRun {
				t.Fatalf("longest flat band %d px is not at most half of Plan9's %d px", adaptive.longestRun, old.longestRun)
			}
			if adaptive.lowpass > undithered.lowpass+0.05 {
				t.Fatalf("dithering raised low-pass error: %.2f > %.2f", adaptive.lowpass, undithered.lowpass)
			}
		})
	}
}

func TestAdaptiveGIFGlobalPaletteMovie(t *testing.T) {
	frames := make([]image.Image, 6)
	for i := range frames {
		frames[i] = testVRShadedFrame(96, 80, float64(i)/6)
	}
	var buf bytes.Buffer
	if err := EncodeGIF(&buf, frames, 7); err != nil {
		t.Fatalf("EncodeGIF: %v", err)
	}
	g := decodeGIF(t, buf.Bytes())
	if len(g.Image) != len(frames) || g.LoopCount != 0 {
		t.Fatalf("decoded %d frames loop %d, want %d looping forever", len(g.Image), g.LoopCount, len(frames))
	}
	global, ok := g.Config.ColorModel.(color.Palette)
	if !ok || len(global) < 128 {
		t.Fatalf("global colour table = %T len %d, want an adaptive palette", g.Config.ColorModel, len(global))
	}
	for i, frame := range g.Image {
		if g.Delay[i] != 7 {
			t.Fatalf("frame %d delay %d, want 7", i, g.Delay[i])
		}
		if len(frame.Palette) != len(global) {
			t.Fatalf("frame %d carries its own palette; global mode should share one", i)
		}
		for j := range global {
			if frame.Palette[j] != global[j] {
				t.Fatalf("frame %d palette entry %d differs from the global table", i, j)
			}
		}
		if s := measureBanding(frames[i].(*image.RGBA), frame); s.lowpass > 1.5 {
			t.Fatalf("frame %d low-pass error %.2f, want <= 1.5", i, s.lowpass)
		}
	}

	buf.Reset()
	if err := EncodeAdaptiveGIF(&buf, frames, GIFOptions{DelayCs: 5, LoopCount: -1, Palette: GIFPalettePerFrame, Colors: 64}); err != nil {
		t.Fatal(err)
	}
	g = decodeGIF(t, buf.Bytes())
	if len(g.Image) != len(frames) {
		t.Fatalf("per-frame mode decoded %d frames", len(g.Image))
	}
	for i, frame := range g.Image {
		if len(frame.Palette) > 64 {
			t.Fatalf("frame %d palette has %d colours, want <= 64", i, len(frame.Palette))
		}
	}
}

func TestMedianCutPaletteExactForFewColours(t *testing.T) {
	gray := image.NewGray(image.Rect(0, 0, 256, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 256; x++ {
			gray.SetGray(x, y, color.Gray{Y: uint8(x)})
		}
	}
	p := MedianCutPalette([]image.Image{gray}, 256)
	if len(p) != 256 {
		t.Fatalf("palette has %d entries, want the 256 exact grays", len(p))
	}
	q := QuantizeFloydSteinberg(gray, p)
	for y := 0; y < 4; y++ {
		for x := 0; x < 256; x++ {
			if r, _, _, _ := q.At(x, y).RGBA(); uint8(r>>8) != uint8(x) {
				t.Fatalf("pixel (%d,%d) = %d, want exact %d", x, y, r>>8, x)
			}
		}
	}
	if got := MedianCutPalette([]image.Image{testWarmRamp(300, 2)}, 16); len(got) != 16 {
		t.Fatalf("16-colour cut returned %d entries", len(got))
	}
}

func TestPaletteSearchMatchesBruteForce(t *testing.T) {
	rng := rand.New(rand.NewSource(1070))
	p := make(color.Palette, 200)
	for i := range p {
		p[i] = color.RGBA{R: uint8(rng.Intn(256)), G: uint8(rng.Intn(256)), B: uint8(rng.Intn(256)), A: 255}
	}
	s := newPaletteSearch(p)
	for i := 0; i < 20000; i++ {
		r, g, b := int32(rng.Intn(256)), int32(rng.Intn(256)), int32(rng.Intn(256))
		got := p[s.index[s.nearest(r, g, b)]].(color.RGBA)
		best := int32(1 << 30)
		for _, c := range p {
			cc := c.(color.RGBA)
			dr, dg, db := int32(cc.R)-r, int32(cc.G)-g, int32(cc.B)-b
			if d := dr*dr + dg*dg + db*db; d < best {
				best = d
			}
		}
		dr, dg, db := int32(got.R)-r, int32(got.G)-g, int32(got.B)-b
		if d := dr*dr + dg*dg + db*db; d != best {
			t.Fatalf("nearest(%d,%d,%d) distance %d, brute force %d", r, g, b, d, best)
		}
	}
}

// BenchmarkEncodeGIFOrbit measures a 12-frame 256x256 VR-like orbit.
func BenchmarkEncodeGIFOrbit(b *testing.B) {
	frames := make([]image.Image, 12)
	for i := range frames {
		frames[i] = testVRShadedFrame(256, 256, float64(i)/12)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var buf bytes.Buffer
		if err := EncodeGIF(&buf, frames, 7); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEncodeMJPEGAVIOrbit measures the same orbit as Motion-JPEG AVI.
func BenchmarkEncodeMJPEGAVIOrbit(b *testing.B) {
	frames := make([]image.Image, 12)
	for i := range frames {
		frames[i] = testVRShadedFrame(256, 256, float64(i)/12)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var buf bytes.Buffer
		if err := EncodeMJPEGAVI(&buf, frames, AVIOptions{FPS: 15}); err != nil {
			b.Fatal(err)
		}
	}
}

func TestEncodeAdaptiveGIFRejectsInvalidInput(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeAdaptiveGIF(&buf, nil, GIFOptions{}); err == nil {
		t.Fatal("no frames: want error")
	}
	if err := EncodeAdaptiveGIF(&buf, []image.Image{nil}, GIFOptions{}); err == nil {
		t.Fatal("nil frame: want error")
	}
	if err := EncodeAdaptiveGIF(&buf, []image.Image{testWarmRamp(4, 4)}, GIFOptions{Colors: 300}); err == nil {
		t.Fatal("300 colours: want error")
	}
	// Frames with a non-zero origin are normalized.
	sub := testWarmRamp(40, 20).SubImage(image.Rect(10, 5, 30, 15))
	if err := EncodeAdaptiveGIF(&buf, []image.Image{sub}, GIFOptions{}); err != nil {
		t.Fatalf("sub-image frame: %v", err)
	}
	if b := decodeGIF(t, buf.Bytes()).Image[0].Bounds(); b != image.Rect(0, 0, 20, 10) {
		t.Fatalf("sub-image frame bounds %v, want 20x10 at origin", b)
	}
}
