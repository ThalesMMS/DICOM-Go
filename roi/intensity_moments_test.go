package roi

import (
	"context"
	"math"
	"math/rand"
	"sort"
	"sync"
	"testing"
)

// sortedIntensityReference is the statistics computation before #1255:
// copy the finite values, sort them, sum them in order and take a two-pass
// variance and the percentile median.
func sortedIntensityReference(values []float64) (count, nonFinite int, minimum, maximum, sum, mean, variance, median float64, sorted []float64) {
	for _, v := range values {
		if finite(v) {
			sorted = append(sorted, v)
		} else {
			nonFinite++
		}
	}
	count, minimum, maximum, sum, mean, variance, median = sortedReferenceOwned(sorted)
	return
}

// sortedReferenceOwned sorts the finite values in place and computes the
// former statistics.
func sortedReferenceOwned(sorted []float64) (count int, minimum, maximum, sum, mean, variance, median float64) {
	sort.Float64s(sorted)
	count = len(sorted)
	if count == 0 {
		return
	}
	minimum, maximum = sorted[0], sorted[count-1]
	for _, v := range sorted {
		sum += v
	}
	mean = sum / float64(count)
	if count > 1 {
		var sq, c float64
		for _, v := range sorted {
			d := v - mean
			sq += d * d
			c += d
		}
		variance = math.Max(0, (sq-c*c/float64(count))/float64(count-1))
	}
	rank := 0.5 * float64(count-1)
	i := int(math.Floor(rank))
	median = sorted[min(i, count-1)]
	if i < count-1 && rank > float64(i) && sorted[i] != sorted[i+1] {
		median = sorted[i] + (rank-float64(i))*(sorted[i+1]-sorted[i])
	}
	return
}

func relativeClose(a, b, tolerance float64) bool {
	return math.Abs(a-b) <= tolerance*math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
}

// intensityCases are value sets covering odd and even counts, duplicates,
// constants, large offsets and non-finite values.
func intensityCases() map[string][]float64 {
	random := rand.New(rand.NewSource(1255))
	normal := func(n int, mean, sd float64) []float64 {
		out := make([]float64, n)
		for i := range out {
			out[i] = mean + sd*random.NormFloat64()
		}
		return out
	}
	quantized := make([]float64, 5000)
	for i := range quantized {
		quantized[i] = float64(random.Intn(7) - 3)
	}
	withNonFinite := normal(1001, 40, 300)
	withNonFinite[3], withNonFinite[500], withNonFinite[1000] = math.NaN(), math.Inf(1), math.Inf(-1)
	ascending, descending := make([]float64, 4096), make([]float64, 4097)
	for i := range ascending {
		ascending[i] = float64(i)
	}
	for i := range descending {
		descending[i] = float64(-i)
	}
	return map[string][]float64{
		"empty":           nil,
		"one":             {42},
		"two":             {3, -1},
		"constant":        {7, 7, 7, 7, 7, 7},
		"odd normal":      normal(1001, 100, 40),
		"even normal":     normal(4096, -1000, 25),
		"large offset":    normal(2000, 1e9, 1),
		"quantized":       quantized,
		"with non-finite": withNonFinite,
		"only non-finite": {math.NaN(), math.Inf(1)},
		"ascending":       ascending,
		"descending":      descending,
	}
}

