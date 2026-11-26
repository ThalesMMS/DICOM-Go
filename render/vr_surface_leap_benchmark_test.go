package render

import (
	"context"
	"crypto/sha256"
	"fmt"
	"image"
	"image/color"
	"testing"
)

// These bounded synthetic headless cases retain the dense and cold controls.
// Preparation is excluded from warm cases and included in ColdSparse128.
func BenchmarkVRSurfaceSpaceLeap(b *testing.B) {
	for _, fixture := range []struct {
		name           string
		iso            float64
		prepared, cold bool
	}{
		{"WarmSparse128", 128, false, false},
		{"WarmEmpty128", 10000, false, false},
		{"WarmDense128", -1, false, false},
		{"PreparedWarmSparse128", 128, true, false},
		{"ColdSparse128", 128, false, true},
	} {
		b.Run(fixture.name, func(b *testing.B) {
			stack := sphereStack(128, 6)
			quality := DefaultVRQuality(96, 80)
			quality.SpacingFraction = .25
			style := color.NRGBA{R: 220, G: 190, B: 130, A: 255}
			var volume *Volume
			var prepared *VRPreparedVolume
			var camera VRCamera
			var expected [32]byte
			build := func() {
				var err error
				volume, err = BuildVolume(stack)
				if err != nil {
					b.Fatal(err)
				}
				camera = NewVRCamera(volume.BoundingRadiusMM())
				camera.FitVolume(volume)
				camera.Rotate(13, -7)
				if fixture.prepared {
					prepared, err = volume.PrepareVRVolume(VRPrefilterNone)
					if err != nil {
						b.Fatal(err)
					}
				}
			}
			render := func() *image.NRGBA {
				var img image.Image
				var err error
				if fixture.prepared {
					img, err = RenderVRSurfacePreparedContext(context.Background(), prepared, camera, fixture.iso, style, false, nil, quality)
				} else {
					img, err = RenderVRSurfaceContext(context.Background(), volume, camera, fixture.iso, style, false, nil, quality)
				}
				if err != nil {
					b.Fatal(err)
				}
				return img.(*image.NRGBA)
			}
			// A correctness admission frame establishes the exact output; it is
			// outside the warm timing and remains explicit in probe counts.
			build()
			expected = sha256.Sum256(render().Pix)
			if fixture.cold {
				_ = volume.Close()
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if fixture.cold {
					build()
				}
				if actual := sha256.Sum256(render().Pix); actual != expected {
					b.Fatal("headless frame changed within benchmark")
				}
				if fixture.cold {
					_ = volume.Close()
				}
			}
			b.StopTimer()
			if !fixture.cold {
				_ = volume.Close()
			}
			b.Logf("ISOIMAGE case=%s sha256=%s dimensions=96x80 measured_frames=%d admission_frames=1 source=128x128x128 spacing_fraction=0.25", fixture.name, fmt.Sprintf("%x", expected), b.N)
		})
	}
}
