package roi

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
)

func TestIntensityVisitIndependentMomentsAndExactQuantiles(t *testing.T) {
	values := []float64{9, 1, 7, 3, 5, math.NaN(), math.Inf(1), math.Inf(-1)}
	visit := func(emit func(float64) error) error {
		for _, v := range values {
			if err := emit(v); err != nil {
				return err
			}
		}
		return nil
	}
	m, err := IntensityMomentsVisit(context.Background(), visit)
	if err != nil {
		t.Fatal(err)
	}
	if m.Count != 5 || m.NonFinite != 3 || m.Sum != 25 || m.Mean != 5 || m.Variance != 10 || m.Min != 1 || m.Max != 9 {
		t.Fatalf("moments %+v", m)
	}
	s, err := IntensityStatisticsVisit(context.Background(), 8, 64, visit)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range []float64{0, 5, 25, 50, 75, 95, 100} {
		got, ok := s.Percentile(p)
		want := []float64{1, 1.4, 3, 5, 7, 8.6, 9}[i]
		if !ok || math.Abs(got-want) > 1e-12 {
			t.Fatalf("P%g %g want %g", p, got, want)
		}
	}
	if s.Median != 5 || !reflect.DeepEqual(s.Moments(), m) || values[0] != 9 || values[1] != 1 || !math.IsNaN(values[5]) {
		t.Fatal("statistics or input changed")
	}
	original := NewIntensityStatistics(values)
	values[0] = 100
	if p, _ := original.Percentile(100); p != 9 {
		t.Fatal("old constructor ceased copying")
	}
}
func TestIntensityVisitAdmissionCancellationAndIgnoredEmitError(t *testing.T) {
	called := 0
	visit := func(emit func(float64) error) error { called++; _ = emit(1); _ = emit(2); return nil }
	if _, err := IntensityStatisticsVisit(context.Background(), 10, 79, visit); err == nil || called != 0 {
		t.Fatal("allocation not pre-admitted")
	}
	if _, err := IntensityStatisticsVisit(context.Background(), 1, 8, visit); err == nil {
		t.Fatal("visitor swallowed limit error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := called
	if _, err := IntensityStatisticsVisit(ctx, 1, 8, visit); !errors.Is(err, context.Canceled) || called != before {
		t.Fatal("cancelled visitor invoked")
	}
	ctx, cancel = context.WithCancel(context.Background())
	visit = func(emit func(float64) error) error {
		for i := 0; i < 4096; i++ {
			if err := emit(float64(4096 - i)); err != nil {
				return err
			}
		}
		cancel()
		return nil
	}
	if _, err := IntensityStatisticsVisit(ctx, 4096, 4096*8, visit); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	empty := func(emit func(float64) error) error { return emit(math.NaN()) }
	s, err := IntensityStatisticsVisit(context.Background(), 1, 8, empty)
	if err != nil || s.Count != 0 || s.NonFinite != 1 || !math.IsNaN(s.Median) {
		t.Fatal(s, err)
	}
}
func TestVolumeSummaryAndTextureAdmissionPreserveMask(t *testing.T) {
	t.Run("sparse-dimensional-shape-bound", func(t *testing.T) {
		sparse := NewVolumeMask(1024, 1024, 2, [3]float64{1, 1, 1})
		sparse.Voxels[0] = true
		sparse.Voxels[len(sparse.Voxels)-1] = true
		bytes, err := sparse.ShapeWorkspaceBytes(context.Background())
		if err != nil || bytes < 8*1024*1024 {
			t.Fatal("sparse Feret/Crofton planes not admitted", bytes, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := sparse.ShapeWorkspaceBytes(ctx); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})

	m := NewVolumeMask(3, 2, 2, [3]float64{2, 3, 4})
	for i := range m.Voxels {
		m.Voxels[i] = true
	}
	before := append([]bool(nil), m.Voxels...)
	n, volume, err := m.Summary(context.Background())
	if err != nil || n != 12 || volume != 288 {
		t.Fatal(n, volume, err)
	}
	calls := 0
	value := func(x, y, z int) (float64, bool) { calls++; return float64(x + y + z), true }
	opts := TextureOptions{BinWidth: 1, MaxGreyLevels: 8, Workers: 1}
	if _, err := m.TextureFeaturesBounded(context.Background(), value, opts, 12, 1); err == nil || calls != 0 {
		t.Fatal("texture sampled before admission")
	}
	a, err := m.TextureFeaturesBounded(context.Background(), value, opts, 12, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.TextureFeatures(context.Background(), value, opts)
	if err != nil {
		t.Fatal(err)
	}
	for i, f := range a.List() {
		g := b.List()[i]
		if f.Value != g.Value && !(math.IsNaN(f.Value) && math.IsNaN(g.Value)) {
			t.Fatal(f, g)
		}
	}
	if !reflect.DeepEqual(before, m.Voxels) {
		t.Fatal("mask mutated")
	}
}

var intensityDemandSink IntensityStatistics

func BenchmarkIntensityDemand(b *testing.B) {
	for _, n := range []int{8192, 262144} {
		for _, kind := range []string{"summary", "full"} {
			b.Run(kind+"/"+fmtIntDemand(n), func(b *testing.B) {
				visit := func(emit func(float64) error) error {
					for i := 0; i < n; i++ {
						if err := emit(float64(i%257) - 128); err != nil {
							return err
						}
					}
					return nil
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if kind == "summary" {
						m, err := IntensityMomentsVisit(context.Background(), visit)
						if err != nil {
							b.Fatal(err)
						}
						intensityDemandSink = IntensityStatistics{Count: m.Count, Mean: m.Mean}
					} else {
						s, err := IntensityStatisticsVisit(context.Background(), n, int64(n)*8, visit)
						if err != nil {
							b.Fatal(err)
						}
						intensityDemandSink = s
					}
				}
			})
		}
	}
}
func fmtIntDemand(n int) string {
	if n == 8192 {
		return "8192"
	}
	return "262144"
}
