package registration

import (
	"context"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

func TestMaskedFFTWorkspaceIncludesInconclusiveKernel(t *testing.T) {
	im, err := NewImage([3]int{8, 6, 1}, make([]float32, 48), render.Vec3{}, render.Vec3{X: 1, Y: 1, Z: 1}, Identity3())
	if err != nil {
		t.Fatal(err)
	}
	maskData := make([]float32, 48)
	for i := range maskData {
		maskData[i] = 1
	}
	mask, err := NewImage([3]int{8, 6, 1}, maskData, render.Vec3{}, render.Vec3{X: 1, Y: 1, Z: 1}, Identity3())
	if err != nil {
		t.Fatal(err)
	}
	bytes, work, err := MaskedFFTWorkspace(im, im, mask, mask, MaskedFFTOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Linear 15x11x1 support pads to 16x16x1, with 8 total FFT stages.
	if bytes != 160*256+16*16 || work != 12*256*8 {
		t.Fatal("public workspace contract", bytes, work)
	}
	if result, err := EstimateMaskedFFTTranslation(context.Background(), im, im, mask, mask, MaskedFFTOptions{}); !errors.Is(err, ErrMaskedFFTVariance) || result != (MaskedFFTResult{}) {
		t.Fatal(result, err)
	}
	if _, _, err := MaskedFFTWorkspace(im, im, mask, mask, MaskedFFTOptions{MaxFFTWork: work - 1}); !errors.Is(err, ErrMaskedFFT) {
		t.Fatal("work admission", err)
	}
}
