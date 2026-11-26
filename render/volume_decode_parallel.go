package render

import (
	"context"
	"sync/atomic"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

// decodeVolumeSlices joins every worker before its caller can publish or drop
// the destination. Slices own disjoint planes; progressive observers retain
// their separate serial publication order. Tiny volumes avoid helper overhead.
func decodeVolumeSlices(ctx context.Context, slices, voxels int, decode func(int) error) error {
	workers := 0
	if voxels < 64<<10 {
		workers = 1
	}
	errors := make([]error, slices)
	var first atomic.Int64
	first.Store(int64(slices))
	// Volume preparation runs ahead of any view, so it yields to interactive
	// rendering (#1613); a view waiting on it is not slowed by its own work.
	err := parallel.DoBackground(ctx, slices, workers, func(index int) {
		if int64(index) > first.Load() {
			return
		}
		if err := decode(index); err != nil {
			errors[index] = err
			for previous := first.Load(); int64(index) < previous; previous = first.Load() {
				if first.CompareAndSwap(previous, int64(index)) {
					break
				}
			}
		}
	})
	if err != nil {
		return err
	}
	// All lower indexes were claimed before the failing index. Return the
	// first source-order error regardless of worker completion order.
	if index := int(first.Load()); index < slices {
		return errors[index]
	}
	return nil
}
