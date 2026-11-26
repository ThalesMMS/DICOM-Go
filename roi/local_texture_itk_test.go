package roi

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"
)

func TestLocalTextureIndependentITKWindows(t *testing.T) {
	raw, e := os.ReadFile("testdata/local_texture/itk.json")
	if e != nil {
		t.Fatal(e)
	}
	var fixture struct {
		ITK     string
		Seconds float64
		PeakRSS int64 `json:"peak_rss_bytes"`
		Cases   []struct {
			Name        string
			Aggregation TextureAggregation
			Values      []float64
			Mask        []bool
			Features    [][4]*float64
		}
	}
	if e = json.Unmarshal(raw, &fixture); e != nil {
		t.Fatal(e)
	}
	if fixture.ITK != "5.4.6" || len(fixture.Cases) != 8 {
		t.Fatal("unexpected oracle identity")
	}
	maximumError := 0.
	var computeTime time.Duration
	for _, c := range fixture.Cases {
		m := NewVolumeMask(7, 7, 7, [3]float64{.8, 1.3, 2})
		copy(m.Voxels, c.Mask)
		zero := 0.
		for k, f := range []LocalTextureFeature{LocalContrast, LocalEntropy, LocalEnergy, LocalHomogeneity} {
			o := LocalTextureOptions{Feature: f, RadiusMM: [3]float64{.8, 1.3, 2}, BinWidth: 1, LowerBound: &zero, MaxGreyLevels: 8, Aggregation: c.Aggregation}
			started := time.Now()
			r, e := m.LocalTexture(context.Background(), func(x, y, z int) (float64, bool) { return c.Values[(z*7+y)*7+x], true }, nil, o)
			computeTime += time.Since(started)
			if e != nil {
				t.Fatal(e)
			}
			if len(c.Features) != len(r.Values) {
				t.Fatal("oracle size")
			}
			for i, v := range r.Values {
				expected := c.Features[i][k]
				if expected == nil {
					if !math.IsNaN(float64(v)) {
						t.Fatal("ITK validity disagreement", c.Name, i)
					}
				} else {
					delta := math.Abs(float64(v) - *expected)
					maximumError = math.Max(maximumError, delta)
					if delta > 2e-6*math.Max(1, math.Abs(*expected)) {
						t.Fatalf("ITK %s/%d/%s at %d: %g want %g", c.Name, c.Aggregation, f, i, v, *expected)
					}
				}
			}
		}
	}
	t.Logf("ITK %s, 8 authored 7^3 cases, 4 features, max abs error %.9g; producer %.6g s, peak RSS %d bytes", fixture.ITK, maximumError, fixture.Seconds, fixture.PeakRSS)
	t.Logf("Go kernel for the same 8 cases and 4 features: %.6g s (excludes oracle parsing and result comparison)", computeTime.Seconds())
}
