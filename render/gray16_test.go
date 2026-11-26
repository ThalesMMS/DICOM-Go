package render

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

func gray16TestFrame(photometric string, values []uint16) *Frame {
	data := make([]byte, 2*len(values))
	for i, v := range values {
		binary.LittleEndian.PutUint16(data[2*i:], v)
	}
	return &Frame{
		Metadata: pixeldata.Metadata{
			Rows:                      1,
			Columns:                   uint16(len(values)),
			SamplesPerPixel:           1,
			BitsAllocated:             16,
			BitsStored:                12,
			HighBit:                   11,
			PhotometricInterpretation: photometric,
		},
		ByteOrder:     binary.LittleEndian,
		PixelBytes:    data,
		DefaultWindow: WindowLevel{Center: 2048, Width: 4096},
		Rescale:       Rescale{Slope: 1},
	}
}

func TestRenderFrameGray16KeepsPrecisionAndMatchesEightBitRender(t *testing.T) {
	values := []uint16{0, 1, 2, 3, 1000, 1001, 2048, 4095}
	frame := gray16TestFrame("MONOCHROME2", values)
	window := WindowLevel{Center: 2048, Width: 4096}
	wide, err := RenderFrameGray16(frame, window)
	if err != nil {
		t.Fatal(err)
	}
	narrow, err := RenderFrame(frame, window)
	if err != nil {
		t.Fatal(err)
	}
	for x := range values {
		p := wide.Gray16At(x, 0).Y
		b := renderCacheGrayAt(narrow, x, 0)
		if d := int(p>>8) - int(b); d < -1 || d > 1 {
			t.Fatalf("pixel %d: 16-bit %d vs 8-bit %d", x, p, b)
		}
	}
	// Neighbouring 12-bit values collapse in 8 bits but stay distinct here.
	if wide.Gray16At(0, 0).Y == wide.Gray16At(1, 0).Y || wide.Gray16At(4, 0).Y == wide.Gray16At(5, 0).Y {
		t.Fatal("16-bit render lost the precision of adjacent stored values")
	}

	cache := NewRenderCache(1 << 20)
	cached, err := cache.RenderFrameGray16(frame, window)
	if err != nil {
		t.Fatal(err)
	}
	for i := range wide.Pix {
		if wide.Pix[i] != cached.Pix[i] {
			t.Fatal("cached 16-bit render differs from the uncached one")
		}
	}
}

func TestRenderFrameGray16InvertsMonochrome1AndRejectsColor(t *testing.T) {
	values := []uint16{0, 4095}
	mono2, err := RenderFrameGray16(gray16TestFrame("MONOCHROME2", values), WindowLevel{})
	if err != nil {
		t.Fatal(err)
	}
	mono1, err := RenderFrameGray16(gray16TestFrame("MONOCHROME1", values), WindowLevel{})
	if err != nil {
		t.Fatal(err)
	}
	for x := range values {
		if a, b := mono2.Gray16At(x, 0).Y, mono1.Gray16At(x, 0).Y; a+b != 65535 {
			t.Fatalf("pixel %d: MONOCHROME2 %d + MONOCHROME1 %d != 65535", x, a, b)
		}
	}

	color := &Frame{
		Metadata: pixeldata.Metadata{
			Rows: 1, Columns: 1, SamplesPerPixel: 3, BitsAllocated: 8, BitsStored: 8, HighBit: 7,
			PhotometricInterpretation: "RGB",
		},
		ByteOrder:  binary.LittleEndian,
		PixelBytes: []byte{10, 20, 30},
	}
	if _, err := RenderFrameGray16(color, WindowLevel{}); !errors.Is(err, ErrNotGrayscaleFrame) {
		t.Fatalf("color frame: %v", err)
	}
	if _, err := RenderFrameGray16(nil, WindowLevel{}); err == nil {
		t.Fatal("nil frame rendered")
	}
}
