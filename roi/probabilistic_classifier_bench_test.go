package roi

import (
	"context"
	"fmt"
	"math"
	"testing"
)

func BenchmarkProbabilisticClassification(b *testing.B) {
	for _, side := range []int{16, 32} {
		for _, mrf := range []bool{false, true} {
			b.Run(fmt.Sprintf("EM_MRF%t/%d", mrf, side), func(b *testing.B) {
				n := side * side * side
				values := make([]float64, n)
				for i := range values {
					if i%2 == 0 {
						values[i] = -4 + float64((i/2)%2)*2
					} else {
						values[i] = 4 + float64((i/2)%2)*2
					}
				}
				in := probabilityFixture(values)
				in.Dimensions = [3]int{side, side, side}
				o := probabilityOptions()
				o.Initial = []GaussianClass{{-3, 1, .5}, {5, 1, .5}}
				if mrf {
					o.MRF = MRFOptions{Beta: .2, Iterations: 10}
				}
				b.ReportAllocs()
				b.SetBytes(int64(n) * 8)
				b.ResetTimer()
				maxMeanError, maxProbabilityError := 0.0, 0.0
				for i := 0; i < b.N; i++ {
					r, e := FitGaussianMixture(context.Background(), in, o)
					if e != nil {
						b.Fatal(e)
					}
					if r.SelectedVoxels != n {
						b.Fatal("count oracle")
					}

					if math.IsNaN(r.LogLikelihood) || math.IsInf(r.LogLikelihood, 0) {
						b.Fatal("finite likelihood oracle")
					}
					for j, expected := range []float64{-3, 5} {
						actual := r.Classes[j]
						if math.IsNaN(actual.Mean) || math.IsInf(actual.Mean, 0) || math.IsNaN(actual.Variance) || math.IsInf(actual.Variance, 0) || actual.Variance <= 0 || math.IsNaN(actual.Weight) || math.IsInf(actual.Weight, 0) {
							b.Fatal("finite model oracle")
						}
						errorValue := math.Abs(actual.Mean - expected)
						if errorValue > maxMeanError {
							maxMeanError = errorValue
						}
						if errorValue > 1e-7 {
							b.Fatal("independent two-mean oracle")
						}
					}
					for v := 0; v < n; v++ {
						for _, p := range r.Posteriors[v*2 : v*2+2] {
							if math.IsNaN(p) || math.IsInf(p, 0) || p < 0 || p > 1 {
								b.Fatal("finite posterior oracle")
							}
						}
						sum := r.Posteriors[v*2] + r.Posteriors[v*2+1]
						errorValue := sum - 1
						if errorValue < 0 {
							errorValue = -errorValue
						}
						if errorValue > maxProbabilityError {
							maxProbabilityError = errorValue
						}
						if sum < .999999999 || sum > 1.000000001 {
							b.Fatal("normalized posteriors")
						}
					}
				}
				b.StopTimer()
				b.ReportMetric(maxMeanError, "max-mean-abs-error")
				b.ReportMetric(maxProbabilityError, "max-probability-sum-error")
			})
		}
	}
}
