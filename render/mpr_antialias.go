package render

import (
	"context"
	"math"
)

// Use one existing source-owned level. Conservative admission avoids averaging
// a coarse slice axis when only an in-plane axis is undersampled. Odd axes are
// kept exact because floor-sized BinShrink would discard the final source bin.
func mprAntialiasVolume(ctx context.Context, source *Volume, plane Plane, width, height int, limits MPRLimits) (*Volume, bool, error) {
	if source.closed.Load() {
		return nil, false, ErrVolumeStoreClosed
	}
	if !source.regular || source.geometry.RequiresResampling || width < 2 || height < 2 {
		return source, false, nil
	}
	coarsest := math.Max(source.ColSpacing, math.Max(source.RowSpacing, source.SliceSpacing))
	spacing := plane.PixelSpacingMM(width, height)
	if coarsest <= 0 || math.IsNaN(coarsest) || math.IsInf(coarsest, 0) || spacing.X < 2*coarsest || spacing.Y < 2*coarsest {
		return source, false, nil
	}
	sizes := [3]int{source.Cols, source.Rows, source.Depth}
	shrinks := false
	for axis, size := range sizes {
		if size >= minInteractiveShrinkAxis {
			if size%2 != 0 {
				return source, false, nil
			}
			sizes[axis] /= 2
			shrinks = true
		}
	}
	if !shrinks {
		return source, false, nil
	}
	count, ok := checkedVoxelCount(sizes[0], sizes[1], sizes[2])
	// Admission includes the cached float32 payload and retained scalar output.
	// The already-owned source and runtime/metadata allocations are not included.
	outputBytes := int64(width) * int64(height) * 9
	if !ok || int64(count) > (limits.MaxWorkingBytes-outputBytes)/4 {
		return nil, false, limitedMPRField("Antialias2x")
	}
	level, err := source.InteractiveVolumeContext(ctx)
	if err != nil {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	return level, level != source, nil
}
