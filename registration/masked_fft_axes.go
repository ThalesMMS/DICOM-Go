package registration

import (
	"context"

	"github.com/ThalesMMS/dicom-go/internal/fft"
)

// maskedFFTAxes transforms along all three axes, on up to workers workers
// (one per fft.ElementsPerWorker voxels), each with its own line scratch.
// Inverse normalization occurs exactly once, after all three unscaled
// one-dimensional transforms.
func maskedFFTAxes(ctx context.Context, a []complex128, size [3]int, inverse bool, workers int) error {
	if err := fft.TransformAxes(ctx, a, size, inverse, workers); err != nil {
		return err
	}
	if inverse {
		scale := complex(1/float64(len(a)), 0)
		for i := range a {
			if i&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			a[i] *= scale
		}
	}
	return ctx.Err()
}
