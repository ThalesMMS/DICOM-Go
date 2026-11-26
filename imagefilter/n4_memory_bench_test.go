package imagefilter

import (
	"context"
	"fmt"
	"testing"
)

var benchmarkN41505Result *N4Result

// Fixed cases: masks select 1/64 or all positive voxels. Setup is excluded
// from benchmark time/allocations, but included in whole-process RSS.
func BenchmarkN41505Memory(b *testing.B) {
	for _, dims := range [][3]int{{64, 48, 32}, {128, 96, 1}, {128, 96, 3}} {
		for _, sparse := range []bool{false, true} {
			b.Run(fmt.Sprintf("%dx%dx%d/sparse%v", dims[0], dims[1], dims[2], sparse), func(b *testing.B) {
				in := NewVolume(dims, [3]float64{0.7, 1.8, 4.2})
				mask := make([]bool, len(in.Data))
				for i := range in.Data {
					in.Data[i] = float32(100 + i%17)
					mask[i] = !sparse || i%64 == 0
				}
				opts := N4Options{Mask: mask, ShrinkFactor: 1, Iterations: []int{3, 3}, Workers: 2}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					var err error
					benchmarkN41505Result, err = N4BiasFieldCorrection(context.Background(), in, opts)
					if err != nil {
						b.Fatal(err)
					}
					if benchmarkN41505Result.Corrected == nil || benchmarkN41505Result.LogBiasField == nil {
						b.Fatal("missing public output")
					}
				}
			})
		}
	}
}
