package roi

import (
	"context"
	"testing"
)

var cvBenchmark1436 any

func BenchmarkChanVeseMatchedMethods(b *testing.B) {
	for _, profile := range []struct {
		name string
		nz   int
		h    [3]float64
	}{{"2d", 1, [3]float64{1, 1, 0}}, {"3d", 7, [3]float64{.8, 1.2, 2}}} {
		f := cvFixture1436(20, 20, profile.nz, profile.h, false, true, false)
		for _, method := range []string{"chan-vese", "threshold", "geodesic"} {
			b.Run(profile.name+"/"+method, func(b *testing.B) {
				o := DefaultChanVeseOptions()
				o.Mu = .005
				o.MaxIterations = 200
				init := LevelSetInitialization{Masks: f.init}
				lastIterations := 0
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if method == "chan-vese" {
						r, e := ChanVese(context.Background(), f.v, f.init, o)
						if e != nil {
							b.Fatal(e)
						}
						lastIterations = r.Iterations
						cvBenchmark1436 = r
					} else if method == "threshold" {
						p := DefaultThresholdLevelSetParams(10.5, 11.5)
						p.MaxIterations = 200
						r, e := ThresholdLevelSet(context.Background(), f.v, init, p, LevelSetOptions{MarginMM: 100})
						if e != nil {
							b.Fatal(e)
						}
						lastIterations = r.Iterations
						cvBenchmark1436 = r
					} else {
						p := DefaultGeodesicActiveContourParams()
						p.MaxIterations = 200
						r, e := GeodesicActiveContour(context.Background(), f.v, init, p, LevelSetOptions{MarginMM: 100})
						if e != nil {
							b.Fatal(e)
						}
						lastIterations = r.Iterations
						cvBenchmark1436 = r
					}
				}
				b.ReportMetric(float64(lastIterations), "iterations/op")
			})
		}
	}
}
