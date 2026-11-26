package jpegls

import (
	"context"
	"fmt"
	"testing"
)

// BenchmarkFrameProfiles measures only the frame APIs, without file IO,
// verification or durability. Every case uses deterministic, non-PHI pixels.
func BenchmarkFrameProfiles(b *testing.B) {
	for _, components := range []uint16{1, 3} {
		for _, stored := range []uint16{8, 12, 16} {
			allocated := uint16(8)
			if stored > 8 {
				allocated = 16
			}
			photo := "MONOCHROME2"
			modes := []InterleaveMode{InterleaveNone}
			if components == 3 {
				photo = "RGB"
				modes = append(modes, InterleaveLine, InterleaveSample)
			}
			metadata := encoderMetadata(256, 256, components, allocated, photo)
			metadata.BitsStored, metadata.HighBit = stored, stored-1
			frame := interleaveSource(256, 256, int(stored), int(allocated), uint32(components))
			if components == 1 {
				// interleaveSource generates three components.
				frame = frame[:int(metadata.FrameSize())]
			}
			for _, mode := range modes {
				name := fmt.Sprintf("%dc-%dbit-ilv%d", components, stored, mode)
				encoder, err := NewEncoderWithOptions(EncoderOptions{Interleave: mode})
				if err != nil {
					b.Fatal(err)
				}
				stream, err := encoder.EncodeFrame(context.Background(), frame, metadata)
				if err != nil {
					b.Fatal(err)
				}
				b.Run(name+"/encode", func(b *testing.B) {
					b.SetBytes(int64(len(frame)))
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						if _, err := encoder.EncodeFrame(context.Background(), frame, metadata); err != nil {
							b.Fatal(err)
						}
					}
				})
				b.Run(name+"/decode", func(b *testing.B) {
					b.SetBytes(int64(len(frame)))
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						if _, err := decodeFrame(stream.Data, metadata); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		}
	}
}
