package iconimage

import (
	"context"
	"image/color"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
)

func TestThumbnailFrameKeepsPaletteColors(t *testing.T) {
	f := generateFile(4, 2, []byte{0, 0, 1, 1, 0, 0, 1, 1},
		text(core.NewTag(0x0028, 0x0004), core.VRCS, "PALETTE COLOR"))
	for i, entries := range [][]uint16{{0xFFFF, 0}, {0, 0}, {0, 0xFFFF}} {
		f.Dataset.SetUS(core.NewTag(0x0028, uint16(0x1101+i)), 2, 0, 16)
		f.Dataset.SetUS(core.NewTag(0x0028, uint16(0x1201+i)), entries...)
	}
	frame, err := RenderThumbnailFrame(context.Background(), f, ThumbnailFrameOptions{TargetSide: 2})
	if err != nil {
		t.Fatal(err)
	}
	thumb := FitThumbnail(frame, 2)
	if thumb.Bounds().Dx() != 2 || thumb.Bounds().Dy() != 1 {
		t.Fatalf("thumbnail bounds = %v", thumb.Bounds())
	}
	for x, want := range []color.NRGBA{{R: 255, A: 255}, {B: 255, A: 255}} {
		if got := color.NRGBAModel.Convert(thumb.At(x, 0)); got != want {
			t.Errorf("thumbnail pixel %d = %v, want %v", x, got, want)
		}
	}
}

func TestThumbnailReductionKeepsTheTargetSideCovered(t *testing.T) {
	if reductionLevel(1024, 256) != 2 || reductionLevel(300, 256) != 0 || reductionLevel(300, 0) != 0 {
		t.Fatal("unexpected thumbnail reduction level")
	}
}

func TestThumbnailFrameCanSelectExactPhysicalFrame(t *testing.T) {
	dicom := generateFile(2, 2, []byte{8, 16, 24, 32, 192, 200, 208, 216},
		text(core.NewTag(0x0028, 0x0008), core.VRIS, "2"),
		text(core.NewTag(0x0028, 0x1050), core.VRDS, "128"),
		text(core.NewTag(0x0028, 0x1051), core.VRDS, "256"))
	dicom.Dataset.SetUS(tagRepresentativeFrameNumber, 2)
	first := 0
	for _, tt := range []struct {
		frame *int
		gray  uint8
	}{
		{nil, 192},
		{&first, 8},
	} {
		img, err := RenderThumbnailFrame(context.Background(), dicom, ThumbnailFrameOptions{FrameIndex: tt.frame})
		if err != nil {
			t.Fatal(err)
		}
		if got := color.GrayModel.Convert(img.At(0, 0)).(color.Gray).Y; got != tt.gray {
			t.Fatalf("frame %v first pixel = %d, want %d", tt.frame, got, tt.gray)
		}
	}
	first = 2
	if _, err := RenderThumbnailFrame(context.Background(), dicom, ThumbnailFrameOptions{FrameIndex: &first}); err == nil {
		t.Fatal("out-of-range exact frame fell back to the SOP representative")
	}
}
