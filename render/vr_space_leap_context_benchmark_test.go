package render

import (
	"context"
	"testing"
)

func BenchmarkVRContextSpaceLeap(b *testing.B) {
	for _, profile := range []string{"gradient", "tf2d", "labels", "fusion"} {
		b.Run(profile, func(b *testing.B) {
			var volume *Volume
			var err error
			var layer VRFusionLayer
			preset := transparentPreset()
			var clip *VRClip
			window := WindowLevel{Center: 128, Width: 256}
			if profile == "fusion" {
				volume = fusionCTVolume(b)
				layer = fusionLayer(fusionPETVolume(b, 0, 1.4, 2.1, fusionHotSpot))
			} else {
				volume, err = BuildVolume(sphereStack(80, 18))
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() { volume.Close() })
			}
			if profile == "gradient" {
				preset = VRPreset{Mode: VRModeDVR, TF: NewNormalizedVRTransferFunction([]TFColorStop{{HU: 0, R: 1}, {HU: 1, R: 1}}, []TFAlphaStop{{HU: 0, A: .03}, {HU: 1, A: .03}}, 1), GradientOpacity: VRGradientOpacityRamp(5, 40)}
			}
			if profile == "tf2d" {
				table := make([]RGBA, 8*8)
				for y := 4; y < 8; y++ {
					for x := 0; x < 8; x++ {
						table[y*8+x] = RGBA{G: 1, A: .03}
					}
				}
				transfer, err := NewVRTransfer2D(8, 8, 0, 255, 140, table)
				if err != nil {
					b.Fatal(err)
				}
				preset.Transfer2D = transfer
			}
			if profile == "labels" {
				labels := make([]uint16, volume.Cols*volume.Rows*volume.Depth)
				for z := 36; z < 44; z++ {
					for y := 36; y < 44; y++ {
						for x := 36; x < 44; x++ {
							labels[(z*volume.Rows+y)*volume.Cols+x] = 300
						}
					}
				}
				styles := make([]VRLabelStyle, 301)
				styles[300] = labelRed
				mask, err := NewVRLabelMap(volume.Cols, volume.Rows, volume.Depth, labels, styles, false)
				if err != nil {
					b.Fatal(err)
				}
				clip = NewVRClip()
				clip.SetLabels(mask)
			}
			prepared, err := volume.PrepareVRVolume(VRPrefilterNone)
			if err != nil {
				b.Fatal(err)
			}
			camera := NewVRCamera(volume.BoundingRadiusMM())
			camera.FitVolume(volume)
			camera.Rotate(17, -9)
			quality := DefaultVRQuality(112, 96)
			render := func(clip *VRClip) error {
				if profile == "fusion" {
					_, _, err := RenderFusedVRPreparedContext(context.Background(), prepared, layer, camera, preset, window, false, clip, quality)
					return err
				}
				_, err := RenderVRPreparedContext(context.Background(), prepared, camera, preset, window, false, clip, quality)
				return err
			}
			if err := render(clip); err != nil {
				b.Fatal(err)
			} // cache warm-up outside measurement
			for _, full := range []bool{true, false} {
				name := "skip"
				selected := clip
				if full {
					name = "full"
					selected = fullMarch(clip)
				}
				b.Run(name, func(b *testing.B) {
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						if err := render(selected); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}
