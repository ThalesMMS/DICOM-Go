package imagefilter

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestPhaseSymmetry2DIndependentCommittedRidges(t *testing.T) {
	dir := filepath.Join("testdata", "phase1548")
	evidence := filepath.Join(dir, "evidence-windows")
	data, err := os.ReadFile(filepath.Join(evidence, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var entries []struct {
		Name, SHA256 string
		Bytes        int
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 20 {
		t.Fatal("incomplete research artifacts")
	}
	for _, entry := range entries {
		if filepath.Base(entry.Name) != entry.Name {
			t.Fatal("invalid artifact name")
		}
		data, err := os.ReadFile(filepath.Join(evidence, entry.Name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if len(data) != entry.Bytes || hex.EncodeToString(sum[:]) != entry.SHA256 {
			t.Fatalf("artifact changed: %s", entry.Name)
		}
	}
	data, err = os.ReadFile(filepath.Join(dir, "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	planSum := sha256.Sum256(data)
	if hex.EncodeToString(planSum[:]) != phase1548PlanSHA256 {
		t.Fatal("preregistered plan changed")
	}
	var plan phase1548Plan
	if err := json.Unmarshal(data, &plan); err != nil {
		t.Fatal(err)
	}
	readValues := func(name string) []float32 {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(evidence, name))
		if err != nil {
			t.Fatal(err)
		}
		if len(data) != 4*plan.Dims[0]*plan.Dims[1] {
			t.Fatal("invalid image payload")
		}
		values := make([]float32, len(data)/4)
		for i := range values {
			values[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[4*i:]))
		}
		return values
	}
	for index := 0; index < 5; index++ {
		t.Run(fmt.Sprintf("case-%d", index), func(t *testing.T) {
			src := &Volume{Dims: plan.Dims, SpacingMM: plan.Spacing, Data: readValues(fmt.Sprintf("case-%d-source.f32", index))}
			want := readValues(fmt.Sprintf("case-%d-phase.f32", index))
			out, err := PhaseSymmetry2D(nil, src, PhaseSymmetry2DOptions{Wavelet: Wavelet2DOptions{ScalesMM: plan.Scales}, NoiseThreshold: plan.Phase.Noise, AmplitudeFloor: plan.Phase.Floor, Polarity: Phase2DBright})
			if err != nil {
				t.Fatal(err)
			}
			// Saved scores were independently checked with NumPy FFT. No
			// external runtime is needed for the normal regression gate.
			for i, v := range out.Symmetry.Data {
				if math.Abs(float64(v-want[i])) > 2e-6 {
					t.Fatalf("independent score mismatch at %d: %g want %g", i, v, want[i])
				}
			}
		})
	}
}
