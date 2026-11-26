package render

import (
	"encoding/binary"
	"image"
	"testing"

	"github.com/ThalesMMS/dicom-go/icc"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

func TestRenderFrameAppliesColorTransform(t *testing.T) {
	profile, err := icc.Parse(icc.AdobeRGB1998.Build())
	if err != nil {
		t.Fatal(err)
	}
	transform, err := icc.NewSRGBTransform(profile)
	if err != nil {
		t.Fatal(err)
	}
	frame := &Frame{
		Metadata: pixeldata.Metadata{
			Rows: 1, Columns: 1, SamplesPerPixel: 3, BitsAllocated: 8, BitsStored: 8, HighBit: 7,
			PhotometricInterpretation: "RGB", NumberOfFrames: 1,
		},
		ByteOrder:  binary.LittleEndian,
		PixelBytes: []byte{200, 100, 50},
	}
	rgb := func() [3]uint8 {
		img, err := RenderFrame(frame, WindowLevel{})
		if err != nil {
			t.Fatal(err)
		}
		r, g, b, _ := img.At(0, 0).RGBA()
		return [3]uint8{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8)}
	}
	if got := rgb(); got != [3]uint8{200, 100, 50} {
		t.Fatalf("without a transform = %v", got)
	}
	frame.ColorTransform = transform
	if got := rgb(); got[0] < 226 || got[0] > 228 || got[1] < 99 || got[1] > 101 || got[2] < 41 || got[2] > 43 {
		t.Fatalf("with the Adobe RGB transform = %v, want about [227 100 42]", got)
	}
	frame.Metadata.SamplesPerPixel, frame.Metadata.PhotometricInterpretation, frame.PixelBytes = 1, "MONOCHROME2", []byte{200}
	img, err := RenderFrame(frame, WindowLevel{Center: 127.5, Width: 256})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := img.(*image.Gray); !ok {
		if r, g, b, _ := img.At(0, 0).RGBA(); r != g || g != b {
			t.Fatal("a grayscale frame was colour managed")
		}
	}
}
