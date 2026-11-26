package imagefilter

import (
	"context"
	"math"
	"testing"
)

// BenchmarkTensorDiffusion64 runs ten coherence-enhancing iterations on a
// 64³ volume with the shared worker budget, where every pass is a parallel
// loop over the voxels.
func BenchmarkTensorDiffusion64(b *testing.B) {
	v := tensorPhantom("oblique", [3]int{64, 64, 64}, [3]float64{.7, 1.3, 2.1})
	o := tensorTestOptions(CoherenceEnhancing)
	o.Iterations = 10
	o.Workers = 0
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := TensorAnisotropicDiffusion(context.Background(), v, o); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkN4SparseSamples corrects a 128 × 128 × 96 volume at shrink one
// whose mask keeps one voxel in 64, so the bias is evaluated at scattered
// samples, one parallel item per sample before #2025.
func BenchmarkN4SparseSamples(b *testing.B) {
	dims := [3]int{128, 128, 96}
	in := NewVolume(dims, [3]float64{1, 1, 1.5})
	mask := make([]bool, len(in.Data))
	for i := range in.Data {
		in.Data[i] = float32(100 + i%17)
		mask[i] = i%64 == 0
	}
	opts := N4Options{Mask: mask, ShrinkFactor: 1, Iterations: []int{10, 10}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := N4BiasFieldCorrection(context.Background(), in, opts); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWavelet2D512 decomposes a 512 × 512 image into three bands and a
// lowpass; reflection pads it to 1024 × 1024 (2^20 pixels) for five FFTs.
func BenchmarkWavelet2D512(b *testing.B) {
	src := NewVolume([3]int{512, 512, 1}, [3]float64{1, 1, 0})
	for i := range src.Data {
		x, y := float64(i%512), float64(i/512)
		src.Data[i] = float32(math.Sin(x*.1) + .5*math.Cos(y*.07+x*.02))
	}
	opts := Wavelet2DOptions{ScalesMM: []float64{1, 2, 4}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := DecomposeWavelet2D(context.Background(), src, opts); err != nil {
			b.Fatal(err)
		}
	}
}
