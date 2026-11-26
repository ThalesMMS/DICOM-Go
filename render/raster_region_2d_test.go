package render

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"math"
	"math/rand"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

func TestRasterRegion2DGrayRoundingMatchesCanonicalThresholds(t *testing.T) {
	values := []float64{0, math.SmallestNonzeroFloat64, 255, math.Nextafter(255, math.Inf(1))}
	for n := 0; n < 255; n++ {
		tie := float64(n) + 0.5
		values = append(values, math.Nextafter(tie, 0), tie, math.Nextafter(tie, math.Inf(1)))
	}
	for _, value := range values {
		if got, want := rasterRoundOpaqueGray(value), uint8(math.Round(value)); got != want {
			t.Fatalf("round(%0.17g) = %d, want %d", value, got, want)
		}
	}
}

func TestRasterRegion2DGrayBilinearMatchesCanonicalNearTies(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 2, 2))
	for n := 0; n < 255; n++ {
		copy(img.Pix, []byte{byte(n), byte(n + 1), byte(n), byte(n + 1)})
		for _, fx := range []float64{0, .5 - 1e-10, math.Nextafter(.5, 0), .5, math.Nextafter(.5, 1), .5 + 1e-10, 1 - 1e-10} {
			for _, fy := range []float64{0, .01, .3, .5, .99} {
				request := RasterViewport2DRequest{Width: 1, Height: 1, Affine: [6]float64{1, 0, 0, 1, -fx, -fy}, Sampling: RasterBilinear2D}
				source := RasterRegion2D{Image: img, Grid: img.Bounds(), FullSize: img.Bounds().Size()}
				fast, err := RasterViewport2D(context.Background(), source, request)
				if err != nil {
					t.Fatal(err)
				}
				source.Image = rasterCanonicalImage{img}
				canonical, err := RasterViewport2D(context.Background(), source, request)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(fast.Pix, canonical.Pix) {
					t.Fatalf("value=%d fx=%.17g fy=%.17g: fast=%v canonical=%v", n, fx, fy, fast.Pix, canonical.Pix)
				}
			}
		}
	}
}

