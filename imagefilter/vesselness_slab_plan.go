package imagefilter

import (
	"fmt"
	"math"
)

// PlanVesselnessSlabSlices fixes the mathematical recursion domains using only
// dimensions and the working bound. Device output partitions may subdivide a
// slab, but must retain the same extended line endpoints to preserve results.
func PlanVesselnessSlabSlices(dims [3]int, workingBytes int64) (int, error) {
	if dims[0] <= 0 || dims[1] <= 0 || dims[2] <= 0 || workingBytes < 0 {
		return 0, fmt.Errorf("%w: invalid vesselness slab metadata", ErrInvalidVolume)
	}
	if int64(dims[0]) > math.MaxInt64/int64(dims[1])/vesselnessSlabBuffers/4 {
		return 0, fmt.Errorf("%w: vesselness slab byte count overflows", ErrInvalidVolume)
	}
	if workingBytes == 0 {
		workingBytes = DefaultVesselnessWorkingBytes
	}
	sliceBytes := int64(dims[0]) * int64(dims[1]) * vesselnessSlabBuffers * 4
	return int(max(1, min(int64(dims[2]), workingBytes/sliceBytes))), nil
}
