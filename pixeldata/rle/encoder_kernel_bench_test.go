package rle

import (
	"context"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

func BenchmarkEncoderKernel(b *testing.B) {
	for _, layout := range []struct {
		name          string
		samples, bits uint16
	}{{"mono8", 1, 8}, {"mono16", 1, 16}, {"rgb8", 3, 8}, {"rgb16", 3, 16}, {"mono120", 1, 120}} {
		for _, pattern := range []string{"noise", "runs", "short-runs"} {
			metadata := encoderMetadata(512, 512, layout.samples, layout.bits, "MONOCHROME2")
			if layout.samples == 3 {
				metadata.PhotometricInterpretation = "RGB"
			}
			frame := kernelPixels(512*512*int(layout.samples)*int(layout.bits/8), pattern)
			for _, version := range []string{"baseline", "current"} {
				b.Run(layout.name+"/"+pattern+"/"+version, func(b *testing.B) {
					var encode func(context.Context, []byte, pixeldata.Metadata) (pixeldata.EncodedFrame, error)
					if version == "baseline" {
						encode = baselineNewEncoder().EncodeFrame
					} else {
						encode = NewEncoder().EncodeFrame
					}
					if _, err := encode(context.Background(), frame, metadata); err != nil {
						b.Fatal(err)
					}
					b.SetBytes(int64(len(frame)))
					b.ReportAllocs()
					b.ResetTimer()
					for range b.N {
						if _, err := encode(context.Background(), frame, metadata); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		}
	}
}
