package roi

import (
	"context"
	"errors"
	"math"

	"github.com/ThalesMMS/dicom-go/render"
)

// VolumeVOIOptions configures VolumeVOI.
type VolumeVOIOptions struct {
	Raster VOIRasterOptions
	// Peak, when non-nil, also searches the peak sphere.
	Peak *PeakOptions
}

// VolumeVOIResult is a VOI of a render.Volume: its mask on the volume's
// canonical grid, the statistics of the modality values and, when requested,
// the peak.
type VolumeVOIResult struct {
	Mask       *VOIMask
	Statistics VOIStatistics
	Peak       *PeakResult
	// PeakErr explains a requested peak that is unavailable (for example
	// ErrNoPeak when every sphere leaves the image); the statistics remain
	// valid.
	PeakErr error
}

// VolumeVOI rasterizes a patient-space primitive on the canonical grid of a
// volume (the regular IndexToPatientLPS affine of its snapshot, the grid
// irregular or tilted series are resampled to) and computes the statistics
// of its modality values (HU for CT, the stored unit for PET). The values
// the VOI and the peak sphere can reach are read once into a local cache
// under one snapshot lease.
func VolumeVOI(ctx context.Context, volume *render.Volume, primitive VOIPrimitive, opts VolumeVOIOptions) (VolumeVOIResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if volume == nil {
		return VolumeVOIResult{}, errors.New("roi: volume VOI needs a volume")
	}
	lease, err := volume.AcquireSnapshotContext(ctx)
	if err != nil {
		return VolumeVOIResult{}, err
	}
	defer func() { _ = lease.Release() }()
	snapshot, err := lease.Snapshot()
	if err != nil {
		return VolumeVOIResult{}, err
	}
	descriptor, err := snapshot.Descriptor()
	if err != nil {
		return VolumeVOIResult{}, err
	}
	grid, err := VoxelGridFromDescriptor(descriptor)
	if err != nil {
		return VolumeVOIResult{}, err
	}
	mask, err := RasterizeVOI(ctx, grid, primitive, opts.Raster)
	if err != nil {
		return VolumeVOIResult{}, err
	}
	result := VolumeVOIResult{Mask: mask}
	if len(mask.Voxels) == 0 {
		return result, ErrEmptyVOI
	}
	margin := 0.0
	if opts.Peak != nil {
		volumeMM3 := opts.Peak.VolumeMM3
		if volumeMM3 == 0 {
			volumeMM3 = DefaultPeakVolumeMM3
		}
		if finitePositive(volumeMM3) {
			margin = SphereRadiusForVolume(volumeMM3)
		}
	}
	valueAt, err := cachedVoxelValues(ctx, snapshot, grid, mask, margin)
	if err != nil {
		return result, err
	}
	result.Statistics, err = mask.Statistics(valueAt)
	if err != nil {
		return result, err
	}
	if opts.Peak != nil {
		peak, peakErr := mask.Peak(ctx, valueAt, *opts.Peak)
		if peakErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return result, ctxErr
			}
			result.PeakErr = peakErr
		} else {
			result.Peak = &peak
		}
	}
	return result, nil
}

// cachedVoxelValues reads the voxels of the mask's index box widened by
// marginMM into memory; voxels outside the cached box are read from the
// snapshot directly.
func cachedVoxelValues(ctx context.Context, snapshot render.VolumeSnapshot, grid VoxelGrid, mask *VOIMask, marginMM float64) (VoxelValueFunc, error) {
	t, err := grid.transform()
	if err != nil {
		return nil, err
	}
	box := indexBox{lo: [3]int{math.MaxInt, math.MaxInt, math.MaxInt}, hi: [3]int{math.MinInt, math.MinInt, math.MinInt}}
	for _, v := range mask.Voxels {
		for axis, index := range [3]int{v.Column, v.Row, v.Slice} {
			box.lo[axis] = min(box.lo[axis], index)
			box.hi[axis] = max(box.hi[axis], index)
		}
	}
	dims := [3]int{grid.Columns, grid.Rows, grid.Slices}
	for axis := range dims {
		// A sphere of radius r spans r·|row of the inverse| indices per axis.
		extent := int(math.Ceil(marginMM*t.inverse[axis].Length())) + 1
		if marginMM == 0 {
			extent = 0
		}
		box.lo[axis] = max(0, box.lo[axis]-extent)
		box.hi[axis] = min(dims[axis]-1, box.hi[axis]+extent)
	}
	direct := func(i, j, k int) (float64, bool) {
		if !t.inGrid(i, j, k) {
			return 0, false
		}
		return snapshot.ModalityAt(uint32(i), uint32(j), uint32(k))
	}
	if box.count() > MaxVOIVoxels {
		return direct, nil
	}
	nx, ny := box.hi[0]-box.lo[0]+1, box.hi[1]-box.lo[1]+1
	cache := make([]float64, box.count())
	n := 0
	for k := box.lo[2]; k <= box.hi[2]; k++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for j := box.lo[1]; j <= box.hi[1]; j++ {
			for i := box.lo[0]; i <= box.hi[0]; i++ {
				v, ok := snapshot.ModalityAt(uint32(i), uint32(j), uint32(k))
				if !ok {
					v = math.NaN()
				}
				cache[n] = v
				n++
			}
		}
	}
	return func(i, j, k int) (float64, bool) {
		if i < box.lo[0] || j < box.lo[1] || k < box.lo[2] || i > box.hi[0] || j > box.hi[1] || k > box.hi[2] {
			return direct(i, j, k)
		}
		v := cache[((k-box.lo[2])*ny+(j-box.lo[1]))*nx+(i-box.lo[0])]
		return v, !math.IsNaN(v)
	}, nil
}
