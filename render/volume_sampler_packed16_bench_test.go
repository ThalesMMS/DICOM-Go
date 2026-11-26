package render

import (
	"context"
	"crypto/sha256"
	"image"
	"image/color"
	"testing"
)

var packed16Issue1307SampleSink float64
var packed16Issue1307ImageSink image.Image

// Fixture construction and first canonical payload decode are outside timers.
// This file also compiles against the pinned pre-change library unmodified.
func packed16Issue1307BenchVolume(b *testing.B, signed bool) *Volume {
	b.Helper()
	vol, err := BuildVolume(stack16(96, signed, func(int) Rescale { return Rescale{Slope: 0.5, Intercept: -1024} }, func(x, y, z int) int {
		v := x*37 + y*11 + z*5
		if signed {
			return v - 3000
		}
		return v + 12000
	}))
	if err != nil {
		b.Fatal(err)
	}
	reader, err := vol.AcquireReader()
	if err != nil {
		b.Fatal(err)
	}
	reader.Close()
	b.Cleanup(func() { vol.Close() })
	return vol
}

func packed16Issue1307BenchHash(img image.Image) [32]byte {
	data := make([]byte, 0, img.Bounds().Dx()*img.Bounds().Dy())
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
			data = append(data, color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y)
		}
	}
	return sha256.Sum256(data)
}

func BenchmarkPacked16Trilinear1307(b *testing.B) {
	for _, signed := range []bool{true, false} {
		name := "unsigned"
		if signed {
			name = "signed"
		}
		b.Run(name, func(b *testing.B) {
			vol := packed16Issue1307BenchVolume(b, signed)
			reader, err := vol.AcquireReader()
			if err != nil {
				b.Fatal(err)
			}
			defer reader.Close()
			point := Vec3{X: 54.25, Y: 37.625, Z: 18.125}
			v, ok := reader.TrilinearAt(point)
			if !ok {
				b.Fatal("fixture missing")
			}
			b.Logf("fixture value=%.17g liveBytes=%d", v, vol.VolumeStoreStats().LiveBytes)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				packed16Issue1307SampleSink, _ = reader.TrilinearAt(point)
			}
			b.StopTimer()
			b.ReportMetric(float64(vol.VolumeStoreStats().LiveBytes), "volume-B")
		})
	}
	b.Run("float32-unmodified", func(b *testing.B) {
		vol, err := BuildVolume(gradientColumnStack(96, 96, 96))
		if err != nil {
			b.Fatal(err)
		}
		defer vol.Close()
		reader, err := vol.AcquireReader()
		if err != nil {
			b.Fatal(err)
		}
		defer reader.Close()
		point := Vec3{X: 54.25, Y: 37.625, Z: 18.125}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			packed16Issue1307SampleSink, _ = reader.TrilinearAt(point)
		}
		b.StopTimer()
		b.ReportMetric(float64(vol.VolumeStoreStats().LiveBytes), "volume-B")
	})
}

func BenchmarkPacked16Frames1307(b *testing.B) {
	for _, mode := range []string{"oblique256", "slab128x5", "cpr128"} {
		b.Run(mode, func(b *testing.B) {
			vol := packed16Issue1307BenchVolume(b, true)
			plane := obliqueInteriorPlane(vol)
			window := WindowLevel{Center: -2000, Width: 4000}
			path := NewCPRPath([]Vec3{vol.VoxelToPatient(Vec3{X: 47.3, Y: 5.1, Z: 40.2}), vol.VoxelToPatient(Vec3{X: 48.7, Y: 90.4, Z: 49.3})})
			req := CPRRequest{Mode: CPRStraightened, Volume: vol, Path: path, Width: 128, ArcSpacing: 1, CrossSpacing: 0.5, Window: window}
			render := func() (image.Image, error) {
				switch mode {
				case "oblique256":
					return ResliceOblique(vol, plane, 256, 256, window), nil
				case "slab128x5":
					return ResliceObliqueSlab(vol, plane, 128, 128, 5, SlabAverage, window), nil
				default:
					return RenderCPR(context.Background(), req)
				}
			}
			img, err := render()
			if err != nil {
				b.Fatal(err)
			}
			b.Logf("fixture pixelsSHA256=%x dimensions=%dx%d liveBytes=%d", packed16Issue1307BenchHash(img), img.Bounds().Dx(), img.Bounds().Dy(), vol.VolumeStoreStats().LiveBytes)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				packed16Issue1307ImageSink, err = render()
				if err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(vol.VolumeStoreStats().LiveBytes), "volume-B")
		})
	}
}
