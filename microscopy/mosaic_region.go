package microscopy

import (
	"context"
	"encoding/hex"
	"fmt"
	"image"
	"math"

	"github.com/ThalesMMS/dicom-go/render"
)

// MosaicRegionOptions limits a requested output rectangle in the anchor's
// sample coordinate grid. Background is a finite value used only at coverage
// zero. Sampling is bilinear with border clamping inside the half-pixel support;
// blending is the arithmetic mean of valid tile contributions, without any
// illumination correction. Missing mask support is never interpolated across.
type MosaicRegionOptions struct {
	Region image.Rectangle
	// Zero uses the region's native size. Otherwise output pixel centres map
	// uniformly across Region; this is point sampling, not area averaging.
	OutputSize                                    image.Point
	Background                                    float32
	MaxPixels, MaxInputSamples, MaxBytes, MaxWork uint64
}

type MosaicRegion struct {
	LayoutID            string
	Region              image.Rectangle
	OutputSize          image.Point
	Origin, Spacing     render.Vec3
	Pixels              []float32
	Coverage            []uint16
	ReservedBytes, Work uint64
}

func mosaicCheckSeal(m *MosaicLayout) error {
	if m == nil || len(m.Tiles) < 2 || len(m.Tiles) > 1024 || len(m.Links) > 2*len(m.Tiles) || len(m.Diagnostics) > len(m.Tiles) || len(m.ID) > 80 || len(m.AnchorID) > 256 {
		return fmt.Errorf("%w: layout metadata", ErrMosaic)
	}
	for _, t := range m.Tiles {
		if len(t.ID) > 256 {
			return fmt.Errorf("%w: tile metadata", ErrMosaic)
		}
	}
	for _, e := range m.Links {
		if len(e.Reason) > 512 {
			return fmt.Errorf("%w: link metadata", ErrMosaic)
		}
	}
	for _, d := range m.Diagnostics {
		if len(d) > 512 {
			return fmt.Errorf("%w: diagnostics", ErrMosaic)
		}
	}
	s, err := mosaicSeal(m)
	if err != nil {
		return err
	}
	if s != m.seal || m.ID != "mosaic-v1:"+hex.EncodeToString(s[:]) {
		return fmt.Errorf("%w: modified/unbound layout", ErrMosaic)
	}
	return nil
}

func mosaicIndex(m *MosaicLayout, p render.Vec3) (float64, float64) {
	d := p.Sub(m.AnchorOrigin)
	a := m.Direction
	return (a[0][0]*d.X + a[1][0]*d.Y + a[2][0]*d.Z) / m.Spacing.X, (a[0][1]*d.X + a[1][1]*d.Y + a[2][1]*d.Z) / m.Spacing.Y
}

// MosaicBounds reports an enclosing rectangle, without allocating that matrix.
// An inconclusive layout can be inspected but still cannot be composed.
func MosaicBounds(m *MosaicLayout) (image.Rectangle, error) {
	if err := mosaicCheckSeal(m); err != nil {
		return image.Rectangle{}, err
	}
	loX, loY, hiX, hiY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, t := range m.Tiles {
		x, y := mosaicIndex(m, t.Origin)
		loX = math.Min(loX, math.Floor(x))
		loY = math.Min(loY, math.Floor(y))
		hiX = math.Max(hiX, math.Ceil(x+float64(t.Size[0])))
		hiY = math.Max(hiY, math.Ceil(y+float64(t.Size[1])))
	}
	for _, v := range []float64{loX, loY, hiX, hiY} {
		if !mosaicFinite(v) || math.Abs(v) > math.MaxInt32 {
			return image.Rectangle{}, fmt.Errorf("%w: canvas index capacity", ErrMosaic)
		}
	}
	if hiX-loX > float64(^uint(0)>>1) || hiY-loY > float64(^uint(0)>>1) {
		return image.Rectangle{}, fmt.Errorf("%w: native canvas dimensions", ErrMosaicLimit)
	}
	return image.Rect(int(loX), int(loY), int(hiX), int(hiY)), nil
}

