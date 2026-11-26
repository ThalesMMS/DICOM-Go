package seg

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

	"github.com/ThalesMMS/dicom-go/object"
)

func incrementalSEGBenchInput(count int) *Document {
	doc := streamingSEGFixture("fractional")
	doc.Rows, doc.Columns = 512, 512
	prototype := doc.Frames[0]
	doc.Frames = make([]Frame, count)
	doc.ReferencedImages[0].Frames = nil
	for z := range doc.Frames {
		frame := prototype
		frame.SliceIndex = z
		frame.ReferencedFrameNumber = z + 1
		frame.Geometry.Rows, frame.Geometry.Columns = 512, 512
		frame.Geometry.Origin.Z = float64(z) * 3
		frame.Fractional = make([]uint8, 512*512)
		for i := range frame.Fractional {
			frame.Fractional[i] = uint8((z + i) % 201)
		}
		doc.Frames[z] = frame
		doc.ReferencedImages[0].Frames = append(doc.ReferencedImages[0].Frames, z+1)
	}
	return doc
}
func runIncrementalSEGBench(mode string, doc *Document) error {
	if mode == "whole" {
		file, err := Write(doc)
		if err != nil {
			return err
		}
		defer file.Close()
		return object.WriteFile(io.Discard, file)
	}
	_, err := WriteFrames(context.Background(), io.Discard, doc, DocumentFrames(doc), StreamWriteOptions{MaxFrames: 512, MaxFrameReads: 1024, MaxPixelBufferBytes: 1 << 20, MaxMetadataBytes: 128 << 20})
	return err
}
func BenchmarkIncrementalSEG(b *testing.B) {
	for _, count := range []int{32, 128, 512} {
		doc := incrementalSEGBenchInput(count)
		for _, mode := range []string{"whole", "stream"} {
			b.Run(fmt.Sprintf("%d/%s", count, mode), func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if err := runIncrementalSEGBench(mode, doc); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(count*512*512), "caller-B")
				window := 512*512 + 2*(512*512/8) + (32 << 10)
				if mode == "whole" {
					window = count * 512 * 512
				}
				b.ReportMetric(float64(window), "pixel-window-B")
			})
		}
		runtime.KeepAlive(doc)
	}
}

func TestSEGWriteFramesMemoryCampaign(t *testing.T) {
	mode := os.Getenv("DICOM_INCREMENTAL_SEG_MODE")
	if mode == "" {
		t.Skip("opt-in fresh-process memory campaign")
	}
	if mode != "whole" && mode != "stream" {
		t.Fatal("invalid campaign mode")
	}
	count, err := strconv.Atoi(os.Getenv("DICOM_INCREMENTAL_SEG_COUNT"))
	if err != nil || count < 1 || count > 512 {
		t.Fatal("invalid campaign count")
	}
	doc := incrementalSEGBenchInput(count)
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
	err = runIncrementalSEGBench(mode, doc)
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	close(done)
	<-joined
	runtime.KeepAlive(doc)
	if err != nil {
		t.Fatal(err)
	}
	window := 512*512 + 2*(512*512/8) + (32 << 10)
	if mode == "whole" {
		window = count * 512 * 512
	}
	data, _ := json.Marshal(map[string]any{"mode": mode, "frames": count, "caller_pixel_bytes": count * 512 * 512, "pixel_window_bytes": window, "writer_allocated_bytes": after.TotalAlloc - before.TotalAlloc, "writer_allocations": after.Mallocs - before.Mallocs, "observed_heap_peak_bytes": max(peak.Load(), after.HeapAlloc), "heap_before_bytes": before.HeapAlloc, "elapsed_ms": float64(elapsed) / float64(time.Millisecond), "go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH})
	fmt.Println(string(data))
}
