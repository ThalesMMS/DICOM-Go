package mesh

import (
	"fmt"
	"math"
	"slices"

	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/roi"
	"github.com/ThalesMMS/dicom-go/seg"
)

// Label volumes for ExtractLabels from segmentations (#1013): per-label
// masks of roi (GrowCut, region growing, edited masks) and DICOM SEG
// documents resampled with seg.Resample.

// LabelFieldFromMasks builds a label volume on grid from per-label masks,
// masks[label][z] covering slice z (nil or short slices are empty). Where
// labels overlap the highest label wins; overlaps counts those voxels.
// Label 0 is the background and is ignored.
func LabelFieldFromMasks(grid roi.Grid, masks map[uint32][]*roi.RasterMask) (field *DenseLabelField, overlaps int, err error) {
	affine, err := labelGridAffine(grid)
	if err != nil {
		return nil, 0, err
	}
	count := grid.VoxelCount()
	if count > math.MaxInt32 {
		return nil, 0, fmt.Errorf("%w: %d voxels", ErrInvalidField, count)
	}
	field = &DenseLabelField{Size: [3]int{grid.Columns, grid.Rows, grid.Slices}, Affine: affine, Labels: make([]uint32, count)}
	labels := make([]uint32, 0, len(masks))
	for label := range masks {
		if label != 0 {
			labels = append(labels, label)
		}
	}
	slices.Sort(labels)
	plane := grid.Columns * grid.Rows
	for _, label := range labels {
		for z, mask := range masks[label] {
			if mask == nil || z >= grid.Slices {
				continue
			}
			if mask.Columns != grid.Columns || mask.Rows != grid.Rows {
				return nil, 0, fmt.Errorf("%w: label %d slice %d mask is %dx%d on a %dx%d grid", ErrInvalidField, label, z, mask.Columns, mask.Rows, grid.Columns, grid.Rows)
			}
			base := field.Labels[z*plane : (z+1)*plane]
			mask.ForEachRun(func(y int, run roi.MaskRun) {
				for x := run.Start; x < run.End; x++ {
					if base[y*grid.Columns+x] != 0 {
						overlaps++
					}
					base[y*grid.Columns+x] = label
				}
			})
		}
	}
	return field, overlaps, nil
}

// LabelFieldFromSEG builds the label volume of a segmentation resampled on
// a grid (seg.Resample): each segment's number is its label, and where
// segments overlap the highest number wins (overlaps counts those voxels).
func LabelFieldFromSEG(resampled *seg.Resampled) (field *DenseLabelField, overlaps int, err error) {
	if resampled == nil {
		return nil, 0, fmt.Errorf("%w: nil resampled segmentation", ErrInvalidField)
	}
	masks := make(map[uint32][]*roi.RasterMask, len(resampled.Segments))
	for _, segment := range resampled.Segments {
		if segment.Number <= 0 {
			continue
		}
		masks[uint32(segment.Number)] = segment.Masks
	}
	return LabelFieldFromMasks(resampled.Grid, masks)
}

// labelGridAffine is the index-to-patient affine of a regular roi.Grid;
// irregular slice offsets cannot be expressed as an affine.
func labelGridAffine(grid roi.Grid) (render.GeometryAffine, error) {
	if err := grid.Validate(); err != nil {
		return render.GeometryAffine{}, fmt.Errorf("%w: %v", ErrInvalidField, err)
	}
	origin, spacing := grid.Origin, grid.SliceSpacing
	if len(grid.SliceOffsets) > 0 {
		origin = origin.Add(grid.SliceDir.Scale(grid.SliceOffsets[0]))
		if len(grid.SliceOffsets) > 1 {
			spacing = (grid.SliceOffsets[len(grid.SliceOffsets)-1] - grid.SliceOffsets[0]) / float64(len(grid.SliceOffsets)-1)
			for z, offset := range grid.SliceOffsets {
				if expected := grid.SliceOffsets[0] + float64(z)*spacing; math.Abs(offset-expected) > 1e-3*math.Max(spacing, 1e-9) {
					return render.GeometryAffine{}, fmt.Errorf("%w: irregular slice offsets; resample onto a regular grid first", ErrInvalidField)
				}
			}
		}
	}
	if !(spacing > 0) {
		spacing = math.Min(grid.ColSpacing, grid.RowSpacing)
	}
	return GridAffine(origin, [3]render.Vec3{grid.RowDir, grid.ColDir, grid.SliceDir}, [3]float64{grid.ColSpacing, grid.RowSpacing, spacing}), nil
}
