package registration

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/render"
)

type neighborhoodITKCase struct {
	Name        string     `json:"name"`
	Size        [3]int     `json:"size"`
	Spacing     [3]float64 `json:"spacing"`
	Origin      [3]float64 `json:"origin"`
	Direction   Matrix3    `json:"direction"`
	Fixed       []float32  `json:"fixed"`
	Moving      []float32  `json:"moving"`
	Radius      [3]int     `json:"radius"`
	Translation [3]float64 `json:"translation"`
}

// Opt-in: invokes a real pinned ITK runtime, not a precomputed reference value.
func TestNeighborhoodCorrelationITKValueOracle(t *testing.T) {
	python := os.Getenv("DICOMGO_NCC_ITK_PYTHON")
	if python == "" {
		t.Skip("set DICOMGO_NCC_ITK_PYTHON to the pinned ITK 5.4.5 / NumPy 2.2.6 interpreter")
	}
	var cases []neighborhoodITKCase
	var expected []float64
	for _, tc := range []struct {
		name   string
		shift  [3]float64
		scaled bool
	}{
		{"identity", [3]float64{}, true}, {"negative-spatial-contrast", [3]float64{}, false}, {"physical-subvoxel", [3]float64{.17, .31, .23}, false},
	} {
		f := neighborhoodTestImage(t, [3]int{7, 6, 5}, render.Vec3{X: .7, Y: 1.1, Z: 1.6}, func(x, y, z int) float32 {
			return float32(math.Sin(.5*float64(x)) + .11*float64(y*y) + .09*float64(z*z) + .013*float64(x*y*z))
		})
		m := neighborhoodTestImage(t, f.size, f.spacing, func(x, y, z int) float32 {
			if tc.scaled {
				return 3*f.At(x, y, z) + 7
			}
			return -2*f.At(x, y, z) + .04*float32(x*y) + 3
		})
		c, err := NewNeighborhoodCorrelation(f, m, MetricDomain{}, NeighborhoodCorrelationOptions{Radius: [3]int{2, 2, 2}})
		if err != nil {
			t.Fatal(err)
		}
		tr := NewTranslation3D()
		_ = tr.SetParameters(tc.shift[:])
		v, err := c.Value(tr)
		if err != nil {
			t.Fatal(err)
		}
		cases = append(cases, neighborhoodITKCase{Name: tc.name, Size: f.size, Spacing: [3]float64{f.spacing.X, f.spacing.Y, f.spacing.Z}, Origin: [3]float64{}, Direction: f.direction, Fixed: f.data, Moving: m.data, Radius: c.radius, Translation: tc.shift})
		expected = append(expected, v)
	}
	input, err := json.Marshal(cases)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, python, "testdata/neighborhood_correlation_itk.py")
	command.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("ITK oracle: %v\n%s", err, &stderr)
	}
	var result struct {
		ITK, Numpy string
		Results    []struct {
			Name         string
			Value        float64
			ValidCenters int `json:"valid_centers"`
		}
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatal(err, string(output))
	}
	if result.ITK != "5.4.5" || result.Numpy != "2.2.6" || len(result.Results) != len(cases) {
		t.Fatal("oracle identity/case count", string(output))
	}
	for i, r := range result.Results {
		t.Run(cases[i].Name, func(t *testing.T) {
			if r.Name != cases[i].Name || r.ValidCenters == 0 || !finite(r.Value) || math.Abs(r.Value-expected[i]) > 2e-6 {
				t.Fatalf("Go=%0.12g ITK=%0.12g valid=%d", expected[i], r.Value, r.ValidCenters)
			}
			t.Logf("Go=%.12g ITK=%.12g valid=%d", expected[i], r.Value, r.ValidCenters)
		})
	}
}
