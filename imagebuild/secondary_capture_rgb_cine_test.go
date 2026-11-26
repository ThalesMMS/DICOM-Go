package imagebuild

import (
	"bytes"
	"context"
	"errors"
	"image"
	"math"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestSecondaryCaptureRGBCineRoundTrip(t *testing.T) {
	opts := SecondaryCaptureRGBCineOptions{
		SecondaryCaptureRGBOptions: rgbCaptureOptions(),
		Width:                      1, Height: 1, FrameCount: 3, FrameTimeMS: 83.333, ImageTypeDetail: "FLYTHRU",
	}
	opts.SeriesDescription = "Séquence VR"
	frames := [][]byte{{17, 33, 250, 255}, {200, 100, 50, 128}, {33, 44, 55, 0}}
	var order []int
	// The producer can reuse its backing storage across calls.
	frame := &image.NRGBA{Rect: image.Rect(5, 7, 6, 8), Stride: 4, Pix: make([]byte, 4)}
	file, source, err := BuildSecondaryCaptureRGBCineContext(context.Background(), opts, func(_ context.Context, i int) (image.Image, error) {
		order = append(order, i)
		copy(frame.Pix, frames[i])
		return frame, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 0 || file.Dataset.Has(core.TagPixelData) {
		t.Fatal("builder read or retained pixels")
	}
	defer file.Close()
	var out bytes.Buffer
	if err := object.WriteFileFrames(&out, file, source, object.FrameWriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []int{0, 1, 2}) {
		t.Fatalf("producer call order %v", order)
	}
	back, err := object.ReadFile(&out)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	if back.TransferSyntax != transfer.ExplicitVRLittleEndian {
		t.Fatalf("transfer syntax %v", back.TransferSyntax)
	}
	obj := back.Dataset
	for tag, want := range map[core.Tag]string{
		core.NewTag(0x0008, 0x0016): MultiFrameTrueColorSecondaryCaptureStorage,
		core.NewTag(0x0028, 0x0008): "3",
		core.NewTag(0x0018, 0x1063): "83.333",
		core.NewTag(0x0008, 0x0005): "ISO_IR 100",
		core.NewTag(0x0010, 0x0010): "Synthétique^Ana",
		tagSeriesDescription:        opts.SeriesDescription,
	} {
		if got, _ := obj.LookupString(tag); got != want {
			t.Errorf("%s = %q, want %q", tag, got, want)
		}
	}
	if pointers, _ := obj.GetTags(core.NewTag(0x0028, 0x0009)); !reflect.DeepEqual(pointers, []core.Tag{core.NewTag(0x0018, 0x1063)}) {
		t.Errorf("Frame Increment Pointer %v", pointers)
	}
	if ref, _ := obj.GetSequence(core.NewTag(0x0008, 0x2112)); len(ref) != 1 {
		t.Errorf("source references %d", len(ref))
	} else {
		if uid, _ := ref[0].LookupString(core.NewTag(0x0008, 0x1155)); uid != opts.Derivation.Sources[0].SOPInstanceUID {
			t.Errorf("source UID %q", uid)
		}
		if numbers, _ := ref[0].GetInts(core.NewTag(0x0008, 0x1160)); !reflect.DeepEqual(numbers, []int64{2}) {
			t.Errorf("referenced source frames %v", numbers)
		}
	}
	want := []byte{17, 33, 250, 100, 50, 25, 0, 0, 0, 0}
	if raw, _ := obj.GetRaw(core.TagPixelData); !bytes.Equal(raw, want) {
		t.Errorf("Pixel Data %v, want %v", raw, want)
	}
}

func TestSecondaryCaptureRGBCineRejectsInvalidPlanBeforeProducingFrames(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*SecondaryCaptureRGBCineOptions)
	}{
		{"no frames", func(o *SecondaryCaptureRGBCineOptions) { o.FrameCount = 0 }},
		{"frame count limit", func(o *SecondaryCaptureRGBCineOptions) { o.FrameCount = maxRGBCineFrames + 1 }},
		{"dimensions", func(o *SecondaryCaptureRGBCineOptions) { o.Width = 65536 }},
		{"pixel count limit", func(o *SecondaryCaptureRGBCineOptions) { o.Height = 65535; o.Width = 65535 }},
		{"encoded length", func(o *SecondaryCaptureRGBCineOptions) { o.Width, o.Height, o.FrameCount = 1024, 1024, 1366 }},
		{"timing", func(o *SecondaryCaptureRGBCineOptions) { o.FrameTimeMS = math.NaN() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			opts := SecondaryCaptureRGBCineOptions{SecondaryCaptureRGBOptions: rgbCaptureOptions(), Width: 1, Height: 1, FrameCount: 2, FrameTimeMS: 50}
			test.edit(&opts)
			file, frames, err := BuildSecondaryCaptureRGBCineContext(context.Background(), opts, func(context.Context, int) (image.Image, error) {
				t.Fatal("invalid cine plan invoked producer")
				return nil, nil
			})
			if file != nil || frames != nil || !errors.Is(err, ErrInvalid) {
				t.Fatalf("build invalid cine = %v, %v, %v", file, frames, err)
			}
		})
	}
}
