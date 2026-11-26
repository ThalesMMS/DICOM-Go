package imagefilter

import (
	"context"
	"testing"
)

var psfBenchmarkResult PSFDeconvolutionResult

// Cost profile recorded in PLAN before authoring: 3 cells x count5. Setup
// forward blur is outside timing; restoration preparation/solve/residual/output
// allocations are included. No FFT backend or whole-volume RSS is measured.
func BenchmarkPSFDeconvolutionCost(b *testing.B) {
	for _, tc := range []struct {
		name string
		dims [3]int
	}{{"2D16", [3]int{16, 16, 1}}, {"2D32", [3]int{32, 32, 1}}, {"3D8", [3]int{8, 8, 8}}} {
		b.Run(tc.name, func(b *testing.B) {
			n, _ := voxelCount(tc.dims)
			data := make([]float32, n)
			for i := range data {
				data[i] = 1 + float32(i%17)/17
			}
			truth := psfTestVolume(tc.dims, data)
			kernel := psfNeighborKernel(tc.dims)
			o := PSFDeconvolutionOptions{Iterations: 20, SourceReferences: []string{"synthetic:benchmark"}}
			observed, e := ConvolvePSFContext(context.Background(), truth, kernel, o)
			if e != nil {
				b.Fatal(e)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r, e := RichardsonLucy(context.Background(), observed, kernel, o)
				if e != nil {
					b.Fatal(e)
				}
				psfBenchmarkResult = r
			}
		})
	}
}
