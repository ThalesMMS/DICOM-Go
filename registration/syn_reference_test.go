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

func TestSyNITKANTsKnownFieldOracle(t *testing.T) {
	python, ants := os.Getenv("DICOMGO_NCC_ITK_PYTHON"), os.Getenv("DICOMGO_SYN_ANTS")
	if python == "" || ants == "" {
		t.Skip("set DICOMGO_NCC_ITK_PYTHON and DICOMGO_SYN_ANTS to pinned native oracles")
	}
	const n = 16
	const amplitude = .8
	f, m := synPhantomPair(t, n, amplitude)
	var landmarks [][3]float64
	for _, z := range []float64{5, 8, 10} {
		for _, y := range []float64{5, 8, 10} {
			for _, x := range []float64{5, 8, 10} {
				landmarks = append(landmarks, [3]float64{x, y, z})
			}
		}
	}
	request := struct {
		neighborhoodITKCase
		Landmarks      [][3]float64 `json:"landmarks"`
		Iterations     int          `json:"iterations"`
		Step           float64      `json:"step"`
		UpdateVariance float64      `json:"update_variance"`
		TotalVariance  float64      `json:"total_variance"`
	}{neighborhoodITKCase: neighborhoodITKCase{Name: "known-smooth-x-map", Size: f.size, Spacing: [3]float64{1, 1, 1}, Direction: Identity3(), Fixed: f.data, Moving: m.data, Radius: [3]int{2, 2, 2}}, Landmarks: landmarks, Iterations: 60, Step: .3, UpdateVariance: .64, TotalVariance: .5}
	input, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, python, "testdata/syn_reference.py", ants)
	command.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("SyN oracles: %v\n%s", err, &stderr)
	}
	var result struct {
		ITKVersion  string `json:"itk_version"`
		ANTsVersion string `json:"ants_version"`
		ITK, ANTs   struct {
			ElapsedSeconds float64 `json:"elapsed_seconds"`
			Landmarks      [][3]float64
			JacobianMin    float64 `json:"jacobian_min"`
			FoldedNodes    int     `json:"folded_nodes"`
		}
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatal(err, string(output))
	}
	if result.ITKVersion != "5.4.5" {
		t.Fatal("oracle identity", string(output))
	}
	t.Logf("ITK %s; %s", result.ITKVersion, result.ANTsVersion)
	for name, r := range map[string]struct {
		ElapsedSeconds float64 `json:"elapsed_seconds"`
		Landmarks      [][3]float64
		JacobianMin    float64 `json:"jacobian_min"`
		FoldedNodes    int     `json:"folded_nodes"`
	}{"ITK": result.ITK, "ANTs": result.ANTs} {
		t.Run(name, func(t *testing.T) {
			if len(r.Landmarks) != len(landmarks) {
				t.Fatal("missing landmarks")
			}
			worst := 0.0
			for i, p := range landmarks {
				want := render.Vec3{X: p[0] + synKnownDisplacement(p[0], p[1], p[2], n, amplitude), Y: p[1], Z: p[2]}
				got := render.Vec3{X: r.Landmarks[i][0], Y: r.Landmarks[i][1], Z: r.Landmarks[i][2]}
				worst = math.Max(worst, got.Sub(want).Length())
			}
			t.Logf("maximum landmark error=%g mm min Jacobian=%g folded=%d elapsed=%g s", worst, r.JacobianMin, r.FoldedNodes, r.ElapsedSeconds)
			if worst > .3 || r.JacobianMin <= 0 || r.FoldedNodes != 0 {
				t.Fatal("independent known-field qualification failed")
			}
		})
	}
}
