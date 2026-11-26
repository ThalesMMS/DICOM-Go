package storetranscode

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/transfer"
)

// BenchmarkPreparedRepresentation separates actual encoder time from the
// revalidation/storage path. Informational metrics impose no timing threshold.
func BenchmarkPreparedRepresentation(b *testing.B) {
	_, source, binding, opts := preparedTestSource(b, testFile(), transfer.RLELossless, false)
	started := time.Now()
	if _, err := source.Inspect(context.Background()); err != nil {
		b.Fatal(err)
	}
	initial := time.Since(started)
	calls := binding.EncoderCalls()
	encode := binding.EncoderTime()
	timingBefore := source.Timing()
	b.ReportMetric(float64(initial.Nanoseconds()), "initial-prepare+validate-ns")
	b.ReportMetric(float64(encode.Nanoseconds()), "initial-encoder-ns")
	b.ReportMetric(float64(source.session.Stats().RetainedBytes), "retained-bytes")
	b.ResetTimer()
	for range b.N {
		opened, err := source.Open(context.Background())
		if err != nil {
			b.Fatal(err)
		}
		if err := opened.WriteDataSet(context.Background(), io.Discard, opts.Target); err != nil {
			b.Fatal(err)
		}
		if err := opened.Close(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	timingAfter := source.Timing()
	b.ReportMetric(float64((timingAfter.OriginalValidation-timingBefore.OriginalValidation).Nanoseconds())/float64(b.N), "original-revalidate-ns/op")
	b.ReportMetric(float64((timingAfter.SpoolIntegrity-timingBefore.SpoolIntegrity).Nanoseconds())/float64(b.N), "spool-integrity-ns/op")
	b.ReportMetric(float64((timingAfter.Copy-timingBefore.Copy).Nanoseconds())/float64(b.N), "storage+destination-copy-ns/op")
	if binding.EncoderCalls() != calls {
		b.Fatal("reuse calledencoder")
	}
}
