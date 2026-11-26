package imagefilter

// Scenarios ported from ITK (Apache-2.0, commit 35b196e1)
// Modules/Filtering/ImageIntensity/test/itkHistogramMatchingImageFilterTest.cxx:
// a 30×20×2 image whose four intensity classes are scaled by different
// factors must be mapped back onto the reference within one intensity unit,
// with the reference given as an image and as a 50-bin histogram against
// source histograms of 50, 31 and 93 bins. The synthetic patterns and the
// tolerance are re-expressed here; the assertions are adapted to this API.

import (
	"context"
	"errors"
	"math"
	"testing"
)

// itkHistogramPatterns returns the reference and source images of the ITK
// test: offsets 0–39, 40–159, 160–199 and 200–319 hold 5, 10, 15 and 20 in
// the reference and 7.5, 9, 15 and 16 in the source; the rest is 0.
func itkHistogramPatterns() (reference, source *Volume) {
	dims := [3]int{30, 20, 2}
	reference = NewVolume(dims, IsotropicSigma(1))
	source = NewVolume(dims, IsotropicSigma(1))
	for i := range reference.Data {
		switch {
		case i < 40:
			reference.Data[i], source.Data[i] = 5, 5*1.5
		case i < 160:
			reference.Data[i], source.Data[i] = 10, 10*0.9
		case i < 200:
			reference.Data[i], source.Data[i] = 15, 15*1.0
		case i < 320:
			reference.Data[i], source.Data[i] = 20, 20*0.8
		}
	}
	return reference, source
}

func assertWithinOne(t *testing.T, name string, got []float32, want []float32) {
	t.Helper()
	for i := range want {
		if d := math.Abs(float64(got[i] - want[i])); d > 1 {
			t.Fatalf("%s: sample %d = %v, reference %v (|diff| %v > 1)", name, i, got[i], want[i], d)
		}
	}
}

func TestITKHistogramMatchingReferenceImage(t *testing.T) {
	reference, source := itkHistogramPatterns()
	before := source.Clone()
	opts := HistogramMatchOptions{HistogramOptions: HistogramOptions{Levels: 50}, MatchPoints: 8}

	// ITK first exercises ThresholdAtMeanIntensity off (no assertion on the
	// image), then checks the default threshold at the mean.
	if _, _, err := MatchHistograms(context.Background(), source, reference,
		HistogramMatchOptions{HistogramOptions: HistogramOptions{Levels: 50, IncludeBackground: true}, MatchPoints: 8}); err != nil {
		t.Fatal(err)
	}
	out, match, err := MatchHistograms(context.Background(), source, reference, opts)
	if err != nil {
		t.Fatal(err)
	}
	assertWithinOne(t, "reference image", out.Data, reference.Data)
	if match.MatchPoints() != 8 || len(match.Landmarks()) != 10 {
		t.Fatalf("landmarks = %d (%d points), want 10 (8)", len(match.Landmarks()), match.MatchPoints())
	}
	for i := range before.Data {
		if source.Data[i] != before.Data[i] {
			t.Fatalf("source sample %d changed from %v to %v", i, before.Data[i], source.Data[i])
		}
	}
}

func TestITKHistogramMatchingReferenceHistogram(t *testing.T) {
	reference, source := itkHistogramPatterns()
	refHist, err := NewIntensityHistogram(reference.Data, HistogramOptions{Levels: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		levels int
	}{
		{"same size", 50},
		{"smaller source histogram", 31},
		{"larger source histogram", 93},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srcHist, err := NewIntensityHistogram(source.Data, HistogramOptions{Levels: tc.levels})
			if err != nil {
				t.Fatal(err)
			}
			match, err := NewHistogramMatch(srcHist, refHist, 8)
			if err != nil {
				t.Fatal(err)
			}
			out := make([]float32, len(source.Data))
			if err := match.Apply(context.Background(), out, source.Data, 0); err != nil {
				t.Fatal(err)
			}
			assertWithinOne(t, tc.name, out, reference.Data)
		})
	}
}

// ITK rejects a filter whose reference input does not match the selected
// reference mode; here the equivalent is a missing or unusable reference.
func TestITKHistogramMatchingMissingReference(t *testing.T) {
	_, source := itkHistogramPatterns()
	srcHist, err := NewIntensityHistogram(source.Data, HistogramOptions{Levels: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewHistogramMatch(srcHist, nil, 2); !errors.Is(err, ErrInvalidHistogramMatch) {
		t.Fatalf("nil reference histogram: err = %v", err)
	}
	if _, _, err := MatchHistograms(context.Background(), source, nil, HistogramMatchOptions{}); !errors.Is(err, ErrInvalidVolume) {
		t.Fatalf("nil reference image: err = %v", err)
	}
}
