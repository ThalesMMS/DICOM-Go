package render

import (
	"fmt"
	"math"
)

// Components reports the acquired voxel component count. Color frames are
// decoded to three interleaved RGB values in [0,255], independently of VOI.
func (v *Volume) Components() uint32 {
	if v == nil || len(v.slices) == 0 {
		return 1
	}
	return frameVolumeComponents(v.slices[0])
}

func frameVolumeComponents(frame *Frame) uint32 {
	if frame != nil && effectiveFrameMetadata(frame).SamplesPerPixel == 3 {
		return 3
	}
	return 1
}

func (v *Volume) componentSampleCount() (int, error) {
	count, ok := checkedVoxelCount(v.Cols, v.Rows, v.Depth)
	components := int(v.Components())
	if !ok || count > math.MaxInt/components {
		return 0, fmt.Errorf("%w: volume component dimensions overflow", ErrInvalidVolumeSnapshot)
	}
	return count * components, nil
}
