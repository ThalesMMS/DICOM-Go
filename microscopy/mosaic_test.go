package microscopy

import (
	"context"
	"errors"
	"fmt"
	"image"
	"math"
	"reflect"
	"strconv"
	"sync"
	"testing"

	"github.com/ThalesMMS/dicom-go/registration"
	"github.com/ThalesMMS/dicom-go/render"
)

func mosaicTexture(x, y int) float32 {
	v := uint32(x+137)*0x9e3779b9 ^ uint32(y+53)*0x85ebca6b
	v ^= v >> 16
	v *= 0x7feb352d
	v ^= v >> 15
	return float32(v % 251)
}
func mosaicImage(t *testing.T, data []float32, origin render.Vec3) *registration.Image {
	t.Helper()
	im, e := registration.NewImage([3]int{32, 28, 1}, data, origin, render.Vec3{X: .04, Y: .06, Z: 1}, registration.Identity3())
	if e != nil {
		t.Fatal(e)
	}
	return im
}
func mosaicFixture(t *testing.T) []MosaicTile {
	t.Helper()
	var tiles []MosaicTile
	errors := [][2]int{{0, 0}, {1, -1}, {-1, 1}, {1, 1}}
	for gy := 0; gy < 2; gy++ {
		for gx := 0; gx < 2; gx++ {
			i := gy*2 + gx
			data := make([]float32, 32*28)
			for y := 0; y < 28; y++ {
				for x := 0; x < 32; x++ {
					data[y*32+x] = mosaicTexture(x+gx*24, y+gy*20)
				}
			}
			origin := render.Vec3{X: 7.25 + float64(gx*24+errors[i][0])*.04, Y: -3 + float64(gy*20+errors[i][1])*.06, Z: 8}
			tiles = append(tiles, MosaicTile{ID: fmt.Sprint("tile-", i), Grid: image.Pt(gx, gy), Image: mosaicImage(t, data, origin)})
		}
	}
	return tiles
}
func mosaicTestOptions() MosaicOptions {
	return MosaicOptions{MaxCorrectionMM: .25, MinOverlap: 64, MinOverlapFraction: .1}
}

// Independent exhaustive two-pass spatial Pearson correlation. It does not use
// FFTs, the production scorer/graph or the known true-placement oracle.
func mosaicSpatialBest(a, b *registration.Image) ([3]int, float64) {
	best := -2.
	var shift [3]int
	for sy := -27; sy <= 27; sy++ {
		for sx := -31; sx <= 31; sx++ {
			physical := b.Origin().Sub(a.Origin()).Add(render.Vec3{X: float64(sx) * .04, Y: float64(sy) * .06})
			if math.Abs(physical.X) > .25 || math.Abs(physical.Y) > .25 {
				continue
			}
			n := 0
			af, bf := 0., 0.
			for y := 0; y < 28; y++ {
				for x := 0; x < 32; x++ {
					j, k := x+sx, y+sy
					if j < 0 || k < 0 || j >= 32 || k >= 28 {
						continue
					}
					n++
					af += float64(a.At(x, y, 0))
					bf += float64(b.At(j, k, 0))
				}
			}
			if n < 90 {
				continue
			}
			af /= float64(n)
			bf /= float64(n)
			aa, bb, ab := 0., 0., 0.
			for y := 0; y < 28; y++ {
				for x := 0; x < 32; x++ {
					j, k := x+sx, y+sy
					if j < 0 || k < 0 || j >= 32 || k >= 28 {
						continue
					}
					da, db := float64(a.At(x, y, 0))-af, float64(b.At(j, k, 0))-bf
					aa += da * da
					bb += db * db
					ab += da * db
				}
			}
			if aa == 0 || bb == 0 {
				continue
			}
			score := ab / math.Sqrt(aa*bb)
			if score > best {
				best = score
				shift = [3]int{sx, sy, 0}
			}
		}
	}
	return shift, best
}

