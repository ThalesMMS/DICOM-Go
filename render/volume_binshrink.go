package render

import (
	"context"
	"fmt"
)

// Interactive volume pyramid (#1066). BinShrinkVolumeContext is ITK's
// BinShrinkImageFilter: each output voxel averages a block of f voxels per
// axis, the output has floor(n/f) voxels and f times the spacing, and its
// origin moves (f-1)/2 input voxels along each axis so every output voxel
// sits at the centre of its block. Volume.InteractiveVolumeContext keeps the
// 2× level for interaction, VTK's vtkSmartVolumeMapper LowResModeResample
// (written from their documented behaviour, no code is copied).

// BinShrinkVolumeContext returns a new volume whose voxels average the
// factors[0]×factors[1]×factors[2] blocks of v's regular grid (irregular
// geometry is regularized first). Factors below 1 count as 1, and an axis
// shorter than its factor keeps one voxel. The result owns its own store:
// Close it when done.
func BinShrinkVolumeContext(ctx context.Context, v *Volume, factors [3]int) (*Volume, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if v == nil || v.Cols <= 0 || v.Rows <= 0 || v.Depth <= 0 {
		return nil, fmt.Errorf("render: cannot shrink an empty volume")
	}
	source := v
	if v.geometry.RequiresResampling {
		grid, err := v.RegularGrid()
		if err != nil {
			return nil, err
		}
		source = grid
	}
	sampler, err := newVolumeSamplerWithError(source)
	if err != nil {
		return nil, err
	}
	defer sampler.Close()
	sizes := [3]int{source.Cols, source.Rows, source.Depth}
	var out [3]int
	for axis := range factors {
		factors[axis] = max(1, min(factors[axis], sizes[axis]))
		out[axis] = max(1, sizes[axis]/factors[axis])
	}
	cols, rows, depth := out[0], out[1], out[2]
	count, ok := checkedVoxelCount(cols, rows, depth)
	if !ok {
		return nil, fmt.Errorf("render: shrunk volume dimensions overflow")
	}
	values := make([]float32, count)
	fx, fy, fz := factors[0], factors[1], factors[2]
	block := float64(fx * fy * fz)
	err = parallelRowsContext(ctx, depth, func(z int) {
		for y := 0; y < rows; y++ {
			for x := 0; x < cols; x++ {
				sum := 0.0
				for dz := 0; dz < fz; dz++ {
					for dy := 0; dy < fy; dy++ {
						for dx := 0; dx < fx; dx++ {
							value, _ := sampler.valueAt(x*fx+dx, y*fy+dy, z*fz+dz)
							sum += value
						}
					}
				}
				values[(z*rows+y)*cols+x] = float32(sum / block)
			}
		}
	})
	if err != nil {
		return nil, err
	}

	colSpacing := source.ColSpacing * float64(fx)
	rowSpacing := source.RowSpacing * float64(fy)
	sliceSpacing := source.SliceSpacing * float64(fz)
	origin := source.Origin.
		Add(source.AxisX.Scale(float64(fx-1) / 2 * source.ColSpacing)).
		Add(source.AxisY.Scale(float64(fy-1) / 2 * source.RowSpacing)).
		Add(source.Normal.Scale(float64(fz-1) / 2 * source.SliceSpacing))
	return newDerivedFloat32Volume(source, cols, rows, depth, origin, colSpacing, rowSpacing, sliceSpacing, values)
}

// interactiveShrinkFactor is the pyramid level kept for interaction; axes
// shorter than minInteractiveShrinkAxis voxels are not shrunk.
const (
	interactiveShrinkFactor  = 2
	minInteractiveShrinkAxis = 2 * interactiveShrinkFactor
)

// InteractiveVolumeContext returns the 2× BinShrink level of the volume for
// interaction frames, built on first use and owned by v (closed with it).
// Volumes with no axis long enough to shrink return v itself.
func (v *Volume) InteractiveVolumeContext(ctx context.Context) (*Volume, error) {
	if v == nil {
		return nil, fmt.Errorf("render: nil volume")
	}
	if v.closed.Load() {
		return nil, ErrVolumeStoreClosed
	}
	var factors [3]int
	shrinks := false
	for axis, n := range [3]int{v.Cols, v.Rows, v.Depth} {
		factors[axis] = 1
		if n >= minInteractiveShrinkAxis {
			factors[axis] = interactiveShrinkFactor
			shrinks = true
		}
	}
	if !shrinks {
		return v, nil
	}
	v.interactiveMu.Lock()
	defer v.interactiveMu.Unlock()
	if v.interactive != nil {
		return v.interactive, nil
	}
	shrunk, err := BinShrinkVolumeContext(ctx, v, factors)
	if err != nil {
		return nil, err
	}
	if v.closed.Load() {
		_ = shrunk.Close()
		return nil, ErrVolumeStoreClosed
	}
	v.interactive = shrunk
	return shrunk, nil
}

// closeInteractiveVolume closes the cached interaction level.
func (v *Volume) closeInteractiveVolume() error {
	v.interactiveMu.Lock()
	shrunk := v.interactive
	v.interactive = nil
	v.interactiveMu.Unlock()
	if shrunk == nil {
		return nil
	}
	return shrunk.Close()
}

// interactiveVolumeBytes is the interaction level's voxel memory, for tests.
func (v *Volume) interactiveVolumeBytes() uint64 {
	v.interactiveMu.Lock()
	defer v.interactiveMu.Unlock()
	if v.interactive == nil {
		return 0
	}
	count, _ := checkedVoxelCount(v.interactive.Cols, v.interactive.Rows, v.interactive.Depth)
	return uint64(count) * 4
}
