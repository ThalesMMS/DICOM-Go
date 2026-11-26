package imagefilter

import (
	"context"
	"math"
	"reflect"
	"testing"
)

// TestWavelet2DParallelFFTMatchesSerial decomposes an image whose padded
// plane (1024 × 512) admits two FFT workers: the bands equal those of a
// one-worker decomposition bit for bit, and the metadata reports the worker
// bound and one column of scratch per worker.
func TestWavelet2DParallelFFTMatchesSerial(t *testing.T) {
	src := NewVolume([3]int{512, 300, 1}, [3]float64{1, 1, 0})
	for i := range src.Data {
		x, y := float64(i%512), float64(i/512)
		src.Data[i] = float32(math.Sin(x*.1) + .5*math.Cos(y*.07+x*.02))
	}
	serial, err := DecomposeWavelet2D(context.Background(), src, Wavelet2DOptions{ScalesMM: []float64{1, 4}, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	parallel, err := DecomposeWavelet2D(context.Background(), src, Wavelet2DOptions{ScalesMM: []float64{1, 4}})
	if err != nil {
		t.Fatal(err)
	}
	if serial.Metadata.PaddedDims != [2]int{1024, 512} || serial.Metadata.Workers != 1 || parallel.Metadata.Workers != 2 {
		t.Fatalf("padded %v, workers %d and %d", serial.Metadata.PaddedDims, serial.Metadata.Workers, parallel.Metadata.Workers)
	}
	if extra := parallel.Metadata.ScratchBytes - serial.Metadata.ScratchBytes; extra != 16*1024 {
		t.Fatalf("parallel scratch exceeds serial by %d bytes, want one 1024-sample column", extra)
	}
	if !reflect.DeepEqual(parallel.Bands, serial.Bands) || !reflect.DeepEqual(parallel.Lowpass, serial.Lowpass) {
		t.Fatal("parallel FFT changed the decomposition")
	}
	if _, err := DecomposeWavelet2D(context.Background(), src, Wavelet2DOptions{ScalesMM: []float64{1}, Workers: -1}); err == nil {
		t.Fatal("negative workers accepted")
	}
}
