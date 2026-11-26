package imagefilter_test

import (
	"context"
	"math"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/imagefilter"
)

// itkHistogramTest.cxx exercises exact and adjacent bin boundaries. The
// scalar image histogram also has to include its threshold and maximum.
func TestITKReferenceHistogramBins(t *testing.T) {
	const base = float32(1 << 20)
	for _, tc := range []struct {
		name      string
		data      []float32
		include   bool
		threshold float64
		counts    []uint64
	}{
		{
			name: "half-open bins and repeated maximum",
			data: []float32{-4, -4, -1, 0, math.Nextafter32(4, 0), 4,
				math.Nextafter32(4, 8), math.Nextafter32(8, 4), 8, 12, 12},
			include: true, threshold: -4, counts: []uint64{3, 2, 3, 3},
		},
		{
			name:      "exact mean belongs to first bin",
			data:      []float32{-9, -3, 0, 0, 5, 7},
			threshold: 0, counts: []uint64{2, 2},
		},
		{
			name:      "fractional signed mean",
			data:      []float32{-4, -3, -2, 0},
			threshold: -2.25, counts: []uint64{1, 0, 1},
		},
		{
			name:    "one bin includes both endpoints",
			data:    []float32{-8, -8, 2, 9, 9},
			include: true, threshold: -8, counts: []uint64{5},
		},
		{
			name:    "offset with sub-unit bins",
			data:    []float32{base, base + .125, base + .25, base + .375, base + .5, base + 1},
			include: true, threshold: float64(base), counts: []uint64{2, 2, 1, 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hist, err := imagefilter.NewIntensityHistogram(tc.data, imagefilter.HistogramOptions{
				Levels: len(tc.counts), IncludeBackground: tc.include,
			})
			if err != nil {
				t.Fatal(err)
			}
			var counted uint64
			for _, n := range tc.counts {
				counted += n
			}
			if hist.Threshold != tc.threshold || hist.Samples != uint64(len(tc.data)) ||
				hist.Counted != counted || !slices.Equal(hist.Counts, tc.counts) {
				t.Fatalf("histogram = %+v, want threshold %v, %d/%d counted, bins %v",
					hist, tc.threshold, counted, len(tc.data), tc.counts)
			}
		})
	}
}

// itkHistogramTest.cxx probes the two quantile traversal directions. An
// empty run between occupied bins distinguishes a cumulative boundary
// reached from below from the same boundary reached from above.
func TestITKReferenceHistogramSparseQuantiles(t *testing.T) {
	for _, tc := range []struct {
		name   string
		counts []uint64
		p      float64
		want   float64
	}{
		{"clamp below range", []uint64{0, 1, 0, 3}, -.25, 0},
		{"clamp above range", []uint64{3, 0, 1, 0}, 1.25, 16},
		{"lower occupied-bin midpoint", []uint64{0, 1, 0, 3}, .125, 6},
		{"lower exact cumulative boundary", []uint64{1, 0, 0, 3}, .25, 4},
		{"above lower cumulative boundary", []uint64{1, 0, 0, 3}, .3125, 12 + 1.0/3.0},
		{"median at empty run", []uint64{2, 0, 0, 2}, .5, 12},
		{"below upper cumulative boundary", []uint64{3, 0, 0, 1}, .625, 10.0 / 3.0},
		{"upper exact cumulative boundary", []uint64{3, 0, 0, 1}, .75, 12},
		{"upper occupied-bin midpoint", []uint64{3, 0, 1, 0}, .875, 10},
		{"one bin", []uint64{9}, .625, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hist := imagefilter.IntensityHistogram{Min: -8, Threshold: 0, Max: 16, Counts: tc.counts}
			if got := hist.Quantile(tc.p); !(math.Abs(got-tc.want) < 1e-12) {
				t.Fatalf("Quantile(%g) = %g, want %g for counts %v", tc.p, got, tc.want, tc.counts)
			}
		})
	}
}

// itkHistogramMatchingImageFilterTest.cxx varies the size of the source
// histogram relative to a reusable reference. Distinct bin widths and a
// different slope in each segment make the chosen landmarks observable.
func TestITKReferenceHistogramMatchUnequalBins(t *testing.T) {
	source := &imagefilter.IntensityHistogram{Min: -8, Threshold: 0, Max: 16, Counts: []uint64{1, 0, 1, 2}}
	reference := &imagefilter.IntensityHistogram{Min: 60, Threshold: 100, Max: 140, Counts: []uint64{0, 2, 0, 1, 1}}
	match, err := imagefilter.NewHistogramMatch(source, reference, 3)
	if err != nil {
		t.Fatal(err)
	}
	wantLandmarks := []imagefilter.HistogramLandmark{
		{Fraction: 0, Source: 0, Reference: 100},
		{Fraction: .25, Source: 4, Reference: 112},
		{Fraction: .5, Source: 12, Reference: 124},
		{Fraction: .75, Source: 14, Reference: 132},
		{Fraction: 1, Source: 16, Reference: 140},
	}
	gotLandmarks := match.Landmarks()
	if !slices.Equal(gotLandmarks, wantLandmarks) {
		t.Fatalf("landmarks = %v, want %v", gotLandmarks, wantLandmarks)
	}
	gotLandmarks[2].Reference = -200
	in := []float32{-10, -8, -4, 0, 2, 4, 8, 12, 13, 14, 15, 16, 22}
	want := []float32{50, 60, 80, 100, 106, 112, 118, 124, 128, 132, 136, 140, 140}
	got := make([]float32, len(in))
	if err := match.Apply(context.Background(), got, in, 1); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("mapping of %v = %v, want %v", in, got, want)
	}
}
