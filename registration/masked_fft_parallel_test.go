package registration

import (
	"context"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/fft"
	"github.com/ThalesMMS/dicom-go/render"
)

// TestMaskedFFTParallelAxesMatchSerial uses an image whose padded transform
// (1024 × 512) admits two FFT workers: the estimate with the shared budget
// is identical to the one-worker estimate, and the admitted scratch counts
// one axis line per FFT worker.
func TestMaskedFFTParallelAxesMatchSerial(t *testing.T) {
	f, m, fm, mm := maskedFFTCostFixture(t, [3]int{400, 200, 1})
	o := MaskedFFTOptions{MinOverlap: 12, MinOverlapFraction: .2, Search: &MaskedFFTSearch{Min: render.Vec3{X: -6, Y: -6}, Max: render.Vec3{X: 6, Y: 6}}}
	serial := o
	serial.Workers = 1
	want, err := EstimateMaskedFFTTranslation(context.Background(), f, m, fm, mm, serial)
	if err != nil {
		t.Fatal(err)
	}
	if want.PaddedSize != [3]int{1024, 512, 1} {
		t.Fatalf("serial estimate %+v", want)
	}
	if lines := fft.AxisWorkers(1024*512, 0); lines != 2 || want.ScratchBytes != 160*1024*512+16*1024 {
		t.Fatalf("serial scratch %d bytes, %d line workers", want.ScratchBytes, lines)
	}
	got, err := EstimateMaskedFFTTranslation(context.Background(), f, m, fm, mm, o)
	if err != nil {
		t.Fatal(err)
	}
	if got.ScratchBytes != 160*1024*512+2*16*1024 {
		t.Fatalf("parallel scratch %d bytes", got.ScratchBytes)
	}
	got.ScratchBytes = want.ScratchBytes
	if got != want {
		t.Fatalf("parallel estimate %+v, serial %+v", got, want)
	}
}
