package render

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// windowDragFrame builds a synthetic signed 12-bit CT-like frame with a
// rescale, the common W/L drag workload.
func windowDragFrame(rows, cols int) *Frame {
	data := make([]byte, rows*cols*2)
	for y := 0; y < rows; y++ {
		for x := 0; x < cols; x++ {
			value := int16((x*7+y*13)%4096 - 2048)
			binary.LittleEndian.PutUint16(data[(y*cols+x)*2:], uint16(value))
		}
	}
	return &Frame{
		Metadata: pixeldata.Metadata{
			Rows:                      uint16(rows),
			Columns:                   uint16(cols),
			SamplesPerPixel:           1,
			BitsAllocated:             16,
			BitsStored:                12,
			HighBit:                   11,
			PixelRepresentation:       1,
			PhotometricInterpretation: "MONOCHROME2",
		},
		ByteOrder:     binary.LittleEndian,
		PixelBytes:    data,
		Rescale:       Rescale{Slope: 1, Intercept: -1024},
		DefaultWindow: WindowLevel{Center: 40, Width: 400},
	}
}

// BenchmarkRenderCacheWindowDrag measures one interactive W/L drag frame: the
// decoded modality plane is resident and every iteration asks for a new
// window, so each frame pays only the VOI remap (#1222).
func BenchmarkRenderCacheWindowDrag(b *testing.B) {
	for _, size := range []int{512, 2048} {
		b.Run(fmt.Sprintf("%dx%d", size, size), func(b *testing.B) {
			frame := windowDragFrame(size, size)
			cache := NewRenderCache(1 << 30)
			if _, err := cache.RenderFrame(frame, frame.DefaultWindow); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				window := WindowLevel{Center: 40 + float64(i%997), Width: 400 + float64(i%331)}
				if _, err := cache.RenderFrame(frame, window); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRenderCacheFirstWindow measures scrolling onto a slice: every
// iteration decodes a frame the cache has not seen and renders it at its
// default window, and reports the decoded bytes the cache keeps (#1612).
func BenchmarkRenderCacheFirstWindow(b *testing.B) {
	for _, size := range []int{512, 2048} {
		b.Run(fmt.Sprintf("%dx%d", size, size), func(b *testing.B) {
			base := windowDragFrame(size, size)
			cache := NewRenderCache(1 << 30)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				frame := *base
				if _, err := cache.RenderFrame(&frame, frame.DefaultWindow); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				cache.Clear()
				b.StartTimer()
			}
			b.StopTimer()
			frame := *base
			if _, err := cache.RenderFrame(&frame, frame.DefaultWindow); err != nil {
				b.Fatal(err)
			}
			b.ReportMetric(float64(cache.Stats().DecodedBytes), "decoded-B")
		})
	}
}
