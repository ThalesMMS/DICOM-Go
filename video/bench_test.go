package video

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func benchmarkMux(b *testing.B, name string, codec Codec) {
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := MuxFragmentedMP4(context.Background(), io.Discard, data, MuxOptions{Codec: codec, FrameRate: 25}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMuxFragmentedMP4H264(b *testing.B) { benchmarkMux(b, "vt-h264.h264", CodecH264) }

func BenchmarkMuxFragmentedMP4HEVC(b *testing.B) { benchmarkMux(b, "vt-hevc.h265", CodecHEVC) }