func TestMosaicKnownGlobalTranslationsAndIndependentSeams(t *testing.T) {
	tiles := mosaicFixture(t)
	before := make([][]float32, len(tiles))
	for i, v := range tiles {
		before[i] = append([]float32(nil), v.Image.Data()...)
	}
	m, e := RegisterMosaic(context.Background(), tiles, mosaicTestOptions())
	if e != nil {
		t.Fatal(e)
	}
	if !m.Conclusive || m.Components != 1 || m.CycleRank != 1 || len(m.Links) != 4 || m.MaxCycleClosureMM > 1e-8 || m.MaxResidualMM > 1e-8 {
		t.Fatalf("global graph not qualified: %+v", m)
	}
	for i, p := range m.Tiles {
		want := render.Vec3{X: 7.25 + float64(p.Grid.X*24)*.04, Y: -3 + float64(p.Grid.Y*20)*.06, Z: 8}
		if mosaicLength(p.Origin.Sub(want)) > 1e-8 {
			t.Fatal("physical placement", i, p.Origin, want)
		}
		if !reflect.DeepEqual(before[i], tiles[i].Image.Data()) {
			t.Fatal("source pixels changed")
		}
	}
	measuredCycles := 0
	for _, link := range m.Links {
		if link.CycleClosureMeasured {
			measuredCycles++
		}
		if !link.ResidualMeasured {
			t.Fatal("connected fit omitted residual availability")
		}
		shift, score := mosaicSpatialBest(tiles[link.A].Image, tiles[link.B].Image)
		if !link.Accepted || link.Registration.IndexShift != shift || math.Abs(link.Registration.Score-score) > 1e-8 {
			t.Fatal("independent spatial comparison", link, shift, score)
		}
	}
	if measuredCycles != m.CycleRank {
		t.Fatal("fundamental cycle availability", measuredCycles, m.CycleRank)
	}
	out, e := ComposeMosaicRegion(context.Background(), tiles, m, MosaicRegionOptions{Region: image.Rect(0, 0, 56, 48)})
	if e != nil {
		t.Fatal(e)
	}
	maxError := 0.
	overlaps := 0
	for y := 0; y < 48; y++ {
		for x := 0; x < 56; x++ {
			id := y*56 + x
			if out.Coverage[id] == 0 {
				t.Fatal("invented gap", x, y)
			}
			if out.Coverage[id] > 1 {
				overlaps++
			}
			maxError = math.Max(maxError, math.Abs(float64(out.Pixels[id]-mosaicTexture(x, y))))
		}
	}
	// Prespecified physical error 1e-8 mm, intensity/seam error 1e-4 units.
	if maxError > 1e-4 || overlaps == 0 {
		t.Fatal("independent whole-canvas seam oracle", maxError, overlaps)
	}
	small, e := ComposeMosaicRegion(context.Background(), tiles, m, MosaicRegionOptions{Region: image.Rect(22, 18, 28, 24), MaxPixels: 36, MaxBytes: 64 << 10})
	if e != nil {
		t.Fatal(e)
	}
	if len(small.Pixels) != 36 || small.ReservedBytes >= out.ReservedBytes {
		t.Fatal("regional allocation not bounded")
	}
	t.Logf("physical residual=%g cycle=%g seam=%g overlapPixels=%d FFTwork=%d scratch=%d regionalBytes=%d", m.MaxResidualMM, m.MaxCycleClosureMM, maxError, overlaps, m.FFTWork, m.ScratchBytes, small.ReservedBytes)
}

