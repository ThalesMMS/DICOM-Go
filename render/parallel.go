package render

import (
	"context"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

const (
	// parallelRowsMinRows keeps tiny outputs on the calling goroutine, where
	// spawning workers costs more than it saves.
	parallelRowsMinRows = 32
	// parallelRowChunksPerWorker is the average number of row chunks each
	// worker claims. Several small chunks per worker let the workers that draw
	// cheap rows (rays that miss the volume, empty MPR borders) keep pulling
	// work while others are still inside expensive rows, instead of idling
	// behind a fixed contiguous share (#1061).
	parallelRowChunksPerWorker = 8
)

// BeginInteractiveWork marks rendering a user is waiting for, such as a 2D
// frame during a scroll, until the returned end function is called. Volume
// preparation running meanwhile returns its helper goroutines to the shared
// concurrency budget and pauses, and resumes once interaction has been quiet
// for a short period (#1613).
func BeginInteractiveWork() (end func()) { return parallel.BeginInteractive() }

// parallelRows renders independent output rows on the calling goroutine plus
// helpers from the shared concurrency budget (GOMAXPROCS workers by default).
// Rows are scheduled dynamically: workers claim small contiguous chunks from a
// shared atomic cursor until none remain, so an uneven row cost (an object that
// covers only a band of the viewport) no longer leaves most workers idle. Every
// row is rendered exactly once and rows write disjoint output, so the result
// does not depend on which worker renders a row.
func parallelRows(rowCount int, renderRow func(int)) {
	runParallelRows(nil, rowCount, func() func(int) { return renderRow })
}

// parallelRowsContext renders independent output rows with the shared bounded
// worker pool while honoring cancellation between rows. Workers that observe a
// canceled context stop claiming rows; the caller receives ctx.Err() only after
// every started worker has stopped touching the output buffer.
func parallelRowsContext(ctx context.Context, rowCount int, renderRow func(int)) error {
	return parallelRowsWorkerContext(ctx, rowCount, func() func(int) { return renderRow })
}

// parallelRowsWorkerContext is parallelRowsContext with per-worker state:
// newWorker is called once by each worker that claims rows (once in total on
// the serial path) and returns that worker's row renderer, so scratch such as
// a gradient cache is owned by one goroutine and needs neither locking nor a
// pool.
func parallelRowsWorkerContext(ctx context.Context, rowCount int, newWorker func() func(int)) error {
	if ctx == nil {
		ctx = context.Background()
	}
	runParallelRows(ctx, rowCount, newWorker)
	return ctx.Err()
}

// runParallelRows is the shared scheduler; a nil ctx disables cancellation.
// The workers beyond the calling goroutine come from the module's shared
// concurrency budget (internal/parallel), so concurrent renders and filters
// together stay within it (#1188).
func runParallelRows(ctx context.Context, rowCount int, newWorker func() func(int)) {
	if rowCount <= 0 {
		return
	}
	workers := parallel.DefaultWorkers()
	if rowCount < parallelRowsMinRows {
		workers = 1
	}
	workers = min(workers, rowCount)
	_ = parallel.Run(ctx, rowCount, workers, parallelRowChunk(rowCount, workers), newWorker)
}

// parallelRowChunk returns the number of contiguous rows a worker claims at a
// time: about parallelRowChunksPerWorker chunks per worker, at least one row.
func parallelRowChunk(rowCount, workers int) int {
	chunks := workers * parallelRowChunksPerWorker
	chunk := (rowCount + chunks - 1) / chunks
	if chunk < 1 {
		chunk = 1
	}
	return chunk
}
