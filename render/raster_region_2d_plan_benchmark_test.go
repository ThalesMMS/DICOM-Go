package render

import (
	"context"
	"image"
	"image/color"
	"sort"
	"testing"
	"time"
)

// BenchmarkRasterViewport2DPreparation4K measures the current complete grayscale
// preparation, including its background row and closure. It deliberately does
// not execute the returned pixel kernel or allocate a new published output.
// Compare it with BenchmarkRasterViewport2DClinical4K before retaining plans.
func BenchmarkRasterViewport2DPreparation4K(b *testing.B) {
	gray := image.NewGray(image.Rect(0, 0, 512, 512))
	source := RasterRegion2D{Image: gray, Grid: gray.Bounds(), FullSize: gray.Bounds().Size()}
	request := RasterViewport2DRequest{Width: 3140, Height: 1820, Affine: [6]float64{3.5, 0, 0, 3.5, 674, 14}, Sampling: RasterBilinear2D, Background: color.NRGBA{A: 255}}
	out := image.NewNRGBA(image.Rect(0, 0, request.Width, request.Height))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, _, err := rasterGrayAxisRows(context.Background(), gray, source, request, out, 12.25, 1); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRasterViewport2DRepeatedGeometry4K projects changing, already
// windowed slices through identical acquired-2D geometry. Each output remains
// independently owned. It excludes decode/windowing and native UI adoption.
func BenchmarkRasterViewport2DRepeatedGeometry4K(b *testing.B) {
	var images [8]*image.Gray
	for n := range images {
		images[n] = image.NewGray(image.Rect(0, 0, 512, 512))
		for i := range images[n].Pix {
			images[n].Pix[i] = byte(i*13 + n*31)
		}
	}
	source := RasterRegion2D{Grid: images[0].Bounds(), FullSize: images[0].Bounds().Size()}
	request := RasterViewport2DRequest{Width: 3140, Height: 1820, Affine: [6]float64{3.5, 0, 0, 3.5, 674, 14}, Sampling: RasterBilinear2D, Background: color.NRGBA{A: 255}}
	// Bound the quantile workspace even when an external caller selects a long
	// benchmark window. Samples after this prefix still contribute to ns/op.
	samples := make([]time.Duration, 0, min(b.N, 1000))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		source.Image = images[i%len(images)]
		started := time.Now()
		if _, err := RasterViewport2D(context.Background(), source, request); err != nil {
			b.Fatal(err)
		}
		if len(samples) < cap(samples) {
			samples = append(samples, time.Since(started))
		}
	}
	b.StopTimer()
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	if len(samples) > 0 {
		b.ReportMetric(float64(samples[(len(samples)*95+99)/100-1]), "p95-ns/frame")
	}
}
