package imagebuild

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/commontest"
	"github.com/ThalesMMS/dicom-go/object"
)

// Original synthetic input, allocated before timing: both paths retain exactly
// the same caller-owned float64 volume. No corpus or external implementation.
func incrementalBenchInput(count int) (Geometry, []Slice, Options) {
	g := Geometry{Rows: 512, Columns: 512, PixelSpacing: [2]float64{1, 1}, RowDirection: [3]float64{1, 0, 0}, ColumnDirection: [3]float64{0, 1, 0}, SliceThickness: 2, FrameOfReferenceUID: "1.2.1483.900"}
	slices := make([]Slice, count)
	opts := Options{IOD: CT, Common: object.DerivedCommon{Source: commontest.Source()}, SeriesInstanceUID: "1.2.1483.901", ContentTime: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}
	for z := range slices {
		slices[z] = Slice{Position: [3]float64{0, 0, float64(z) * 2}, Values: make([]float64, 512*512)}
		for p := range slices[z].Values {
			slices[z].Values[p] = float64((z*65537+p)%100000)*.031 - 70.5
		}
		opts.SOPInstanceUIDs = append(opts.SOPInstanceUIDs, fmt.Sprintf("1.2.1483.902.%d", z+1))
	}
	return g, slices, opts
}
func runIncrementalBench(mode string, g Geometry, slices []Slice, opts Options) error {
	if mode == "whole" {
		files, err := Build(g, slices, opts)
		if err != nil {
			return err
		}
		defer func() {
			for _, f := range files {
				_ = f.Close()
			}
		}()
		for _, f := range files {
			if err := object.WriteFile(io.Discard, f); err != nil {
				return err
			}
		}
		return nil
	}
	_, err := WriteSeries(context.Background(), g, &streamTestSlices{slices: slices}, opts, StreamOptions{MaxSlices: 512, MaxPixelBufferBytes: 10 * 512 * 512, Replayable: true}, func(_ int, write func(io.Writer) error) error { return write(io.Discard) })
	return err
}
func BenchmarkIncrementalSeries(b *testing.B) {
	for _, count := range []int{32, 128, 512} {
		g, slices, opts := incrementalBenchInput(count)
		for _, mode := range []string{"whole", "stream"} {
			b.Run(fmt.Sprintf("%d/%s", count, mode), func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if err := runIncrementalBench(mode, g, slices, opts); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(count*8*512*512), "caller-B")
				window := 2 * 512 * 512
				if mode == "whole" {
					window *= count
				}
				b.ReportMetric(float64(window), "encoded-window-B")
			})
		}
		runtime.KeepAlive(slices)
	}
}

// This opt-in one-shot test is run in fresh processes to measure Windows peak
// working set externally. Heap sampling is an observed peak, not an RSS claim.
func TestWriteSeriesMemoryCampaign(t *testing.T) {
	mode := os.Getenv("DICOM_INCREMENTAL_SERIES_MODE")
	if mode == "" {
		t.Skip("opt-in fresh-process memory campaign")
	}
	if mode != "whole" && mode != "stream" {
		t.Fatal("invalid campaign mode")
	}
	count, err := strconv.Atoi(os.Getenv("DICOM_INCREMENTAL_SERIES_COUNT"))
	if err != nil || count < 1 || count > 512 {
		t.Fatal("invalid campaign count")
	}
	g, slices, opts := incrementalBenchInput(count)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	var peak atomic.Uint64
	peak.Store(before.HeapAlloc)
	done, joined := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(joined)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				var sample runtime.MemStats
				runtime.ReadMemStats(&sample)
				for old := peak.Load(); sample.HeapAlloc > old; old = peak.Load() {
					if peak.CompareAndSwap(old, sample.HeapAlloc) {
						break
					}
				}
			}
		}
	}()
	start := time.Now()
	err = runIncrementalBench(mode, g, slices, opts)
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	close(done)
	<-joined
	runtime.KeepAlive(slices)
	if err != nil {
		t.Fatal(err)
	}
	window := 2 * 512 * 512
	if mode == "whole" {
		window *= count
	}
	data, _ := json.Marshal(map[string]any{"mode": mode, "slices": count, "caller_pixel_bytes": count * 8 * 512 * 512, "encoded_window_bytes": window, "writer_allocated_bytes": after.TotalAlloc - before.TotalAlloc, "writer_allocations": after.Mallocs - before.Mallocs, "observed_heap_peak_bytes": max(peak.Load(), after.HeapAlloc), "heap_before_bytes": before.HeapAlloc, "elapsed_ms": float64(elapsed) / float64(time.Millisecond), "go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH})
	fmt.Println(string(data))
}
