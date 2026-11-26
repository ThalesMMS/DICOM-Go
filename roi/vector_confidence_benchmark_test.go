package roi_test

import (
	"fmt"
	"testing"

	"github.com/ThalesMMS/dicom-go/roi"
)

// Descriptive cost only: callbacks synthesize inputs, no precomputed dense
// channel volume. Benchmarks are opt-in and have no timing acceptance threshold.
func BenchmarkVectorConfidence(b *testing.B) {
	for _, dimension := range []int{2, 3} {
		for _, iterations := range []int{0, 2} {
			for _, ridge := range []bool{false, true} {
				b.Run(fmt.Sprintf("%dD/iterations%d/ridge%t", dimension, iterations, ridge), func(b *testing.B) {
					cols, rows, slices, split := 64, 64, 1, 31
					seed := roi.RegionGrowSeed{X: 16, Y: 32}
					if dimension == 3 {
						cols, rows, slices, split = 32, 32, 32, 15
						seed = roi.RegionGrowSeed{X: 8, Y: 16, Slice: 16}
					}
					v := vectorConfidencePhantom(cols, rows, slices, split)
					p := roi.VectorConfidenceParams{Multiplier: 2.5, InitialRadius: 1, Iterations: iterations}
					if ridge {
						p.CovariancePolicy = roi.VectorCovarianceRidge
						p.Ridge = 0.1
					}
					seeds := []roi.RegionGrowSeed{seed}
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						r, err := roi.GrowVectorConfidenceConnected(nil, v, seeds, p, roi.VectorConfidenceOptions{})
						if err != nil || r.VoxelCount != (split+1)*rows*slices {
							b.Fatal("unexpected benchmark result", err)
						}
					}
				})
			}
		}
	}
}
