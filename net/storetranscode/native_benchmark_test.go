package storetranscode

import (
	"context"
	"encoding/hex"
	"io"
	"testing"

	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// This benchmark measures the complete local preparation/spool/write/close
// path on a small synthetic JPEG Lossless Part 10 fixture, not a codec kernel,
// large-study throughput, network latency or target-PACS acceptance.
func BenchmarkNativeRetrieveSelectedRepresentation(b *testing.B) {
	for _, mode := range []string{"OriginalLazy", "NativeFallback", "LegacyEagerOriginal"} {
		b.Run(mode, func(b *testing.B) {
			source, calls, original, path := nativeFixtureSource(b, 2<<20)
			descriptor, inspectErr := dimse.NewPathStoreSource(path).Inspect(context.Background())
			if inspectErr != nil {
				b.Fatal(inspectErr)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(original)))
			b.ResetTimer()
			for range b.N {
				var opened dimse.OpenedStoreSource
				var err error
				target := transfer.JPEGLosslessNonHierarchical
				if mode == "LegacyEagerOriginal" {
					eager, createErr := NewSource(dimse.NewPathStoreSource(path), Options{Target: transfer.ExplicitVRLittleEndian, FallbackOriginal: true, Transcode: source.opts.Transcode, SpoolDirectory: source.opts.SpoolDirectory, MaxSpoolBytes: 2 << 20})
					if createErr != nil {
						b.Fatal(createErr)
					}
					opened, err = eager.Open(context.Background())
				} else {
					fresh, createErr := NewNativePathSource(path, descriptor, hex.EncodeToString(source.path.expectedSHA256[:]), source.opts)
					if createErr != nil {
						b.Fatal(createErr)
					}
					if mode == "NativeFallback" {
						target = transfer.ExplicitVRLittleEndian
					}
					opened, err = fresh.OpenForTransferSyntax(context.Background(), target)
				}
				if err != nil {
					b.Fatal(err)
				}
				if err := opened.WriteDataSet(context.Background(), io.Discard, target); err != nil {
					b.Fatal(err)
				}
				if err := opened.Close(); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(calls.Load())/float64(b.N), "decodes/op")
			if mode == "OriginalLazy" && calls.Load() != 0 {
				b.Fatal("original path decoded")
			}
		})
	}
}