// TestIntensityStatisticsMatchTheSortedReference: the streaming moments,
// the selected median and the lazily sorted percentiles give the results of
// the sort-everything computation (#1255): order statistics exactly, sums
// and moments to rounding.
func TestIntensityStatisticsMatchTheSortedReference(t *testing.T) {
	for name, values := range intensityCases() {
		input := append([]float64(nil), values...)
		count, nonFinite, minimum, maximum, sum, mean, variance, median, sorted := sortedIntensityReference(values)
		s := NewIntensityStatistics(input)
		for i := range values {
			if math.Float64bits(input[i]) != math.Float64bits(values[i]) {
				t.Fatalf("%s: input modified", name)
			}
		}
		// Interpolated ranks may differ in the last bit where the compiler
		// fuses the multiply-add in one place and not the other.
		if s.Count != count || s.NonFinite != nonFinite || s.Min != minimum || s.Max != maximum || !relativeClose(s.Median, median, 1e-15) {
			t.Fatalf("%s: got %+v, want count %d non-finite %d min %v max %v median %v", name, s, count, nonFinite, minimum, maximum, median)
		}
		if !relativeClose(s.Sum, sum, 1e-12) || !relativeClose(s.Mean, mean, 1e-12) || !relativeClose(s.Variance, variance, 1e-9) ||
			!relativeClose(s.StandardDeviation, math.Sqrt(variance), 1e-9) {
			t.Fatalf("%s: sum %v mean %v variance %v, reference %v %v %v", name, s.Sum, s.Mean, s.Variance, sum, mean, variance)
		}
		for _, p := range []float64{0, 1, 12.5, 25, 50, 75, 90, 99, 100} {
			got, ok := s.Percentile(p)
			if count == 0 {
				if ok {
					t.Fatalf("%s: percentile of an empty set", name)
				}
				continue
			}
			rank := p / 100 * float64(count-1)
			i := int(math.Floor(rank))
			want := sorted[min(i, count-1)]
			if i < count-1 && rank > float64(i) && sorted[i] != sorted[i+1] {
				want = sorted[i] + (rank-float64(i))*(sorted[i+1]-sorted[i])
			}
			if !ok || !relativeClose(got, want, 1e-15) {
				t.Fatalf("%s: P%v = %v, want %v", name, p, got, want)
			}
		}
		if m := NewIntensityMoments(values); m != s.Moments() {
			t.Fatalf("%s: moments %+v, statistics %+v", name, m, s.Moments())
		}
	}
}

