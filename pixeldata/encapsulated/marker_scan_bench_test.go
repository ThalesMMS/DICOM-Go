package encapsulated_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/pixeldata/encapsulated"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Structural framing phantoms isolate scanning cost, not decoder throughput.
func markerBenchmarkData(pattern string) []byte {
	entropy := bytes.Repeat([]byte{1}, 1<<20)
	if pattern == "stuffed" || pattern == "dense" {
		stride := 8
		if pattern == "dense" {
			stride = 2
		}
		for i := 0; i < len(entropy); i += stride {
			entropy[i], entropy[i+1] = 0xff, 0
		}
	}
	out := []byte{0xff, 0xd8}
	if pattern == "opaque" {
		payload := bytes.Repeat([]byte{0xff}, 65533)
		for i := 0; i < 16; i++ {
			out = append(out, 0xff, 0xe1, 0xff, 0xff)
			out = append(out, payload...)
		}
		entropy = nil
	}
	out = append(out, 0xff, 0xda, 0, 2)
	out = append(out, entropy...)
	out = append(out, 0xff, 0xd9)
	if len(out)&1 != 0 {
		out = append(out, 0)
	}
	return out
}

func BenchmarkMarkerScan1302(b *testing.B) {
	ctx := context.Background()
	for _, pattern := range []string{"sparse", "stuffed", "opaque", "dense"} {
		data := markerBenchmarkData(pattern)
		for _, format := range []encapsulated.Format{encapsulated.JPEG, encapsulated.JPEGLS} {
			name, syntax := "jpeg", transfer.JPEGBaseline
			if format == encapsulated.JPEGLS {
				name, syntax = "jpegls", transfer.JPEGLSLossless
			}
			b.Run(pattern+"/"+name+"/validate", func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(data)))
				for i := 0; i < b.N; i++ {
					if err := encapsulated.ValidateFrame(ctx, data, format); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run(pattern+"/"+name+"/stream", func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(data)))
				for i := 0; i < b.N; i++ {
					calls := 0
					s, err := encapsulated.NewStream(ctx, parser.EncodedFrameSinkFunc(func(frame parser.EncodedFrame) error {
						if frame.Index != 0 || len(frame.Data) != len(data) {
							b.Fatal("changed frame boundaries")
						}
						calls++
						return nil
					}), encapsulated.Limits{})
					if err != nil {
						b.Fatal(err)
					}
					if err := s.StartPixelData(parser.FrameMetadata{NumberOfFrames: 1}, syntax); err != nil {
						b.Fatal(err)
					}
					if err := s.Item(0, true, bytes.NewReader(nil)); err != nil {
						b.Fatal(err)
					}
					if err := s.Item(uint32(len(data)), false, bytes.NewReader(data)); err != nil {
						b.Fatal(err)
					}
					if err := s.EndPixelData(); err != nil || calls != 1 {
						b.Fatalf("delivery: calls=%d err=%v", calls, err)
					}
					if err := s.Close(); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