func TestRasterRegion2DParallelRowsMatchSerialReference(t *testing.T) {
	bounds := image.Rect(7, 9, 79, 65)
	gray := image.NewGray(bounds)
	rgba := image.NewRGBA(bounds)
	nrgba := image.NewNRGBA(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			c := color.NRGBA{R: byte(x * 13), G: byte(y * 11), B: byte(x + y), A: byte(x*7 + y*3)}
			gray.SetGray(x, y, color.Gray{Y: c.R})
			rgba.Set(x, y, c)
			nrgba.SetNRGBA(x, y, c)
		}
	}
	for _, img := range []image.Image{gray, rgba, nrgba} {
		for _, sampling := range []RasterSampling2D{RasterNearest2D, RasterBilinear2D} {
			for _, affine := range [][6]float64{{5, 0, 0, 5, 10, 10}, {-5, 1, 1, 5, 400, 10}} {
				source := RasterRegion2D{Image: img, Grid: image.Rect(0, 0, 72, 56), FullSize: image.Pt(72, 56)}
				request := RasterViewport2DRequest{Width: 384, Height: 288, Affine: affine, Sampling: sampling, Background: color.NRGBA{A: 255}}
				parallel, err := RasterViewport2D(context.Background(), source, request)
				if err != nil {
					b := img.Bounds()
					t.Fatalf("%T %v: %v", img, b, err)
				}
				source.Image = rasterCanonicalImage{img}
				serial, err := RasterViewport2D(context.Background(), source, request)
				if err != nil || !bytes.Equal(parallel.Pix, serial.Pix) {
					t.Fatalf("%T sampling %d affine %v changed samples: %v", img, sampling, affine, err)
				}
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out, err := RasterViewport2D(ctx, RasterRegion2D{Image: gray, Grid: image.Rect(0, 0, 72, 56), FullSize: image.Pt(72, 56)}, RasterViewport2DRequest{Width: 384, Height: 288, Affine: [6]float64{5, 0, 0, 5, 10, 10}}); out != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled parallel raster published")
	}
}

func BenchmarkRasterViewport2DClinical(b *testing.B) {
	benchmarkRasterViewport2DClinical(b, 1)
}

func BenchmarkRasterViewport2DClinical4K(b *testing.B) {
	benchmarkRasterViewport2DClinical(b, 2)
}

func benchmarkRasterViewport2DClinical(b *testing.B, scale int) {
	img := image.NewGray(image.Rect(0, 0, 512, 512))
	for i := range img.Pix {
		img.Pix[i] = byte(i)
	}
	source := RasterRegion2D{Image: img, Grid: img.Bounds(), FullSize: img.Bounds().Size()}
	request := RasterViewport2DRequest{Width: 1570, Height: 910, Affine: [6]float64{1.75, 0, 0, 1.75, 337, 7}, Sampling: RasterBilinear2D, Background: color.NRGBA{A: 255}}
	request.Width *= scale
	request.Height *= scale
	for i := range request.Affine {
		request.Affine[i] *= float64(scale)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := RasterViewport2D(context.Background(), source, request); err != nil {
			b.Fatal(err)
		}
	}
}

func TestRasterRegion2DMatchesOriginalLandmarksWithoutFullSourceAllocation(t *testing.T) {
	full := image.NewGray(image.Rect(0, 0, 24, 20))
	for y := 0; y < 20; y++ {
		for x := 0; x < 24; x++ {
			full.SetGray(x, y, color.Gray{Y: uint8(x + y*7)})
		}
	}
	region := image.Rect(4, 3, 18, 14)
	crop := full.SubImage(region)
	source := RasterRegion2D{Image: crop, Grid: region, FullSize: image.Pt(24, 20)}
	request := RasterViewport2DRequest{Width: 8, Height: 4, Affine: [6]float64{1, 0, 0, 1, -6, -5}, Background: color.NRGBA{A: 255}}
	result, err := RasterViewport2D(context.Background(), source, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Pix) != 8*4*4 {
		t.Fatal("output allocated full source dimensions")
	}
	for y := 0; y < 4; y++ {
		for x := 0; x < 8; x++ {
			want := uint8(x + 6 + (y+5)*7)
			if got := result.NRGBAAt(x, y); got != (color.NRGBA{R: want, G: want, B: want, A: 255}) {
				t.Fatalf("landmark %d,%d: %+v != %d", x, y, got, want)
			}
		}
	}
	request.Affine = [6]float64{0, -1, 1, 0, 14, -6}
	rotated, err := RasterViewport2D(context.Background(), source, request)
	if err != nil {
		t.Fatal(err)
	}
	for y := 0; y < 4; y++ {
		for x := 0; x < 8; x++ {
			sx, sy := y+6, 13-x
			want := uint8(sx + sy*7)
			if got := rotated.NRGBAAt(x, y); got.R != want {
				t.Fatalf("rotation source coordinate %d,%d lost: %+v", sx, sy, got)
			}
		}
	}
}

func TestRasterRegion2DReducedGridOriginAndPremultipliedBilinear(t *testing.T) {
	reduced := image.NewGray(image.Rect(0, 0, 2, 2))
	reduced.SetGray(0, 0, color.Gray{Y: 37})
	reduced.SetGray(1, 0, color.Gray{Y: 81})
	source := RasterRegion2D{Image: reduced, Grid: image.Rect(2, 3, 4, 5), FullSize: image.Pt(16, 16), ReductionLevel: 1}
	out, err := RasterViewport2D(context.Background(), source, RasterViewport2DRequest{Width: 2, Height: 1, Affine: [6]float64{.5, 0, 0, .5, -2, -3}})
	if err != nil || out.NRGBAAt(0, 0).R != 37 || out.NRGBAAt(1, 0).R != 81 {
		t.Fatalf("reduced origin: %v", err)
	}
	pair := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	pair.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 0})
	pair.SetNRGBA(1, 0, color.NRGBA{B: 255, A: 128})
	source = RasterRegion2D{Image: pair, Grid: pair.Bounds(), FullSize: image.Pt(2, 1)}
	out, err = RasterViewport2D(context.Background(), source, RasterViewport2DRequest{Width: 1, Height: 1, Affine: [6]float64{1, 0, 0, 1, -.5, 0}, Sampling: RasterBilinear2D})
	if err != nil {
		t.Fatal(err)
	}
	if got := out.NRGBAAt(0, 0); got != (color.NRGBA{B: 255, A: 64}) {
		t.Fatalf("transparent colour leaked through interpolation: %+v", got)
	}
}

func TestRasterRegion2DRejectsMissingCoverageAndAtomicCancellation(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 2, 2))
	source := RasterRegion2D{Image: img, Grid: image.Rect(1, 1, 3, 3), FullSize: image.Pt(8, 8)}
	request := RasterViewport2DRequest{Width: 4, Height: 4, Affine: [6]float64{1, 0, 0, 1, 0, 0}}
	if out, err := RasterViewport2D(context.Background(), source, request); out != nil || !errors.Is(err, ErrRasterRegion2D) {
		t.Fatal("missing regional samples hidden as background")
	}
	request.MaxBytes = 1
	if out, err := RasterViewport2D(context.Background(), source, request); out != nil || !errors.Is(err, ErrRasterRegion2D) {
		t.Fatal("byte budget ignored")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source.Grid = img.Bounds()
	source.FullSize = image.Pt(2, 2)
	request.Width, request.Height, request.MaxBytes = 2, 2, 64
	request.SourceColor = func(x, y int, c color.NRGBA) color.NRGBA { cancel(); return c }
	if out, err := RasterViewport2D(ctx, source, request); out != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled partial viewport published")
	}
}

