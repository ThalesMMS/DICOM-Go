package render

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestParallelRowsRenderEveryRowOnce(t *testing.T) {
	original := runtime.GOMAXPROCS(0)
	defer runtime.GOMAXPROCS(original)
	for _, procs := range []int{1, 2, 3, 7, 16} {
		runtime.GOMAXPROCS(procs)
		for _, rows := range []int{0, 1, 31, 32, 33, 100, 257, 1024} {
			counts := make([]atomic.Int32, rows)
			parallelRows(rows, func(row int) { counts[row].Add(1) })
			for row := range counts {
				if got := counts[row].Load(); got != 1 {
					t.Fatalf("GOMAXPROCS=%d rows=%d: row %d rendered %d times, want 1", procs, rows, row, got)
				}
			}
		}
	}
}

func TestParallelRowChunkSplitsWorkIntoSeveralChunksPerWorker(t *testing.T) {
	for _, c := range []struct{ rows, workers, want int }{
		{32, 28, 1},
		{256, 28, 2},
		{512, 8, 8},
		{1000, 4, 32},
		{5, 16, 1},
	} {
		if got := parallelRowChunk(c.rows, c.workers); got != c.want {
			t.Fatalf("parallelRowChunk(%d, %d) = %d, want %d", c.rows, c.workers, got, c.want)
		}
	}
}

// TestParallelRowsSchedulesDynamically stalls the first row until another
// worker has rendered the second quarter of the rows. A static contiguous
// split gives that quarter to the stalled worker and never finishes; dynamic
// scheduling lets the other worker claim it.
func TestParallelRowsSchedulesDynamically(t *testing.T) {
	original := runtime.GOMAXPROCS(2)
	defer runtime.GOMAXPROCS(original)
	const rows = 64
	var pending atomic.Int32
	pending.Store(rows / 4)
	released := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		parallelRows(rows, func(row int) {
			if row == 0 {
				select {
				case <-released:
				case <-time.After(10 * time.Second):
				}
				return
			}
			if row >= rows/4 && row < rows/2 && pending.Add(-1) == 0 {
				close(released)
			}
		})
	}()
	select {
	case <-released:
	case <-time.After(10 * time.Second):
		t.Fatal("rows behind a stalled row were never claimed by another worker")
	}
	<-finished
}

func TestParallelRowsWorkerContextGivesEachWorkerPrivateState(t *testing.T) {
	original := runtime.GOMAXPROCS(0)
	defer runtime.GOMAXPROCS(original)
	procs := minIntForRenderTest(8, runtime.NumCPU())
	if procs < 2 {
		procs = 2
	}
	runtime.GOMAXPROCS(procs)
	const rows = 999
	var workers atomic.Int32
	var rendered atomic.Int32
	err := parallelRowsWorkerContext(context.Background(), rows, func() func(int) {
		workers.Add(1)
		var busy atomic.Bool
		return func(int) {
			if !busy.CompareAndSwap(false, true) {
				t.Error("worker state used by two goroutines at once")
			}
			rendered.Add(1)
			runtime.Gosched()
			busy.Store(false)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := workers.Load(); got < 1 || int(got) > procs {
		t.Fatalf("newWorker called %d times, want 1..%d", got, procs)
	}
	if got := rendered.Load(); got != rows {
		t.Fatalf("rendered %d rows, want %d", got, rows)
	}
}

func TestParallelRowsContextStopsPromptlyAfterCancel(t *testing.T) {
	original := runtime.GOMAXPROCS(0)
	defer runtime.GOMAXPROCS(original)
	for _, procs := range []int{1, 4} {
		runtime.GOMAXPROCS(procs)
		ctx, cancel := context.WithCancel(context.Background())
		var canceled atomic.Bool
		var lateStarts, rendered atomic.Int32
		const rows = 4096
		err := parallelRowsContext(ctx, rows, func(row int) {
			if canceled.Load() {
				lateStarts.Add(1)
			}
			if rendered.Add(1) == 100 {
				cancel()
				canceled.Store(true)
			}
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("GOMAXPROCS=%d: error = %v, want context.Canceled", procs, err)
		}
		// A worker may already be past its cancellation check when cancel
		// lands, so at most one late row per other worker may start.
		if got := lateStarts.Load(); int(got) >= procs {
			t.Fatalf("GOMAXPROCS=%d: %d rows started after cancel", procs, got)
		}
		if got := rendered.Load(); got >= rows {
			t.Fatalf("GOMAXPROCS=%d: rendered all %d rows despite cancel", procs, got)
		}
	}
}

func TestParallelRowsContextPreCanceledRendersNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var workers, rendered atomic.Int32
	err := parallelRowsWorkerContext(ctx, 512, func() func(int) {
		workers.Add(1)
		return func(int) { rendered.Add(1) }
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if workers.Load() != 0 || rendered.Load() != 0 {
		t.Fatalf("pre-canceled render created %d workers and rendered %d rows", workers.Load(), rendered.Load())
	}
}

// TestParallelVRUnbalancedMatchesSerialPixels renders the unbalanced benchmark
// scene serially and with dynamic scheduling; the pixels must be identical.
func TestParallelVRUnbalancedMatchesSerialPixels(t *testing.T) {
	if runtime.NumCPU() < 2 {
		t.Skip("parallel equivalence requires at least two CPUs")
	}
	vol, cam, window := unbalancedVRScene(t)
	quality := DefaultVRQuality(96, 80)
	jittered := quality
	jittered.Jitter = true
	surfaceColor := color.NRGBA{R: 220, G: 200, B: 160, A: 255}
	renders := []struct {
		name   string
		render func() image.Image
	}{
		{"dvr", func() image.Image {
			return RenderVR(vol, cam, translucentAbovePreset(), window, true, nil, quality)
		}},
		{"dvr-opaque-jitter", func() image.Image {
			return RenderVR(vol, cam, opaqueAbovePreset(), window, true, nil, jittered)
		}},
		{"mip", func() image.Image {
			return RenderVR(vol, cam, VRPreset{Name: "mip", Mode: VRModeMIP}, window, false, nil, quality)
		}},
		{"surface", func() image.Image {
			return RenderVRSurface(vol, cam, 128, surfaceColor, true, nil, quality)
		}},
	}
	original := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(original)
	serial := make([]image.Image, len(renders))
	for i, r := range renders {
		serial[i] = r.render()
	}
	runtime.GOMAXPROCS(minIntForRenderTest(8, runtime.NumCPU()))
	for i, r := range renders {
		want, wantOK := serial[i].(*image.NRGBA)
		got, gotOK := r.render().(*image.NRGBA)
		if !wantOK || !gotOK {
			t.Fatalf("%s image types = %T/%T, want *image.NRGBA", r.name, serial[i], got)
		}
		if want.Rect != got.Rect || !bytes.Equal(want.Pix, got.Pix) {
			t.Fatalf("%s parallel pixels differ from serial output", r.name)
		}
	}
}
