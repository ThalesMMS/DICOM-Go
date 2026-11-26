package render_test

import (
	"bytes"
	"context"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/render/qualification"
)

func TestLungOpacityNormalsGolden(t *testing.T) {
	for _, id := range []qualification.ClinicalFixtureID{qualification.ClinicalFixtureAirStructure, qualification.ClinicalFixtureContrastTubes} {
		t.Run(string(id), func(t *testing.T) {
			fixture, err := qualification.NewClinicalFixture(id, 1)
			if err != nil {
				t.Fatal(err)
			}
			vol, err := fixture.BuildRenderVolume()
			if err != nil {
				t.Fatal(err)
			}
			defer vol.Close()
			cam := render.NewVRCamera(vol.BoundingRadiusMM())
			cam.Projection, cam.Yaw, cam.Pitch = render.VRProjectionParallel, 0.4, 0.2
			preset := render.VRPreset{Mode: render.VRModeDVR, TF: render.NewLungAdvancedVRTransferFunction(), LightingMaterialSet: true, LightingMaterial: render.BasicClinicalVRLightingMaterial()}
			window := render.WindowLevel{Center: -537.80029296875, Width: 408.62545776367188}
			quality := render.DefaultVRQuality(96, 96)
			quality.Jitter, quality.Scattering = false, false
			var images [][]byte
			for _, enabled := range []bool{false, true} {
				preset.OpacityNormals = enabled
				img, err := render.RenderVRContext(context.Background(), vol, cam, preset, window, true, nil, quality)
				if err != nil {
					t.Fatal(err)
				}
				var encoded bytes.Buffer
				if err := png.Encode(&encoded, img); err != nil {
					t.Fatal(err)
				}
				images = append(images, encoded.Bytes())
				mode := "scalar"
				if enabled {
					mode = "opacity"
				}
				path := filepath.Join("testdata", "vr_lung_normals_"+string(id)+"_"+mode+".png")
				if os.Getenv("DICOMGO_UPDATE_GOLDEN") == "1" {
					if err := os.WriteFile(path, encoded.Bytes(), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				golden, err := png.Decode(bytes.NewReader(data))
				if err != nil {
					t.Fatal(err)
				}
				if golden.Bounds() != img.Bounds() {
					t.Fatal("golden dimensions differ")
				}
				var visible int
				for y := 0; y < 96; y++ {
					for x := 0; x < 96; x++ {
						gr, gg, gb, _ := golden.At(x, y).RGBA()
						ir, ig, ib, _ := img.At(x, y).RGBA()
						if ir+ig+ib > 0 {
							visible++
						}
						for _, pair := range [][2]uint32{{gr, ir}, {gg, ig}, {gb, ib}} {
							if delta := int(pair[0]) - int(pair[1]); delta < -2*257 || delta > 2*257 {
								t.Fatalf("golden changed at (%d,%d): %v", x, y, pair)
							}
						}
					}
				}
				if visible < 100 {
					t.Fatalf("only %d visible pixels", visible)
				}
			}
			if bytes.Equal(images[0], images[1]) {
				t.Fatal("opacity normals did not change Lung interfaces")
			}
			t.Logf("qualified fixture %s SHA256 %s", id, fixture.PayloadSHA256())
		})
	}
}
