package roi

import (
	"context"
	"math"
	"testing"
)

// nonFiniteValues holds the finite values 1, 2 and 3 mixed with NaN and ±Inf
// (#1240): every statistics entry point must report the statistics of the
// finite values alone and count the three skipped values.
var nonFiniteValues = []float64{1, math.Inf(1), 2, math.NaN(), math.Inf(-1), 3}

func assertFiniteIntensityStatistics(t *testing.T, name string, got IntensityStatistics) {
	t.Helper()
	want := NewIntensityStatistics([]float64{1, 2, 3})
	if got.Count != 3 || got.NonFinite != 3 {
		t.Fatalf("%s: Count, NonFinite = %d, %d; want 3, 3", name, got.Count, got.NonFinite)
	}
	if got.Min != want.Min || got.Max != want.Max || got.Sum != want.Sum || got.Mean != want.Mean ||
		got.Variance != want.Variance || got.Median != want.Median {
		t.Fatalf("%s: got %+v, want the statistics of the finite values %+v", name, got, want)
	}
	if p, ok := got.Percentile(100); !ok || p != 3 {
		t.Fatalf("%s: Percentile(100) = %v, %v; want 3, true", name, p, ok)
	}
}

func TestIntensityStatisticsSkipNonFiniteValues(t *testing.T) {
	n := len(nonFiniteValues)
	valueAt := func(x, _, _ int) (float64, bool) { return nonFiniteValues[x], true }

	assertFiniteIntensityStatistics(t, "NewIntensityStatistics", NewIntensityStatistics(nonFiniteValues))

	mask := NewVolumeMask(n, 1, 1, [3]float64{1, 1, 1})
	for i := range mask.Voxels {
		mask.Voxels[i] = true
	}
	stats, err := mask.IntensityStatistics(nonFiniteValues)
	if err != nil {
		t.Fatal(err)
	}
	assertFiniteIntensityStatistics(t, "VolumeMask.IntensityStatistics", stats)

	seg := statisticsBenchmarkSegmentation(n, 1, 1)
	assertFiniteIntensityStatistics(t, "Segmentation3D.IntensityStatistics", seg.IntensityStatistics(valueAt))

	lv := &LabelVolume{Grid: Grid{Columns: n, Rows: 1, Slices: 1}, Labels: make([]uint16, n)}
	for i := range lv.Labels {
		lv.Labels[i] = 7
	}
	labels, err := LabelIntensityStatistics(context.Background(), lv, nonFiniteValues)
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 1 || labels[0].Label != 7 {
		t.Fatalf("LabelIntensityStatistics = %+v, want one entry for label 7", labels)
	}
	assertFiniteIntensityStatistics(t, "LabelIntensityStatistics", labels[0])
}

func TestLabelIntensityStatisticsReportsLabelWithOnlyNonFiniteValues(t *testing.T) {
	lv := &LabelVolume{Grid: Grid{Columns: 2, Rows: 1, Slices: 1}, Labels: []uint16{4, 4}}
	labels, err := LabelIntensityStatistics(context.Background(), lv, []float64{math.Inf(1), math.NaN()})
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 1 || labels[0].Label != 4 || labels[0].Count != 0 || labels[0].NonFinite != 2 {
		t.Fatalf("LabelIntensityStatistics = %+v, want label 4 with Count 0 and NonFinite 2", labels)
	}
}

func TestSegmentationStatisticsSkipNonFiniteValues(t *testing.T) {
	n := len(nonFiniteValues)
	seg := statisticsBenchmarkSegmentation(n, 1, 1)
	got := seg.Statistics(func(x, _, _ int) (float64, bool) { return nonFiniteValues[x], true }, 4)

	finite := statisticsBenchmarkSegmentation(3, 1, 1)
	want := finite.Statistics(func(x, _, _ int) (float64, bool) { return float64(x + 1), true }, 4)

	if got.VoxelCount != 3 || got.NonFiniteCount != 3 {
		t.Fatalf("VoxelCount, NonFiniteCount = %d, %d; want 3, 3", got.VoxelCount, got.NonFiniteCount)
	}
	if got.Min != want.Min || got.Max != want.Max || got.Mean != want.Mean || got.Variance != want.Variance ||
		got.AreaMM2 != want.AreaMM2 || got.VolumeMM3 != want.VolumeMM3 {
		t.Fatalf("got %+v, want the statistics of the finite values %+v", got, want)
	}
	for i := range want.Histogram {
		if got.Histogram[i] != want.Histogram[i] {
			t.Fatalf("Histogram = %v, want %v", got.Histogram, want.Histogram)
		}
	}
}
