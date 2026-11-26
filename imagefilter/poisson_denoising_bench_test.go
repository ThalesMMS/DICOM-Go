package imagefilter

import (
	"context"
	"testing"
)

func poissonCostDims(cell string) ([3]int, bool) {
	switch cell {
	case "2D32":
		return [3]int{32, 32, 1}, true
	case "2D64":
		return [3]int{64, 64, 1}, true
	case "3D16":
		return [3]int{16, 16, 16}, true
	}
	return [3]int{}, false
}
func BenchmarkPoissonDenoisingCost(b *testing.B) {
	for _, cell := range []string{"2D32", "2D64", "3D16"} {
		b.Run(cell, func(b *testing.B) {
			dims, _ := poissonCostDims(cell)
			n, _ := voxelCount(dims)
			data := make([]float32, n)
			for i := range data {
				data[i] = float32(1 + i%13)
			}
			v := poissonTestVolume(dims, data)
			o := poissonTestOptions()
			o.Lambda = .1
			o.Iterations = 10
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r, e := DenoisePoisson(context.Background(), v, o)
				if e != nil || r.Metadata.IterationsCompleted != 10 {
					b.Fatal(e)
				}
			}
		})
	}
}
