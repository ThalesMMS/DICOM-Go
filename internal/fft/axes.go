package fft

import (
	"context"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

// ElementsPerWorker is the transform size each worker of TransformAxes
// needs: ITK found that below about 2^18 elements per thread the extra
// threads of an N-D FFT cost more than they save (3f184df2c0, #2025).
const ElementsPerWorker = 1 << 18

// AxisWorkers is the most workers TransformAxes uses for n elements when its
// caller allows at most workers (at least one; workers <= 0 means no bound
// beyond the size): one per ElementsPerWorker elements.
func AxisWorkers(n, workers int) int {
	w := max(1, n/ElementsPerWorker)
	if workers > 0 {
		w = min(w, workers)
	}
	return w
}

// GatherLength is the longest line TransformAxes gathers into scratch for
// size: the y or z length, since x lines are transformed in place.
func GatherLength(size [3]int) int {
	longest := 0
	for axis := 1; axis < 3; axis++ {
		if size[axis] > 1 {
			longest = max(longest, size[axis])
		}
	}
	return longest
}

// TransformAxes is the unscaled forward or inverse transform of data, an
// x-fastest size[0] × size[1] × size[2] array whose axes longer than one are
// powers of two, along each such axis in turn. The lines of an axis are
// transformed on up to AxisWorkers(len(data), workers) workers, the calling
// goroutine and helpers from the module's shared concurrency budget
// (workers <= 0 bounds them by the budget alone). x lines are transformed in
// place; y and z lines are gathered into one scratch line of GatherLength
// elements per worker. Every line is transformed exactly as it would be
// serially, so the result does not depend on the workers. Cancellation
// leaves data partially transformed: the caller discards it.
func TransformAxes(ctx context.Context, data []complex128, size [3]int, inverse bool, workers int) error {
	if ctx == nil {
		ctx = context.Background()
	}
	workers = AxisWorkers(len(data), parallel.Workers(workers))
	gather := GatherLength(size)
	stride := 1
	for axis := 0; axis < 3; axis++ {
		n := size[axis]
		if n < 2 {
			stride *= n
			continue
		}
		block := stride * n
		lines := len(data) / n
		axisStride := stride
		var err error
		if axis == 0 {
			err = parallel.Run(ctx, lines, workers, 0, func() func(int) {
				return func(line int) {
					_ = transformContext(ctx, data[line*n:(line+1)*n], inverse)
				}
			})
		} else {
			err = parallel.Run(ctx, lines, workers, 0, func() func(int) {
				scratch := make([]complex128, gather)
				return func(line int) {
					// The line's first element: its block, then its offset
					// within the block.
					start := line/axisStride*block + line%axisStride
					v := scratch[:n]
					for k := range v {
						v[k] = data[start+k*axisStride]
					}
					if transformContext(ctx, v, inverse) != nil {
						return
					}
					for k, value := range v {
						data[start+k*axisStride] = value
					}
				}
			})
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		stride = block
	}
	return ctx.Err()
}
