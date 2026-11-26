package roi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/roi"
)

type vectorConfidenceOracleFixture struct {
	Name                string  `json:"name"`
	Dimensions          [3]int  `json:"dimensions"`
	Split               int     `json:"split"`
	Seed                [3]int  `json:"seed"`
	InitialRadius       int     `json:"initial_radius"`
	Multiplier          float64 `json:"multiplier"`
	Iterations          []int   `json:"iterations"`
	ChannelCounts       []int   `json:"channel_counts"`
	OracleNormalization string  `json:"oracle_covariance_normalization"`
}

func vectorConfidenceOracleFixtures(t *testing.T) []vectorConfidenceOracleFixture {
	t.Helper()
	data, err := os.ReadFile("testdata/vector_confidence/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Schema   int                             `json:"schema"`
		Fixtures []vectorConfidenceOracleFixture `json:"fixtures"`
	}
	if err := json.Unmarshal(data, &file); err != nil || file.Schema != 1 || len(file.Fixtures) != 2 {
		t.Fatal("fixture contract", err)
	}
	return file.Fixtures
}

func TestVectorConfidenceFixtureContract(t *testing.T) {
	for _, f := range vectorConfidenceOracleFixtures(t) {
		if f.OracleNormalization != "population" || f.InitialRadius != 1 || f.Multiplier != 2.5 || !reflect.DeepEqual(f.Iterations, []int{0, 2}) || !reflect.DeepEqual(f.ChannelCounts, []int{1, 2}) {
			t.Fatal("unexpected fixture", f.Name)
		}
		v := vectorConfidencePhantom(f.Dimensions[0], f.Dimensions[1], f.Dimensions[2], f.Split)
		r, err := roi.GrowVectorConfidenceConnected(nil, v, []roi.RegionGrowSeed{{X: f.Seed[0], Y: f.Seed[1], Slice: f.Seed[2]}}, roi.VectorConfidenceParams{Multiplier: f.Multiplier, InitialRadius: f.InitialRadius}, roi.VectorConfidenceOptions{})
		if err != nil || r.VoxelCount != (f.Split+1)*f.Dimensions[1]*f.Dimensions[2] {
			t.Fatal("fixture expected mask", f.Name, err)
		}
	}
}

type vectorConfidenceBoundedOutput struct {
	bytes.Buffer
	exceeded bool
}

func (b *vectorConfidenceBoundedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 65536 {
		b.exceeded = true
		return 0, fmt.Errorf("oracle output exceeds 64KiB")
	}
	return b.Buffer.Write(p)
}

// Opt-in: build the pinned public C++ oracle separately. No CMake invocation,
// runtime installation, implicit fallback or networking occurs in this test.
func TestVectorConfidenceITKOracle(t *testing.T) {
	if os.Getenv("DICOM_GO_VECTOR_CONFIDENCE_ITK") != "1" {
		t.Skip("opt-in ITK5.4.5 vector-confidence oracle")
	}
	bin := os.Getenv("DICOM_GO_VECTOR_CONFIDENCE_ITK_BIN")
	info, err := os.Stat(bin)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 512<<20 {
		t.Fatal("explicit oracle binary unavailable", err)
	}
	for _, f := range vectorConfidenceOracleFixtures(t) {
		for _, channels := range f.ChannelCounts {
			for _, iterations := range f.Iterations {
				t.Run(fmt.Sprintf("%s/channels%d/iterations%d", f.Name, channels, iterations), func(t *testing.T) {
					dimension := 3
					if f.Dimensions[2] == 1 {
						dimension = 2
					}
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					cmd := exec.CommandContext(ctx, bin, strconv.Itoa(dimension), strconv.Itoa(channels), strconv.Itoa(iterations))
					cmd.WaitDelay = time.Second
					out := &vectorConfidenceBoundedOutput{}
					cmd.Stdout = out
					cmd.Stderr = out
					if err := cmd.Run(); err != nil || out.exceeded {
						t.Fatalf("oracle process: %v, exceeded=%v: %s", err, out.exceeded, out.String())
					}
					var oracle struct {
						Version    string    `json:"version"`
						Indices    []int     `json:"indices"`
						Mean       []float64 `json:"mean"`
						Covariance []float64 `json:"covariance"`
					}
					if err := json.Unmarshal(out.Bytes(), &oracle); err != nil || oracle.Version != "5.4.5" {
						t.Fatal("oracle version/output", err)
					}
					v := vectorConfidencePhantom(f.Dimensions[0], f.Dimensions[1], f.Dimensions[2], f.Split)
					v.Channels = v.Channels[:channels]
					r, err := roi.GrowVectorConfidenceConnected(nil, v, []roi.RegionGrowSeed{{X: f.Seed[0], Y: f.Seed[1], Slice: f.Seed[2]}}, roi.VectorConfidenceParams{Multiplier: f.Multiplier, Iterations: iterations, InitialRadius: f.InitialRadius, CovarianceNormalization: roi.VectorCovariancePopulation}, roi.VectorConfidenceOptions{})
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(oracle.Indices, vectorConfidenceIndices(r)) {
						t.Fatal("exact mask mismatch")
					}
					if len(oracle.Mean) != channels || len(oracle.Covariance) != channels*channels {
						t.Fatal("oracle matrix shape")
					}
					for i := range r.Mean {
						if math.Abs(r.Mean[i]-oracle.Mean[i]) > 1e-12 {
							t.Fatal("mean mismatch", i, r.Mean, oracle.Mean)
						}
					}
					for i := range r.Covariance {
						if math.Abs(r.Covariance[i]-oracle.Covariance[i]) > 1e-12 {
							t.Fatal("covariance mismatch", i, r.Covariance, oracle.Covariance)
						}
					}
				})
			}
		}
	}
}
