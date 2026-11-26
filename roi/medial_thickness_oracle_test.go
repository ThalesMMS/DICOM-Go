package roi

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"os"
	"testing"
)

func TestMedialThicknessIndependentSkeletonAndDistance(t *testing.T) {
	f, e := os.Open("testdata/medial_thickness/oracle.json.gz")
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	g, e := gzip.NewReader(f)
	if e != nil {
		t.Fatal(e)
	}
	defer g.Close()
	var corpus struct {
		Numpy, Skimage, Simpleitk, Itk string
		GeneratorSHA256                string `json:"generator_sha256"`
		Cases                          []struct {
			Name                 string
			Dimensions           [3]int
			Spacing              [3]float64
			Foreground, Skeleton []int
			Distance             []float64
		}
	}
	if e = json.NewDecoder(io.LimitReader(g, 8<<20)).Decode(&corpus); e != nil {
		t.Fatal(e)
	}
	if corpus.Numpy != "2.3.5" || corpus.Skimage != "0.26.0" || corpus.Simpleitk != "2.5.3" || len(corpus.Cases) != 15 {
		t.Fatal("oracle identity")
	}
	source, e := os.ReadFile("testdata/medial_thickness/generate.py")
	if e != nil {
		t.Fatal(e)
	}
	digest := sha256.Sum256(source)
	if hex.EncodeToString(digest[:]) != corpus.GeneratorSHA256 {
		t.Fatal("oracle generator changed")
	}
	for _, c := range corpus.Cases {
		t.Run(c.Name+fmtSpacing(c.Spacing), func(t *testing.T) {
			m := NewVolumeMask(c.Dimensions[0], c.Dimensions[1], c.Dimensions[2], c.Spacing)
			for _, i := range c.Foreground {
				if i < 0 || i >= len(m.Voxels) {
					t.Fatal("oracle index")
				}
				m.Voxels[i] = true
			}
			s, e := m.Thin(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			expected := make([]bool, len(m.Voxels))
			for _, i := range c.Skeleton {
				if i < 0 || i >= len(expected) {
					t.Fatal("oracle skeleton index")
				}
				expected[i] = true
			}
			difference := 0
			for i, v := range s.Voxels {
				if v != expected[i] {
					difference++
				}
			}
			if difference != 0 {
				t.Fatal("independent Lee skeleton disagreement on the fixed corpus", difference)
			}
			d, e := m.SignedDistanceTransform(context.Background(), SignedDistanceOptions{InsideIsPositive: true})
			if e != nil {
				t.Fatal(e)
			}
			if len(c.Distance) != len(c.Foreground) {
				t.Fatal("oracle distance length")
			}
			maxDelta := 0.
			for j, i := range c.Foreground {
				maxDelta = max(maxDelta, math.Abs(d[i]-c.Distance[j]))
			}
			// SimpleITK emits float32 distances and uses float32 coordinates.
			// Reserve four float32 epsilons over the largest physical span;
			// boundary centres can carry tiny nonzero SDK round-off as well.
			span := float64(c.Dimensions[0]-1) * max(c.Spacing[0], c.Spacing[1], c.Spacing[2])
			if maxDelta > 4*math.Ldexp(1, -23)*span {
				t.Fatal("ITK distance disagreement", maxDelta)
			}
			// Distance is independently qualified at every foreground point,
			// including sites outside either skeleton. Exact independent Lee
			// agreement on this corpus is not an ITK whole-filter parity claim.
			r, e := m.MedialThickness(context.Background(), MedialThicknessOptions{})
			if e != nil {
				t.Fatal(e)
			}
			for _, sample := range r.Samples {
				if math.Abs(sample.ThicknessMM-2*d[sample.Index]) > 1e-12 {
					t.Fatal("composition")
				}
			}
			t.Logf("Go skeleton %d; independent Lee %d; symmetric difference %d; ITK distance max delta %.9g mm; medial samples %d", s.Count(), len(c.Skeleton), difference, maxDelta, len(r.Samples))
		})
	}
}