func TestMosaicBoundedOverviewUsesPhysicalPixelCentres(t *testing.T) {
	tiles := mosaicFixture(t)
	m, err := RegisterMosaic(context.Background(), tiles, mosaicTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	out, err := ComposeMosaicRegion(context.Background(), tiles, m, MosaicRegionOptions{Region: image.Rect(0, 0, 56, 48), OutputSize: image.Pt(4, 3), MaxPixels: 12})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Pixels) != 12 || out.OutputSize != image.Pt(4, 3) || math.Abs(out.Spacing.X-.56) > 1e-12 || math.Abs(out.Spacing.Y-.96) > 1e-12 || math.Abs(out.Origin.X-(7.25+6.5*.04)) > 1e-12 || math.Abs(out.Origin.Y-(-3+7.5*.06)) > 1e-12 {
		t.Fatal("overview geometry", out)
	}
	for y := 0; y < 3; y++ {
		for x := 0; x < 4; x++ {
			// Independent continuous whole-texture oracle at exact half-pixel
			// coordinates; four original samples contribute with equal weights.
			i, j := 6+14*x, 7+16*y
			want := (float64(mosaicTexture(i, j)) + float64(mosaicTexture(i+1, j)) + float64(mosaicTexture(i, j+1)) + float64(mosaicTexture(i+1, j+1))) / 4
			if out.Coverage[y*4+x] == 0 || math.Abs(float64(out.Pixels[y*4+x])-want) > 1e-4 {
				t.Fatal("overview sample", x, y, out.Pixels[y*4+x], want)
			}
		}
	}
}

