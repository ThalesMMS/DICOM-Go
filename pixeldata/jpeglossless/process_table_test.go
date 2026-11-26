package jpeglossless

import (
	"bytes"
	"errors"
	"image"
	stdjpeg "image/jpeg"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// Under .57/.70 only SOF3 (Process 14) streams decode; DCT processes are
// refused rather than decoded as lossless.
func TestLosslessRejectsDCTProcesses(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 2, 1))
	img.Pix[1] = 255
	var buf bytes.Buffer
	if err := stdjpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	meta := pixeldata.Metadata{Rows: 1, Columns: 2, SamplesPerPixel: 1, BitsAllocated: 8, BitsStored: 8, HighBit: 7, NumberOfFrames: 1, PhotometricInterpretation: "MONOCHROME2"}
	for _, code := range []byte{0xc0, 0xc1, 0xc2} {
		stream := append([]byte(nil), buf.Bytes()...)
		i := bytes.Index(stream, []byte{0xff, 0xc0})
		stream[i+1] = code
		if _, err := decodeFrame(stream, 0, meta); !errors.Is(err, ErrInvalidStream) {
			t.Errorf("SOF%x: error = %v, want ErrInvalidStream", code-0xc0, err)
		}
	}
}
