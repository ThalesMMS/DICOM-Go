package jpeg

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// BenchmarkJPEGExtendedDecodeBuffers measures complete Process 4 frame
// decoding. Neither generating the codestream nor assembling the DICOM
// fixture is timed.
func BenchmarkJPEGExtendedDecodeBuffers(b *testing.B) {
	ctx := context.Background()
	for _, fixture := range []struct {
		name          string
		rows, columns uint16
	}{
		{"independent_8x8", 8, 8},
		{"odd_2047x2049", 2049, 2047},
	} {
		b.Run(fixture.name, func(b *testing.B) {
			stream, err := base64.StdEncoding.DecodeString(process4FixtureBase64)
			if err != nil {
				b.Fatal(err)
			}
			if fixture.rows != 8 {
				encoder, err := NewExtendedEncoder(DefaultQuality)
				if err != nil {
					b.Fatal(err)
				}
				m := extendedEncoderMetadata(fixture.rows, fixture.columns, 12)
				raw := extendedPhantomFrame(int(fixture.rows), int(fixture.columns), 12, 1740)
				encoded, err := encoder.EncodeFrame(ctx, raw, m)
				if err != nil {
					b.Fatal(err)
				}
				stream = encoded.Data
			}
			obj, pixel := process4ObjectNoTest(stream, fixture.rows, fixture.columns, 16, 12, 11, 0)
			registry := pixeldata.NewMemoryRegistry()
			if err := Register(registry); err != nil {
				b.Fatal(err)
			}
			codec, ok := registry.GetCodec(transfer.JPEGExtended.UID)
			if !ok {
				b.Fatal("JPEG Extended codec missing")
			}
			b.SetBytes(int64(fixture.rows) * int64(fixture.columns) * 2)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				frames, err := codec.Decode(pixel, obj)
				if err != nil || len(frames.Data) != 1 || len(frames.Data[0]) != int(fixture.rows)*int(fixture.columns)*2 {
					b.Fatalf("decode output layout: frames=%d err=%v", len(frames.Data), err)
				}
			}
		})
	}
}