func TestMosaicRegionNativeSpanAdmission(t *testing.T) {
	tiles := mosaicFixture(t)
	m, err := RegisterMosaic(context.Background(), tiles, mosaicTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	r, err := ComposeMosaicRegion(context.Background(), tiles, m, MosaicRegionOptions{Region: image.Rect(-math.MaxInt32, -math.MaxInt32, math.MaxInt32, math.MaxInt32), OutputSize: image.Pt(2, 2), MaxPixels: 4})
	if strconv.IntSize == 32 {
		if !errors.Is(err, ErrMosaicLimit) || r != nil {
			t.Fatal("native rectangle subtraction overflow accepted", r, err)
		}
		return
	}
	if err != nil || r == nil || len(r.Pixels) != 4 || r.Spacing.X <= 0 || r.Spacing.Y <= 0 {
		t.Fatal("bounded wide overview geometry", r, err)
	}
}

func TestMosaicAmbiguityOverlapIlluminationAndDisconnected(t *testing.T) {
	for _, kind := range []string{"periodic", "missing", "illumination", "disconnected"} {
		t.Run(kind, func(t *testing.T) {
			tiles := mosaicFixture(t)[:2]
			switch kind {
			case "periodic":
				for _, tile := range tiles {
					for i := range tile.Image.Data() {
						tile.Image.Data()[i] = float32((i % 32) % 2)
					}
				}
			case "missing":
				tiles[1].Mask = mosaicImage(t, make([]float32, 32*28), tiles[1].Image.Origin())
			case "illumination":
				for i := range tiles[1].Image.Data() {
					tiles[1].Image.Data()[i] = tiles[1].Image.Data()[i]*2 + 30
				}
			case "disconnected":
				tiles[1].Grid = image.Pt(3, 0)
			}
			m, e := RegisterMosaic(context.Background(), tiles, mosaicTestOptions())
			if e != nil {
				t.Fatal(e)
			}
			if m.Conclusive || m.Components != 2 {
				t.Fatal("unsupported link silently adopted", m)
			}
			if kind != "disconnected" && (len(m.Links) != 1 || m.Links[0].Accepted || m.Links[0].Reason == "") {
				t.Fatal("missing rejection diagnostic", m.Links)
			}
			if kind != "disconnected" && m.FFTWork == 0 {
				t.Fatal("inconclusive FFT escaped aggregate work accounting")
			}
			if out, e := ComposeMosaicRegion(context.Background(), tiles, m, MosaicRegionOptions{Region: image.Rect(0, 0, 4, 4)}); !errors.Is(e, ErrMosaicInconclusive) || out != nil {
				t.Fatal("incomplete mosaic published", out, e)
			}
		})
	}
}

func TestMosaicGlobalCycleConflictCannotBecomeConclusive(t *testing.T) {
	r := &mosaicRun{ctx: context.Background(), o: MosaicOptions{MaxWork: 10000, MaxCorrectionMM: 100, MaxCycleErrorMM: .01}}
	m := &MosaicLayout{AnchorOrigin: render.Vec3{}, Tiles: make([]MosaicPlacement, 4), Links: []MosaicLink{{A: 0, B: 1, Accepted: true, Offset: render.Vec3{X: 1}}, {A: 0, B: 2, Accepted: true, Offset: render.Vec3{Y: 1}}, {A: 1, B: 3, Accepted: true, Offset: render.Vec3{Y: 1}}, {A: 2, B: 3, Accepted: true, Offset: render.Vec3{X: 1.2}}}}
	if e := r.solve(m, 0); e != nil {
		t.Fatal(e)
	}
	if m.Conclusive || m.CycleRank != 1 || math.Abs(m.MaxCycleClosureMM-.2) > 1e-12 || math.Abs(m.MaxResidualMM-.05) > 1e-12 {
		t.Fatal("local links hid global cycle inconsistency", m)
	}
}

func TestMosaicBindingsQuotasCancellationAndConcurrency(t *testing.T) {
	tiles := mosaicFixture(t)
	ctx := context.Background()
	o := mosaicTestOptions()
	m, e := RegisterMosaic(ctx, tiles, o)
	if e != nil {
		t.Fatal(e)
	}
	for _, mutate := range []func(*MosaicOptions){func(o *MosaicOptions) { o.MaxTiles = 2 }, func(o *MosaicOptions) { o.MaxSamples = 10 }, func(o *MosaicOptions) { o.MaxBytes = 100 }, func(o *MosaicOptions) { o.MaxFFTWork = 1 }, func(o *MosaicOptions) { o.MaxWork = 10 }} {
		limited := o
		mutate(&limited)
		if got, e := RegisterMosaic(ctx, tiles, limited); !errors.Is(e, ErrMosaicLimit) || got != nil {
			t.Fatal("quota published", got, e)
		}
	}
	for _, limited := range []MosaicRegionOptions{{Region: image.Rect(0, 0, 10, 10), MaxPixels: 10}, {Region: image.Rect(0, 0, 10, 10), MaxBytes: 10}, {Region: image.Rect(0, 0, 10, 10), MaxInputSamples: 10}, {Region: image.Rect(0, 0, 10, 10), MaxWork: 10}} {
		if got, e := ComposeMosaicRegion(ctx, tiles, m, limited); !errors.Is(e, ErrMosaicLimit) || got != nil {
			t.Fatal("region quota published", got, e)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if got, e := RegisterMosaic(canceled, tiles, o); !errors.Is(e, context.Canceled) || got != nil {
		t.Fatal(got, e)
	}
	if got, e := ComposeMosaicRegion(canceled, tiles, m, MosaicRegionOptions{Region: image.Rect(0, 0, 4, 4)}); !errors.Is(e, context.Canceled) || got != nil {
		t.Fatal(got, e)
	}
	copyLayout := *m
	copyLayout.Conclusive = false
	if got, e := ComposeMosaicRegion(ctx, tiles, &copyLayout, MosaicRegionOptions{Region: image.Rect(0, 0, 4, 4)}); !errors.Is(e, ErrMosaic) || got != nil {
		t.Fatal("changed layout adopted", got, e)
	}
	value := tiles[0].Image.Data()[0]
	tiles[0].Image.Data()[0]++
	if got, e := ComposeMosaicRegion(ctx, tiles, m, MosaicRegionOptions{Region: image.Rect(0, 0, 4, 4)}); !errors.Is(e, ErrMosaic) || got != nil {
		t.Fatal("changed pixels adopted", got, e)
	}
	tiles[0].Image.Data()[0] = value
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, e := RegisterMosaic(ctx, tiles, o)
			if e != nil {
				t.Error(e)
				return
			}
			if !reflect.DeepEqual(got, m) {
				t.Error("nondeterministic shared inputs")
			}
		}()
	}
	wg.Wait()
}

func TestMosaicMaskCoverageGeometryAndMidPhaseCancellation(t *testing.T) {
	tiles := mosaicFixture(t)
	mask := make([]float32, 32*28)
	for i := range mask {
		mask[i] = 1
	}
	mask[3*32+3] = 0
	tiles[0].Mask = mosaicImage(t, mask, tiles[0].Image.Origin())
	m, err := RegisterMosaic(context.Background(), tiles, mosaicTestOptions())
	if err != nil || !m.Conclusive {
		t.Fatal(m, err)
	}
	region := MosaicRegionOptions{Region: image.Rect(-2, -2, 8, 8), Background: -123}
	out, err := ComposeMosaicRegion(context.Background(), tiles, m, region)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []image.Point{image.Pt(-1, -1), image.Pt(3, 3)} {
		id := (p.Y+2)*10 + p.X + 2
		if out.Coverage[id] != 0 || out.Pixels[id] != -123 {
			t.Fatal("missing data filled", p, out.Coverage[id], out.Pixels[id])
		}
	}
	if out.Coverage[4*10+4] != 1 || out.Pixels[4*10+4] != mosaicTexture(2, 2) {
		t.Fatal("valid neighbour lost")
	}
	probe := &mosaicCancelContext{Context: context.Background(), done: make(chan struct{})}
	if _, err := RegisterMosaic(probe, tiles, mosaicTestOptions()); err != nil {
		t.Fatal(err)
	}
	for _, at := range []int{probe.calls / 4, probe.calls * 3 / 4, probe.calls - 1} {
		ctx := &mosaicCancelContext{Context: context.Background(), at: at, done: make(chan struct{})}
		if got, err := RegisterMosaic(ctx, tiles, mosaicTestOptions()); !errors.Is(err, context.Canceled) || got != nil {
			t.Fatal("registration cancellation published", at, got, err)
		}
	}
	probe = &mosaicCancelContext{Context: context.Background(), done: make(chan struct{})}
	if _, err := ComposeMosaicRegion(probe, tiles, m, region); err != nil {
		t.Fatal(err)
	}
	for _, at := range []int{probe.calls / 4, probe.calls * 3 / 4, probe.calls - 1} {
		ctx := &mosaicCancelContext{Context: context.Background(), at: at, done: make(chan struct{})}
		if got, err := ComposeMosaicRegion(ctx, tiles, m, region); !errors.Is(err, context.Canceled) || got != nil {
			t.Fatal("composition cancellation published", at, got, err)
		}
	}
	for _, mutate := range []func([]MosaicTile){
		func(ts []MosaicTile) { ts[1].ID = ts[0].ID },
		func(ts []MosaicTile) { ts[1].Grid = ts[0].Grid },
		func(ts []MosaicTile) { ts[1].Image = nil },
		func(ts []MosaicTile) {
			im, _ := registration.NewImage([3]int{2, 2, 1}, make([]float32, 4), render.Vec3{}, render.Vec3{X: 1, Y: 1, Z: 1}, registration.Identity3())
			ts[1].Image = im
		},
		func(ts []MosaicTile) { ts[1].Mask = mosaicImage(t, make([]float32, 32*28), render.Vec3{}) },
	} {
		bad := append([]MosaicTile(nil), tiles...)
		mutate(bad)
		if got, err := RegisterMosaic(context.Background(), bad, mosaicTestOptions()); !errors.Is(err, ErrMosaic) || got != nil {
			t.Fatal("invalid geometry published", got, err)
		}
	}
}

type mosaicCancelContext struct {
	context.Context
	mu        sync.Mutex
	calls, at int
	done      chan struct{}
	canceled  bool
}

func (c *mosaicCancelContext) Done() <-chan struct{} { return c.done }
func (c *mosaicCancelContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.at > 0 && c.calls >= c.at && !c.canceled {
		c.canceled = true
		close(c.done)
	}
	if c.canceled {
		return context.Canceled
	}
	return nil
}