// ComposeMosaicRegion verifies source/derivation bindings and composes only the
// requested region. It refuses an inconclusive or modified layout, never
// invents pixels for holes, and publishes nil on cancellation or any failure.
// Inputs remain borrowed and stable until return. Coverage is a count, not a
// confidence map. Output does not claim a DICOM object or write source metadata.
func ComposeMosaicRegion(ctx context.Context, tiles []MosaicTile, m *MosaicLayout, o MosaicRegionOptions) (*MosaicRegion, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if o.MaxPixels == 0 {
		o.MaxPixels = 4 << 20
	}
	if o.MaxInputSamples == 0 {
		o.MaxInputSamples = 8 << 20
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = 64 << 20
	}
	if o.MaxWork == 0 {
		o.MaxWork = 64 << 20
	}
	if !mosaicFinite(float64(o.Background)) || o.Region.Empty() || o.MaxInputSamples > math.MaxInt32 {
		return nil, fmt.Errorf("%w: region options", ErrMosaic)
	}
	// Bound dimensions before subtraction/multiplication on every native int.
	for _, v := range []int{o.Region.Min.X, o.Region.Min.Y, o.Region.Max.X, o.Region.Max.Y} {
		if int64(v) < -math.MaxInt32 || int64(v) > math.MaxInt32 {
			return nil, fmt.Errorf("%w: region index capacity", ErrMosaic)
		}
	}
	// Endpoints may each fit an int while their difference does not on a
	// 32-bit host. Reject before image.Rectangle.Size/Dx performs subtraction.
	dx, dy := int64(o.Region.Max.X)-int64(o.Region.Min.X), int64(o.Region.Max.Y)-int64(o.Region.Min.Y)
	if dx > int64(^uint(0)>>1) || dy > int64(^uint(0)>>1) {
		return nil, fmt.Errorf("%w: native region dimensions", ErrMosaicLimit)
	}
	if o.OutputSize == (image.Point{}) {
		o.OutputSize = o.Region.Size()
	}
	if o.OutputSize.X < 1 || o.OutputSize.Y < 1 || int64(o.OutputSize.X) > math.MaxInt32 || int64(o.OutputSize.Y) > math.MaxInt32 {
		return nil, fmt.Errorf("%w: output size", ErrMosaic)
	}
	w, h := uint64(o.OutputSize.X), uint64(o.OutputSize.Y)
	if w > o.MaxPixels/h {
		return nil, fmt.Errorf("%w: region pixels", ErrMosaicLimit)
	}
	pixels := w * h
	if m == nil || len(tiles) != len(m.Tiles) || len(tiles) > 1024 {
		return nil, fmt.Errorf("%w: source count", ErrMosaic)
	}
	metadata := uint64(len(tiles))*8192 + 512
	if metadata > o.MaxBytes || pixels > (o.MaxBytes-metadata)/14 || pixels > uint64(^uint(0)>>1) {
		return nil, fmt.Errorf("%w: regional payload/scratch", ErrMosaicLimit)
	}
	if err := mosaicCheckSeal(m); err != nil {
		return nil, err
	}
	if !m.Conclusive {
		return nil, ErrMosaicInconclusive
	}
	r := &mosaicRun{ctx: ctx, o: MosaicOptions{MaxWork: o.MaxWork}}
	var samples uint64
	for i, t := range tiles {
		if t.Image == nil || t.ID != m.Tiles[i].ID || t.Grid != m.Tiles[i].Grid || t.Image.Size() != m.Tiles[i].Size || t.Image.Origin() != m.Tiles[i].SourceOrigin || t.Image.Spacing() != m.Spacing || t.Image.Direction() != m.Direction {
			return nil, fmt.Errorf("%w: changed source geometry/identity", ErrMosaic)
		}
		n := uint64(len(t.Image.Data()))
		if n > o.MaxInputSamples-samples {
			return nil, fmt.Errorf("%w: composition input samples", ErrMosaicLimit)
		}
		samples += n
		if t.Mask != nil && (t.Mask.Size() != t.Image.Size() || t.Mask.Origin() != t.Image.Origin() || t.Mask.Spacing() != m.Spacing || t.Mask.Direction() != m.Direction || len(t.Mask.Data()) != len(t.Image.Data())) {
			return nil, fmt.Errorf("%w: changed mask geometry", ErrMosaic)
		}
		digest, err := r.sourceDigest(t)
		if err != nil {
			return nil, err
		}
		if digest != m.Tiles[i].SourceDigest {
			return nil, fmt.Errorf("%w: changed source pixels/mask", ErrMosaic)
		}
	}
	sx, sy := float64(o.Region.Dx())/float64(w), float64(o.Region.Dy())/float64(h)
	out := &MosaicRegion{LayoutID: m.ID, Region: o.Region, OutputSize: o.OutputSize, Spacing: render.Vec3{X: m.Spacing.X * sx, Y: m.Spacing.Y * sy, Z: m.Spacing.Z}, Pixels: make([]float32, int(pixels)), Coverage: make([]uint16, int(pixels)), ReservedBytes: metadata + 14*pixels}
	out.Origin = m.AnchorOrigin.Add(m.Direction.MulVec(render.Vec3{X: (float64(o.Region.Min.X) + (sx-1)/2) * m.Spacing.X, Y: (float64(o.Region.Min.Y) + (sy-1)/2) * m.Spacing.Y}))
	sums := make([]float64, int(pixels))
	for tile, t := range tiles {
		ox, oy := mosaicIndex(m, m.Tiles[tile].Origin)
		for y := 0; y < o.OutputSize.Y; y++ {
			for x := 0; x < o.OutputSize.X; x++ {
				if err := r.step(1); err != nil {
					return nil, err
				}
				px, py := float64(o.Region.Min.X)+(float64(x)+.5)*sx-.5, float64(o.Region.Min.Y)+(float64(y)+.5)*sy-.5
				value, ok := mosaicSample(t, px-ox, py-oy)
				if !ok {
					continue
				}
				id := y*int(w) + x
				sums[id] += value
				out.Coverage[id]++
			}
		}
	}
	for i := range out.Pixels {
		if err := r.step(1); err != nil {
			return nil, err
		}
		if out.Coverage[i] == 0 {
			out.Pixels[i] = o.Background
		} else {
			value := float32(sums[i] / float64(out.Coverage[i]))
			if !mosaicFinite(float64(value)) {
				return nil, fmt.Errorf("%w: composite overflow", ErrMosaic)
			}
			out.Pixels[i] = value
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out.Work = r.work
	return out, nil
}

func mosaicSample(t MosaicTile, x, y float64) (float64, bool) {
	s := t.Image.Size()
	if x < -.5 || y < -.5 || x >= float64(s[0])-.5 || y >= float64(s[1])-.5 {
		return 0, false
	}
	x = math.Max(0, math.Min(float64(s[0]-1), x))
	y = math.Max(0, math.Min(float64(s[1]-1), y))
	x0, y0 := int(math.Floor(x)), int(math.Floor(y))
	x1, y1 := min(x0+1, s[0]-1), min(y0+1, s[1]-1)
	fx, fy := x-float64(x0), y-float64(y0)
	sum := 0.
	for _, p := range []struct {
		x, y int
		w    float64
	}{{x0, y0, (1 - fx) * (1 - fy)}, {x1, y0, fx * (1 - fy)}, {x0, y1, (1 - fx) * fy}, {x1, y1, fx * fy}} {
		if p.w == 0 {
			continue
		}
		if t.Mask != nil && t.Mask.At(p.x, p.y, 0) <= 0 {
			return 0, false
		}
		sum += p.w * float64(t.Image.At(p.x, p.y, 0))
	}
	return sum, true
}
