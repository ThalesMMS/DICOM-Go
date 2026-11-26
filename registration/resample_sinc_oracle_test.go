package registration

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

type sincOracleGrid struct {
	Size      [3]int     `json:"size"`
	Origin    [3]float64 `json:"origin"`
	Spacing   [3]float64 `json:"spacing"`
	Direction Matrix3    `json:"direction"`
	Data      []float32  `json:"data"`
}

func (g sincOracleGrid) grid() Grid {
	return Grid{Size: g.Size, Origin: render.Vec3{X: g.Origin[0], Y: g.Origin[1], Z: g.Origin[2]},
		Spacing: render.Vec3{X: g.Spacing[0], Y: g.Spacing[1], Z: g.Spacing[2]}, Direction: g.Direction}
}

func TestResampleWindowedSincPublicITKOracles(t *testing.T) {
	data, err := os.ReadFile("testdata/windowed_sinc/oracles.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures struct {
		FormatVersion    int    `json:"format_version"`
		SimpleITKVersion string `json:"simpleitk_version"`
		ITKVersion       string `json:"itk_version"`
		Cases            []struct {
			Name     string         `json:"name"`
			Window   string         `json:"window"`
			Source   sincOracleGrid `json:"source"`
			Target   sincOracleGrid `json:"target"`
			Matrix   Matrix3        `json:"matrix"`
			Offset   [3]float64     `json:"offset"`
			Padding  float32        `json:"padding"`
			Expected []float64      `json:"expected"`
			Inside   []bool         `json:"inside"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	if fixtures.FormatVersion != 1 || fixtures.SimpleITKVersion != "2.5.3" || !strings.HasPrefix(fixtures.ITKVersion, "5.4") || len(fixtures.Cases) != 38 {
		t.Fatalf("unexpected oracle provenance: version=%d SimpleITK=%s ITK=%s cases=%d", fixtures.FormatVersion, fixtures.SimpleITKVersion, fixtures.ITKVersion, len(fixtures.Cases))
	}
	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			g := fixture.Source.grid()
			moving, err := NewImage(g.Size, fixture.Source.Data, g.Origin, g.Spacing, g.Direction)
			if err != nil {
				t.Fatal(err)
			}
			mapping := func(p render.Vec3) (render.Vec3, bool) {
				return fixture.Matrix.MulVec(p).Add(render.Vec3{X: fixture.Offset[0], Y: fixture.Offset[1], Z: fixture.Offset[2]}), true
			}
			window := InterpolateLanczosWindowedSinc
			if fixture.Window == "Welch" {
				window = InterpolateWelchWindowedSinc
			} else if fixture.Window != "Lanczos" {
				t.Fatal("unknown oracle window")
			}
			out, err := Resample(context.Background(), moving, fixture.Target.grid(), mapping, ResampleOptions{Interpolator: window, Padding: fixture.Padding})
			if err != nil {
				t.Fatal(err)
			}
			if !out.Image.Grid().SameAs(fixture.Target.grid(), 1e-12) || len(out.Image.Data()) != len(fixture.Expected) || len(out.Inside) != len(fixture.Inside) {
				t.Fatal("output grid/length differs from oracle")
			}
			outside := 0
			var maximum float64
			for i, reference := range fixture.Expected {
				expected := float32(reference)
				ulp := math.Max(math.Abs(float64(math.Nextafter32(expected, float32(math.Inf(1))))-float64(expected)), math.Abs(float64(math.Nextafter32(expected, float32(math.Inf(-1))))-float64(expected)))
				difference := math.Abs(float64(out.Image.Data()[i]) - float64(expected))
				maximum = math.Max(maximum, difference)
				if difference > math.Max(2e-5, 4*ulp) || !finite(float64(out.Image.Data()[i])) {
					t.Fatalf("sample %d=%g expected %g (oracle64 %g), error=%g limit=%g", i, out.Image.Data()[i], expected, reference, difference, math.Max(2e-5, 4*ulp))
				}
				if out.Inside[i] != fixture.Inside[i] {
					t.Fatalf("sample %d Inside=%v expected %v", i, out.Inside[i], fixture.Inside[i])
				}
				if !fixture.Inside[i] {
					outside++
					if out.Image.Data()[i] != fixture.Padding {
						t.Fatal("outside sample differs from padding")
					}
				}
			}
			if out.Outside != outside {
				t.Fatalf("Outside=%d expected %d", out.Outside, outside)
			}
			if strings.HasPrefix(fixture.Name, "step_ringing") {
				ringing := false
				for _, value := range out.Image.Data() {
					ringing = ringing || value < 0 || value > 100
				}
				if !ringing {
					t.Fatal("windowed-sinc overshoot was clipped")
				}
			}
			t.Logf("public ITK max absolute error after float32 rounding = %.9g", maximum)
		})
	}
}