// TestIntensityMomentsAgreeAcrossEntryPoints: the mask, segmentation and
// label entry points give the moments of the slice entry point and of the
// matching statistics.
func TestIntensityMomentsAgreeAcrossEntryPoints(t *testing.T) {
	const nx, ny, nz = 9, 7, 5
	random := rand.New(rand.NewSource(4))
	values := make([]float64, nx*ny*nz)
	labels := make([]uint16, len(values))
	for i := range values {
		values[i] = 50 + 20*random.NormFloat64()
		labels[i] = uint16(random.Intn(3))
	}
	values[11] = math.NaN()
	labels[11] = 2
	lv := &LabelVolume{Grid: Grid{Columns: nx, Rows: ny, Slices: nz}, Labels: labels}
	byLabel, err := LabelIntensityMoments(context.Background(), lv, values)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := LabelIntensityStatistics(context.Background(), lv, values)
	if err != nil {
		t.Fatal(err)
	}
	if len(byLabel) != 2 || len(stats) != 2 {
		t.Fatalf("%d label moments, %d label statistics; want 2", len(byLabel), len(stats))
	}
	for li, label := range []uint16{1, 2} {
		mask := NewVolumeMask(nx, ny, nz, [3]float64{1, 1, 1})
		seg := statisticsBenchmarkSegmentation(nx, ny, nz)
		var inside []float64
		for i, l := range labels {
			mask.Voxels[i] = l == label
			if l == label {
				inside = append(inside, values[i])
			} else {
				seg.SetVoxel(i%nx, (i/nx)%ny, i/(nx*ny), false)
			}
		}
		want := NewIntensityMoments(inside)
		want.Label = label
		maskMoments, err := mask.IntensityMoments(values)
		if err != nil {
			t.Fatal(err)
		}
		maskMoments.Label = label
		segMoments := seg.IntensityMoments(func(x, y, z int) (float64, bool) { return values[(z*ny+y)*nx+x], true })
		segMoments.Label = label
		for name, got := range map[string]IntensityMoments{
			"label": byLabel[li], "label statistics": stats[li].Moments(), "mask": maskMoments, "segmentation": segMoments,
		} {
			if got != want {
				t.Fatalf("label %d %s: %+v, want %+v", label, name, got, want)
			}
		}
		if label == 2 && want.NonFinite != 1 {
			t.Fatalf("label 2 skipped %d non-finite values, want 1", want.NonFinite)
		}
	}
	if _, err := LabelIntensityMoments(context.Background(), lv, values[:3]); err == nil {
		t.Fatal("short values accepted")
	}
	if _, err := NewVolumeMask(2, 2, 1, [3]float64{1, 1, 1}).IntensityMoments(values); err == nil {
		t.Fatal("mismatched mask accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := LabelIntensityMoments(ctx, lv, values); err == nil {
		t.Fatal("canceled context ignored")
	}
}

func TestSelectKthPlacesTheKthValue(t *testing.T) {
	random := rand.New(rand.NewSource(9))
	for trial := 0; trial < 200; trial++ {
		n := 1 + random.Intn(300)
		values := make([]float64, n)
		for i := range values {
			values[i] = float64(random.Intn(1 + trial%20))
		}
		sorted := append([]float64(nil), values...)
		sort.Float64s(sorted)
		k := random.Intn(n)
		selectKth(values, k)
		if values[k] != sorted[k] {
			t.Fatalf("trial %d: k-th %v, want %v", trial, values[k], sorted[k])
		}
		for i := range values {
			if (i < k && values[i] > values[k]) || (i > k && values[i] < values[k]) {
				t.Fatalf("trial %d: value %v at %d on the wrong side of %v at %d", trial, values[i], i, values[k], k)
			}
		}
	}
}

// TestIntensityMomentsDoNotAllocate: moments hold no values.
func TestIntensityMomentsDoNotAllocate(t *testing.T) {
	values := make([]float64, 1<<14)
	for i := range values {
		values[i] = float64(i % 97)
	}
	mask := NewVolumeMask(128, 128, 1, [3]float64{1, 1, 1})
	for i := range mask.Voxels {
		mask.Voxels[i] = i%3 != 0
	}
	if n := testing.AllocsPerRun(10, func() { NewIntensityMoments(values) }); n != 0 {
		t.Fatalf("NewIntensityMoments allocated %v times", n)
	}
	if n := testing.AllocsPerRun(10, func() { _, _ = mask.IntensityMoments(values) }); n != 0 {
		t.Fatalf("VolumeMask.IntensityMoments allocated %v times", n)
	}
}

// TestIntensityPercentilesAreSafeOnSharedCopies: copies of one statistics
// value share the lazily sorted values.
func TestIntensityPercentilesAreSafeOnSharedCopies(t *testing.T) {
	values := intensityCases()["odd normal"]
	s := NewIntensityStatistics(values)
	want, _ := s.Percentile(90)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(copy IntensityStatistics) {
			defer wg.Done()
			if got, _ := copy.Percentile(90); got != want {
				t.Errorf("P90 %v on a copy, want %v", got, want)
			}
		}(s)
	}
	wg.Wait()
}

// BenchmarkIntensityStatisticsLargeVOI measures a 4M-voxel VOI (a 256 × 256
// × 64 region of one value per voxel): the former sort-everything
// computation against the moments alone and the statistics with the
// selected median.
func BenchmarkIntensityStatisticsLargeVOI(b *testing.B) {
	const nx, ny, nz = 256, 256, 64
	random := rand.New(rand.NewSource(1255))
	values := make([]float64, nx*ny*nz)
	for i := range values {
		values[i] = 40 + 300*random.NormFloat64()
	}
	mask := NewVolumeMask(nx, ny, nz, [3]float64{1, 1, 1})
	for i := range mask.Voxels {
		x, y, z := i%nx-nx/2, (i/nx)%ny-ny/2, i/(nx*ny)-nz/2
		mask.Voxels[i] = x*x+y*y+4*z*z < 120*120
	}
	b.Run("sorted-reference", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var inside []float64
			for j, on := range mask.Voxels {
				if on {
					inside = append(inside, values[j])
				}
			}
			sortedReferenceOwned(inside)
		}
	})
	b.Run("statistics", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := mask.IntensityStatistics(values); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("moments", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := mask.IntensityMoments(values); err != nil {
				b.Fatal(err)
			}
		}
	})
}
