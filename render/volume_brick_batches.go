package render

import (
	"fmt"
	"math"
)

// VolumeBrickSampleStencil returns every brick read by trilinear sampling at
// index, including a central-difference gradient when gradient is true. Border
// coordinates repeat the edge voxels, as in the volume sampler. The result is
// bounded by 64 entries, independent of the volume size. It describes reads;
// it does not sample voxels or compute a rendered contribution.
func VolumeBrickSampleStencil(grid VolumeBrickGrid, index [3]float32, gradient bool) ([]int, error) {
	if err := grid.Validate(); err != nil {
		return nil, err
	}
	margin := int64(0)
	if gradient {
		margin = 1
	}
	var low, high [3]uint32
	for axis, value := range index {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, fmt.Errorf("%w: non-finite sample coordinate", ErrInvalidVolumeBricks)
		}
		// Clamp before converting to an integer, including arbitrary finite
		// caller coordinates outside the volume.
		base := int64(math.Floor(min(max(float64(value), -2), float64(grid.Dimensions[axis])+1)))
		upper := int64(grid.Dimensions[axis]) - 1
		low[axis] = uint32(min(max(base-margin, 0), upper)) / grid.BrickSize[axis]
		high[axis] = uint32(min(max(base+1+margin, 0), upper)) / grid.BrickSize[axis]
	}
	counts := grid.Counts()
	result := make([]int, 0, int(high[0]-low[0]+1)*int(high[1]-low[1]+1)*int(high[2]-low[2]+1))
	for z := low[2]; z <= high[2]; z++ {
		for y := low[1]; y <= high[1]; y++ {
			for x := low[0]; x <= high[0]; x++ {
				result = append(result, int((z*counts[1]+y)*counts[0]+x))
			}
		}
	}
	return result, nil
}

// VolumeBrickBatchBudget limits a complete frame, including all revisits of
// evicted bricks. Callers charge a pass before dispatch and its upload before
// reading or allocating that upload. Zero limits admit no work.
type VolumeBrickBatchBudget struct {
	MaxPasses      uint32
	MaxUploadBytes uint64
}

// VolumeBrickBatchUsage is a monotonic per-frame accounting value. A rejected
// charge leaves it unchanged, so a backend can refuse a frame without exposing
// a partially accumulated image.
type VolumeBrickBatchUsage struct {
	Passes      uint32
	UploadBytes uint64
}

// Charge reserves the next pass and its uploads within budget.
func (u *VolumeBrickBatchUsage) Charge(budget VolumeBrickBatchBudget, uploadBytes uint64) error {
	if u == nil || u.Passes >= budget.MaxPasses || u.UploadBytes > budget.MaxUploadBytes || uploadBytes > budget.MaxUploadBytes-u.UploadBytes {
		return fmt.Errorf("%w: streamed frame pass/upload budget", ErrVolumeBrickBudget)
	}
	u.Passes++
	u.UploadBytes += uploadBytes
	return nil
}
