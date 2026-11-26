package autothreshold

import (
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/roi"
)

// itkOtsuMaskedThresholdImageFilterTest exercises thresholding a selected
// region. Here clipping and unavailable samples are explicit in the fixture.
func TestITKReferenceHistogramSources(t *testing.T) {
	first := roi.NewRasterMask(5, 2)
	first.SetRun(0, 0, 2)
	first.SetRun(0, 3, 4)
	first.SetRun(1, 1, 4)
	last := roi.NewRasterMask(5, 2)
	last.SetRun(0, 1, 4)
	last.SetRun(1, 1, 4)
	seg := roi.NewSegmentation3D(render.VolumeGeometry{}, 5, 2)
	seg.SetMask(2, last)
	seg.SetMask(0, first)

	nan, inf := math.NaN(), math.Inf(-1)
	planes := [3][10]float64{
		{-3, -3, 0, 0, 4095, -2, nan, 1, 3, 4095},
		{5000, 5000, 5000, 5000, 5000, 5000, 5000, 5000, 5000, 5000},
		{999, -4, inf, 2, 999, 999, 2, 3, 4, 999},
	}
	for _, tc := range []struct {
		name          string
		source        func(func(x, y, z int) (float64, bool)) Values
		wantValues    []float64
		wantHistogram []float64
		wantExcluded  int
	}{
		{
			name: "whole grid",
			source: func(at func(x, y, z int) (float64, bool)) Values {
				return GridValues(5, 2, func(x, y int) (float64, bool) { return at(x, y, 0) })
			},
			wantValues:    []float64{-3, -3, 0, 4095, -2, nan, 1, 3, 4095},
			wantHistogram: []float64{2, 1, 2, 1},
			wantExcluded:  3,
		},
		{
			name: "disjoint runs",
			source: func(at func(x, y, z int) (float64, bool)) Values {
				return MaskValues(first, func(x, y int) (float64, bool) { return at(x, y, 0) })
			},
			wantValues:    []float64{-3, -3, nan, 1, 3},
			wantHistogram: []float64{2, 0, 1, 1},
			wantExcluded:  1,
		},
		{
			name:          "sparse slices",
			source:        func(at func(x, y, z int) (float64, bool)) Values { return SegmentationValues(seg, at) },
			wantValues:    []float64{-3, -3, nan, 1, 3, -4, inf, 2, 2, 4},
			wantHistogram: []float64{3, 0, 1, 4},
			wantExcluded:  2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reads int
			values := tc.source(func(x, y, z int) (float64, bool) {
				reads++
				unavailable := (x == 3 && y == 0 && z == 0) || (x == 2 && y == 1 && z == 2)
				return planes[z][y*5+x], !unavailable
			})
			values(func(float64) bool { return false })
			if reads != 1 {
				t.Fatalf("reads on early stop = %d, want 1", reads)
			}
			var observed []float64
			values(func(v float64) bool {
				observed = append(observed, v)
				return true
			})
			if !slices.EqualFunc(observed, tc.wantValues, func(a, b float64) bool {
				return a == b || math.IsNaN(a) && math.IsNaN(b)
			}) {
				t.Fatalf("read values = %v, want %v", observed, tc.wantValues)
			}
			h, err := NewHistogram(values, HistogramOptions{Bins: 4, Lower: -4, Upper: 4})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(h.Counts, tc.wantHistogram) || h.Excluded != tc.wantExcluded ||
				h.Samples != len(tc.wantValues)-tc.wantExcluded {
				t.Fatalf("histogram = %+v, want counts %v and %d exclusions", h, tc.wantHistogram, tc.wantExcluded)
			}
		})
	}
}