type rasterCanonicalImage struct{ image.Image }

func TestRasterRegion2DGrayAxisMatchesCanonicalCrops(t *testing.T) {
	rng := rand.New(rand.NewSource(91377))
	for n := 0; n < 240; n++ {
		full := image.Pt(71, 57)
		reduction := n % 4
		grid := pixeldata.ReducedRegion(image.Rectangle{Max: full}, reduction)
		if n%3 == 0 {
			grid = image.Rect(2, 1, grid.Max.X-2, grid.Max.Y-1)
		}
		// A subimage exercises both nonzero bounds and padded row strides.
		backing := image.NewGray(image.Rect(-7, 9, grid.Dx()+12, grid.Dy()+24))
		img := backing.SubImage(image.Rect(-3, 12, grid.Dx()-3, grid.Dy()+12)).(*image.Gray)
		for i := range backing.Pix {
			backing.Pix[i] = uint8(rng.Intn(256))
		}
		sx, sy := .25+rng.Float64()*7, .25+rng.Float64()*7
		if n%2 == 0 {
			sx = -sx
		}
		if n%5 == 0 {
			sy = -sy
		}
		request := RasterViewport2DRequest{Width: 163, Height: 127, Affine: [6]float64{sx, 0, 0, sy, rng.Float64()*180 - 50, rng.Float64()*130 - 40}, Sampling: RasterSampling2D((n / 2) % 2), Background: color.NRGBA{R: 13, G: 97, B: 123, A: 41}}
		if n%7 == 0 {
			request.Width, request.Height = 384, 288
		}
		source := RasterRegion2D{Image: img, Grid: grid, FullSize: full, ReductionLevel: reduction}
		fast, fastErr := RasterViewport2D(context.Background(), source, request)
		source.Image = rasterCanonicalImage{img}
		canonical, canonicalErr := RasterViewport2D(context.Background(), source, request)
		if !errors.Is(fastErr, canonicalErr) || (fast == nil) != (canonical == nil) {
			t.Fatalf("case %d grid=%v affine=%v: fast=%v canonical=%v", n, grid, request.Affine, fastErr, canonicalErr)
		}
		if fast != nil && !bytes.Equal(fast.Pix, canonical.Pix) {
			t.Fatalf("case %d grid=%v affine=%v: samples changed", n, grid, request.Affine)
		}
	}
}

func TestRasterRegion2DColorModelsPreserveCanonicalSamplesAndAlpha(t *testing.T) {
	bounds := image.Rect(7, 9, 10, 11)
	gray := image.NewGray(bounds)
	gray16 := image.NewGray16(bounds)
	rgba := image.NewRGBA(bounds)
	nrgba := image.NewNRGBA(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			gray.SetGray(x, y, color.Gray{Y: uint8(x*13 + y)})
			gray16.SetGray16(x, y, color.Gray16{Y: uint16(x*1234 + y*73)})
			alpha := uint8(255)
			if x == 8 {
				alpha = 113
			}
			if x == 9 {
				alpha = 0
			}
			nrgba.SetNRGBA(x, y, color.NRGBA{R: 33, G: 117, B: 209, A: alpha})
			rgba.Set(x, y, color.NRGBA{R: 33, G: 117, B: 209, A: alpha})
		}
	}
	for _, pixels := range []image.Image{gray, gray16, rgba, nrgba} {
		request := RasterViewport2DRequest{Width: 4, Height: 3, Affine: [6]float64{1.3, 0, 0, 1.2, 0, 0}, Sampling: RasterBilinear2D}
		source := RasterRegion2D{Image: pixels, Grid: image.Rect(0, 0, 3, 2), FullSize: image.Pt(3, 2)}
		fast, err := RasterViewport2D(context.Background(), source, request)
		if err != nil {
			t.Fatal(err)
		}
		source.Image = rasterCanonicalImage{pixels}
		canonical, err := RasterViewport2D(context.Background(), source, request)
		if err != nil {
			t.Fatal(err)
		}
		for y := 0; y < 3; y++ {
			for x := 0; x < 4; x++ {
				if fast.NRGBAAt(x, y) != canonical.NRGBAAt(x, y) {
					t.Fatalf("color model %T changed canonical/alpha sample at%d,%d", pixels, x, y)
				}
			}
		}
	}
}
