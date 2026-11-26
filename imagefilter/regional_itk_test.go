package imagefilter

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"testing"
)

// External public-API output is mandatory for issue qualification; the baseline
// does not install/run ITK. Missing or still-null expected values are not proof.
func TestRegionalITKRequestedRegionOracle(t *testing.T) {
	path := os.Getenv("DICOM_GO_REGIONAL_ITK_FIXTURE")
	if path == "" {
		t.Skip("independent ITK requested-region fixture not configured")
	}
	data, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var corpus struct {
		Version, SourceCommit, BinarySHA256 string
		Cases                               []struct {
			ID            string
			Dims, Radius  [3]int
			Output, Input Region
			Expected      []uint32
		}
	}
	if e = json.Unmarshal(data, &corpus); e != nil {
		t.Fatal(e)
	}
	if corpus.Version == "" || len(corpus.SourceCommit) != 40 || len(corpus.BinarySHA256) != 64 || len(corpus.Cases) != 8 {
		t.Fatal("incomplete public oracle provenance/case inventory")
	}
	seen := map[string]bool{}
	for _, c := range corpus.Cases {
		if c.ID == "" || seen[c.ID] {
			t.Fatal("case identity")
		}
		seen[c.ID] = true
		t.Run(c.ID, func(t *testing.T) {
			spacing := [3]float64{.7, 1.3, 2.1}
			v := NewVolume(c.Dims, spacing)
			for z := 0; z < c.Dims[2]; z++ {
				for y := 0; y < c.Dims[1]; y++ {
					for x := 0; x < c.Dims[0]; x++ {
						value := float32((x*17+y*13+z*11)%19) - 9.5
						if x == 0 && y == 0 && z == 0 {
							value = 50
						}
						if x == c.Dims[0]-1 && y == c.Dims[1]-1 && z == c.Dims[2]-1 {
							value = -50
						}
						v.Data[v.Index(x, y, z)] = value
					}
				}
			}
			var mm [3]float64
			for a, r := range c.Radius {
				mm[a] = float64(r) * spacing[a]
			}
			f := MedianOptions{RadiusMM: mm, Workers: 2}
			plan, e := PlanRegions(f, c.Dims, spacing, c.Output, RegionalOptions{})
			if e != nil {
				t.Fatal(e)
			}
			if plan.Halo != c.Radius || regionalWindow(c.Output, c.Dims, c.Radius) != c.Input {
				t.Fatal("ITK requested input region mismatch")
			}
			n, ok := voxelCount(c.Output.Size)
			if !ok || len(c.Expected) != n {
				t.Fatal("missing complete independently generated expected output")
			}
			out := NewVolume(c.Dims, spacing)
			if _, e = FilterRegions(context.Background(), f, v, c.Dims, spacing, c.Output, out, RegionalOptions{TileSize: [3]int{2, 2, 1}}); e != nil {
				t.Fatal(e)
			}
			i := 0
			for z := 0; z < c.Output.Size[2]; z++ {
				for y := 0; y < c.Output.Size[1]; y++ {
					for x := 0; x < c.Output.Size[0]; x++ {
						if math.Float32bits(out.At(x+c.Output.Origin[0], y+c.Output.Origin[1], z+c.Output.Origin[2])) != c.Expected[i] {
							t.Fatalf("public value mismatch %d", i)
						}
						i++
					}
				}
			}
		})
	}
}