// The two ITK entropy calculator GTests check a zero-entropy tie between the
// first occupied bin and later empty bins.
func TestITKReferenceEntropyThresholdTies(t *testing.T) {
	for _, tc := range []struct {
		name         string
		counts       []float64
		lower, upper float64
		want         Threshold
	}{
		{"even peaks", []float64{0, 1, 0, 1, 0}, 0, 5, Threshold{Bin: 1, Center: 1.5, Value: 2}},
		{"unequal peaks on signed range", []float64{0, 0, 1, 0, 0, 3, 0}, -14, 0, Threshold{Bin: 2, Center: -9, Value: -8}},
		{"adjacent fractional weights", []float64{0, 0.25, 0.75, 0}, -2, 0, Threshold{Bin: 1, Center: -1.25, Value: -1}},
		{"outermost bins", []float64{1, 0, 0, 0, 1}, 0, 10, Threshold{Bin: 0, Center: 1, Value: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := NewHistogramFromCounts(tc.counts, tc.lower, tc.upper)
			if err != nil {
				t.Fatal(err)
			}
			for _, method := range []Method{MaxEntropy, RenyiEntropy} {
				t.Run(string(method), func(t *testing.T) {
					got, err := h.Threshold(method)
					if err != nil || got != tc.want {
						t.Fatalf("threshold = %+v, %v, want %+v", got, err, tc.want)
					}
					assertThresholdTransition(t, []Threshold{got}, 0)
				})
			}
		})
	}
}

// itkOtsuMultipleThresholdsImageFilterTest2/3/5 cover a single split, a
// rare dark mode and three modes. Enumerate all valid partitions of each
// small histogram to check the objective independently of the Go optimizer.
func TestITKReferenceOtsuClassPartitions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		counts []float64
	}{
		{"contiguous imbalanced", []float64{1, 3, 8, 2, 13, 1}},
		{"rare dark mode", []float64{1, 0, 0, 0, 0, 29, 30, 2, 8}},
		{"weighted sparse modes", []float64{0, 0.5, 2, 0, 1.5, 0, 0.25, 2.75, 0}},
	} {
		for classes := 2; classes <= 4; classes++ {
			t.Run(fmt.Sprintf("%s/%d classes", tc.name, classes), func(t *testing.T) {
				const lower, width = -6.0, 2.0
				h, err := NewHistogramFromCounts(tc.counts, lower, lower+width*float64(len(tc.counts)))
				if err != nil {
					t.Fatal(err)
				}
				got, err := h.MultiOtsu(classes)
				if err != nil || len(got) != classes-1 {
					t.Fatalf("MultiOtsu = %+v, %v, want %d thresholds", got, err, classes-1)
				}
				if classes == 2 {
					single, err := h.Threshold(Otsu)
					if err != nil || single != got[0] {
						t.Fatalf("Otsu = %+v, %v, want %+v", single, err, got[0])
					}
				}

				score, start := 0.0, 0
				for index, threshold := range got {
					end := threshold.Bin + 1
					if end <= start || end >= len(tc.counts) {
						t.Fatalf("thresholds = %+v, invalid boundary at %d", got, index)
					}
					if want := lower + width*float64(end); threshold.Value != want || threshold.Center != want-width/2 {
						t.Fatalf("threshold = %+v, want boundary %v and center %v", threshold, want, want-width/2)
					}
					assertThresholdTransition(t, got, index)
					score += withinClassVariance(tc.counts[start:end])
					start = end
				}
				score += withinClassVariance(tc.counts[start:])
				if want := minPartitionVariance(tc.counts, classes); !(math.Abs(score-want) <= 1e-10) {
					t.Fatalf("class variance sum = %v, exhaustive minimum %v", score, want)
				}
			})
		}
	}
}

// An empty class is inadmissible. Using bin indices for the values
// translates and scales the total variance equally for all partitions.
func withinClassVariance(counts []float64) float64 {
	mass, weighted := 0.0, 0.0
	for i, count := range counts {
		mass += count
		weighted += count * float64(i)
	}
	if mass == 0 {
		return math.Inf(1)
	}
	mean := weighted / mass
	var sum float64
	for i, count := range counts {
		delta := float64(i) - mean
		sum += count * delta * delta
	}
	return sum
}

func minPartitionVariance(counts []float64, classes int) float64 {
	if classes == 1 {
		return withinClassVariance(counts)
	}
	best := math.Inf(1)
	for end := 1; end <= len(counts)-classes+1; end++ {
		score := withinClassVariance(counts[:end]) + minPartitionVariance(counts[end:], classes-1)
		best = math.Min(best, score)
	}
	return best
}

func assertThresholdTransition(t *testing.T, thresholds []Threshold, index int) {
	t.Helper()
	edge := thresholds[index].Value
	if below, at := ClassOf(thresholds, math.Nextafter(edge, math.Inf(-1))), ClassOf(thresholds, edge); below != index || at != index+1 {
		t.Fatalf("classes just below and at %v = %d, %d; want %d, %d", edge, below, at, index, index+1)
	}
}
