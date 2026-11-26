package jpegls

import (
	"context"
	"errors"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"testing"
)

func TestInspectCodestreamActualLosslessAndNearHeaders(t *testing.T) {
	for _, near := range []int{0, 2} {
		encoder, e := NewEncoderWithOptions(EncoderOptions{Near: near, AllowLossy: near > 0})
		if e != nil {
			t.Fatal(e)
		}
		m := pixeldata.Metadata{Rows: 1, Columns: 4, SamplesPerPixel: 1, BitsAllocated: 8, BitsStored: 8, HighBit: 7, NumberOfFrames: 1, PhotometricInterpretation: "MONOCHROME2"}
		coded, e := encoder.EncodeFrame(context.Background(), []byte{0, 25, 128, 255}, m)
		if e != nil {
			t.Fatal(e)
		}
		h, e := InspectCodestream(context.Background(), coded.Data, 0)
		if e != nil || h.Rows != 1 || h.Columns != 4 || h.Precision != 8 || h.Components != 1 || h.Near != near || h.MaxValue != 255 {
			t.Fatalf("header %+v: %v", h, e)
		}
		if _, e = InspectCodestream(context.Background(), coded.Data, int64(len(coded.Data)-1)); e == nil {
			t.Fatal("byte cap ignored")
		}
		if _, e = InspectCodestream(context.Background(), coded.Data[:len(coded.Data)-2], 0); e == nil {
			t.Fatal("missing EOI accepted")
		}
	}
}
func TestInspectCodestreamCancellationAndInvalidLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := InspectCodestream(ctx, nil, 0); !errors.Is(e, context.Canceled) {
		t.Fatalf("cancel: %v", e)
	}
	if _, e := InspectCodestream(nil, nil, -1); e == nil {
		t.Fatal("negative cap accepted")
	}
}
