package nifti

import (
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/roi"
	"github.com/ThalesMMS/dicom-go/seg"
)

var (
	// ErrSEGGeometryMismatch reports a labelmap grid that is not an exact
	// axis permutation/flip/offset of the referenced DICOM series grid, or a
	// reference series that is not a regular grid.
	ErrSEGGeometryMismatch = errors.New("dicom/nifti: labelmap grid does not match the referenced series")
	// ErrSEGInvalidLabel reports a voxel that is not a finite integer label in
	// 0..65535, or a label without a segment definition.
	ErrSEGInvalidLabel = errors.New("dicom/nifti: invalid labelmap value")
	// ErrSEGOutsideReference reports a non-zero label outside the series.
	ErrSEGOutsideReference = errors.New("dicom/nifti: label outside the referenced series")
	// ErrSEGEmpty reports a labelmap without any non-zero voxel.
	ErrSEGEmpty = errors.New("dicom/nifti: labelmap has no labels")
)

// ReadCodeSEG classifies failures of the NIfTI-to-SEG conversion.
const ReadCodeSEG ReadErrorCode = "seg"

// DefaultSEGVoxelTolerance is the largest distance, in voxels, between a
// labelmap voxel center and the DICOM voxel center it is assigned to.
const DefaultSEGVoxelTolerance = 0.01

// SEGReferenceImage is one image plane of the source DICOM series.
type SEGReferenceImage struct {
	Geometry render.SliceGeometry
	Image    seg.ReferencedImage
}

// SEGOptions configures Volume.SEGDocument.
type SEGOptions struct {
	SOPInstanceUID      string
	StudyInstanceUID    string
	SeriesInstanceUID   string
	FrameOfReferenceUID string
	// References lists every plane of the source series, in any order.
	References []SEGReferenceImage
	// Segments maps a label value to its segment definition. Every non-zero
	// label must be listed; a nil map creates one generic segment per label.
	// Explicit numbers are reserved first. A zero Segment.Number uses the
	// first unused number at or after its position (starting at 1) in the
	// ascending label order.
	Segments map[int]seg.Segment
	// TimeIndex selects the time point of a 4D labelmap.
	TimeIndex int
	// VoxelTolerance bounds the grid match in voxels; zero selects
	// DefaultSEGVoxelTolerance.
	VoxelTolerance float64
}

// SEGReport describes how labelmap voxels were placed on the DICOM grid.
type SEGReport struct {
	// Axis[n] is the DICOM axis (0 column, 1 row, 2 slice) carrying NIfTI
	// axis n (i, j, k); Flipped[n] reports that it runs backwards.
	Axis    [3]int
	Flipped [3]bool
	// Offset is the DICOM index of NIfTI voxel (0, 0, 0).
	Offset [3]int
	// SegmentNumbers maps each label present to its segment number.
	SegmentNumbers map[int]int
	Frames         int
}

// SEGDocument converts an integer labelmap into a binary DICOM Segmentation
// document referencing the source series. The labelmap must lie exactly on
// the source grid up to an axis permutation, flips and an integer offset (for
// example a crop or a RAS-reoriented copy); no interpolation is performed.
// Pass the result to seg.Write.
func (v *Volume) SEGDocument(options SEGOptions) (*seg.Document, SEGReport, error) {
	report := SEGReport{}
	if v == nil || v.bytesPerVoxel == 0 {
		return nil, report, segError("volume", ErrInvalidSource)
	}
	if v.Geometry.Source == XformSourcePixDim {
		return nil, report, segError("xform", ErrSEGGeometryMismatch)
	}
	if options.TimeIndex < 0 || options.TimeIndex >= v.Dimensions[3] {
		return nil, report, segError("time", ErrUnsupportedDimensions)
	}
	tolerance := options.VoxelTolerance
	if tolerance == 0 {
		tolerance = DefaultSEGVoxelTolerance
	}
	if !finitePositive(tolerance) || tolerance >= 0.5 {
		return nil, report, segError("tolerance", ErrSEGGeometryMismatch)
	}
	grid, err := newReferenceGrid(options.References, v.Geometry.Spacing[2], tolerance)
	if err != nil {
		return nil, report, err
	}
	mapping, err := grid.mapping(v, tolerance)
	if err != nil {
		return nil, report, err
	}
	report.Axis, report.Flipped, report.Offset = mapping.axis, mapping.flipped, mapping.offset

	labels, err := v.validateLabels(options, grid, mapping)
	if err != nil {
		return nil, report, err
	}
	segments, numbers, err := segmentDefinitions(labels, options.Segments)
	if err != nil {
		return nil, report, err
	}
	report.SegmentNumbers = numbers

	masksBySegment := make(map[int][]seg.ROIMask, len(segments))
	columns, rows := grid.columns, grid.rows
	values := make([]uint16, columns*rows)
	buffer := make([]byte, columns*rows)
	present := make([]bool, 1<<16)
	for slice, reference := range grid.references {
		clear(values)
		var sliceLabels []int
		for row := 0; row < rows; row++ {
			for column := 0; column < columns; column++ {
				source, ok := mapping.toNIfTI([3]int{column, row, slice}, v.Dimensions)
				if !ok {
					continue
				}
				label := uint16(v.Value(v.Index(source[0], source[1], source[2], options.TimeIndex)))
				if label == 0 {
					continue
				}
				values[row*columns+column] = label
				if !present[label] {
					present[label] = true
					sliceLabels = append(sliceLabels, int(label))
				}
			}
		}
		sort.Ints(sliceLabels)
		for _, label := range sliceLabels {
			present[label] = false
			for index, value := range values {
				buffer[index] = 0
				if int(value) == label {
					buffer[index] = 1
				}
			}
			mask, err := roi.NewRasterMaskFromBytes(columns, rows, buffer)
			if err != nil {
				return nil, report, segError("mask", err)
			}
			segment := segments[label]
			masksBySegment[segment.Number] = append(masksBySegment[segment.Number], seg.ROIMask{
				Segment:     segment,
				SliceIndex:  slice,
				SourceImage: reference.Image,
				Geometry:    reference.Geometry,
				Mask:        mask,
			})
		}
	}
	segmentNumbers := make([]int, 0, len(masksBySegment))
	for number := range masksBySegment {
		segmentNumbers = append(segmentNumbers, number)
	}
	sort.Ints(segmentNumbers)
	var masks []seg.ROIMask
	for _, number := range segmentNumbers {
		masks = append(masks, masksBySegment[number]...)
	}
	document, err := seg.FromROIMasks(seg.FromROIMasksOptions{
		SOPInstanceUID:      options.SOPInstanceUID,
		StudyInstanceUID:    options.StudyInstanceUID,
		SeriesInstanceUID:   options.SeriesInstanceUID,
		FrameOfReferenceUID: options.FrameOfReferenceUID,
		Rows:                rows,
		Columns:             columns,
		Geometry:            grid.volume,
		Masks:               masks,
	})
	if err != nil {
		return nil, report, segError("document", err)
	}
	report.Frames = len(document.Frames)
	return document, report, nil
}

func segError(field string, err error) error {
	return &ReadError{Code: ReadCodeSEG, Field: field, Err: err}
}

type referenceGrid struct {
	references    []SEGReferenceImage // sorted along the slice normal
	volume        render.VolumeGeometry
	columns, rows int
	indexToLPS    [3]render.Vec3 // column, row and slice steps
	origin        render.Vec3
}

// newReferenceGrid sorts the source planes along their normal and checks
// that they form one regular grid: identical in-plane geometry and a uniform
// slice step (a tilted, sheared step is accepted and stays exact).
func newReferenceGrid(references []SEGReferenceImage, singleSliceSpacing, tolerance float64) (referenceGrid, error) {
	if len(references) == 0 {
		return referenceGrid{}, segError("references", ErrSEGGeometryMismatch)
	}
	first := references[0].Geometry
	sorted := append([]SEGReferenceImage(nil), references...)
	sort.SliceStable(sorted, func(a, b int) bool {
		return sorted[a].Geometry.PositionAlong(first.Normal) < sorted[b].Geometry.PositionAlong(first.Normal)
	})
	grid := referenceGrid{references: sorted, columns: first.Columns, rows: first.Rows, origin: sorted[0].Geometry.Origin}
	if grid.columns <= 0 || grid.rows <= 0 || grid.columns > 1<<16 || grid.rows > 1<<16 {
		return referenceGrid{}, segError("references", ErrSEGGeometryMismatch)
	}
	if !finiteVec(first.RowDir) || !finiteVec(first.ColDir) || !finiteVec(first.Normal) ||
		!finitePositive(first.ColSpacing) || !finitePositive(first.RowSpacing) {
		return referenceGrid{}, segError("references", ErrSEGGeometryMismatch)
	}
	grid.indexToLPS[0] = first.RowDir.Normalize().Scale(first.ColSpacing)
	grid.indexToLPS[1] = first.ColDir.Normalize().Scale(first.RowSpacing)
	if len(sorted) == 1 {
		grid.indexToLPS[2] = first.Normal.Normalize().Scale(singleSliceSpacing)
	} else {
		span := sorted[len(sorted)-1].Geometry.Origin.Sub(sorted[0].Geometry.Origin)
		grid.indexToLPS[2] = span.Scale(1 / float64(len(sorted)-1))
	}
	smallest := math.Min(first.ColSpacing, math.Min(first.RowSpacing, grid.indexToLPS[2].Length()))
	if !finitePositive(smallest) {
		return referenceGrid{}, segError("references", ErrSEGGeometryMismatch)
	}
	slices := make([]render.SliceGeometry, len(sorted))
	for index, reference := range sorted {
		g := reference.Geometry
		slices[index] = g
		expected := grid.origin.Add(grid.indexToLPS[2].Scale(float64(index)))
		if g.Columns != grid.columns || g.Rows != grid.rows ||
			g.RowDir.Normalize().Dot(first.RowDir.Normalize()) < 1-1e-9 ||
			g.ColDir.Normalize().Dot(first.ColDir.Normalize()) < 1-1e-9 ||
			!nearlyEqual(g.ColSpacing, first.ColSpacing, 1e-6) || !nearlyEqual(g.RowSpacing, first.RowSpacing, 1e-6) ||
			!finiteVec(g.Origin) || g.Origin.Sub(expected).Length() > tolerance*smallest {
			return referenceGrid{}, segError("references", ErrSEGGeometryMismatch)
		}
	}
	grid.volume = render.BuildVolumeGeometry(slices, render.GeometryTolerances{})
	return grid, nil
}

type gridMapping struct {
	axis    [3]int
	flipped [3]bool
	offset  [3]int
}

// mapping expresses the labelmap index-to-LPS affine in DICOM grid indexes and
// accepts it only when it is a signed axis permutation plus an integer offset
// at every corner of the labelmap within tolerance voxels.
func (g referenceGrid) mapping(v *Volume, tolerance float64) (gridMapping, error) {
	var basis [3][3]float64
	for column := 0; column < 3; column++ {
		basis[0][column], basis[1][column], basis[2][column] = g.indexToLPS[column].X, g.indexToLPS[column].Y, g.indexToLPS[column].Z
	}
	inverse, ok := invert3(basis)
	if !ok {
		return gridMapping{}, segError("references", ErrSEGGeometryMismatch)
	}
	a := v.Geometry.IndexToLPS
	toGrid := func(p render.Vec3) [3]float64 {
		world := a.TransformPoint(p).Sub(g.origin)
		var out [3]float64
		for r := 0; r < 3; r++ {
			out[r] = inverse[r][0]*world.X + inverse[r][1]*world.Y + inverse[r][2]*world.Z
		}
		return out
	}
	origin := toGrid(render.Vec3{})
	var out gridMapping
	used := [3]bool{}
	for axis := 0; axis < 3; axis++ {
		var step render.Vec3
		switch axis {
		case 0:
			step.X = 1
		case 1:
			step.Y = 1
		default:
			step.Z = 1
		}
		moved := toGrid(step)
		best, bestValue := -1, 0.0
		for r := 0; r < 3; r++ {
			delta := moved[r] - origin[r]
			if math.Abs(delta) > math.Abs(bestValue) {
				best, bestValue = r, delta
			}
		}
		if best < 0 || used[best] || math.Abs(math.Abs(bestValue)-1) > tolerance {
			return gridMapping{}, segError("xform", ErrSEGGeometryMismatch)
		}
		used[best] = true
		out.axis[axis] = best
		out.flipped[axis] = bestValue < 0
	}
	for r := 0; r < 3; r++ {
		if !(math.Abs(origin[r]) <= math.MaxInt32) {
			return gridMapping{}, segError("xform", ErrSEGGeometryMismatch)
		}
		rounded := float64(render.NearestIndex(origin[r]))
		if math.Abs(origin[r]-rounded) > tolerance {
			return gridMapping{}, segError("xform", ErrSEGGeometryMismatch)
		}
		out.offset[r] = int(rounded)
	}
	// Linear maps attain their largest error at a corner.
	extent := [3]float64{float64(v.Dimensions[0] - 1), float64(v.Dimensions[1] - 1), float64(v.Dimensions[2] - 1)}
	for corner := 0; corner < 8; corner++ {
		var p [3]float64
		for axis := 0; axis < 3; axis++ {
			if corner&(1<<axis) != 0 {
				p[axis] = extent[axis]
			}
		}
		actual := toGrid(render.Vec3{X: p[0], Y: p[1], Z: p[2]})
		predicted := out.toGridFloat(p)
		for r := 0; r < 3; r++ {
			if math.Abs(actual[r]-predicted[r]) > tolerance {
				return gridMapping{}, segError("xform", ErrSEGGeometryMismatch)
			}
		}
	}
	return out, nil
}

func (m gridMapping) toGridFloat(p [3]float64) [3]float64 {
	out := [3]float64{float64(m.offset[0]), float64(m.offset[1]), float64(m.offset[2])}
	for axis := 0; axis < 3; axis++ {
		if m.flipped[axis] {
			out[m.axis[axis]] -= p[axis]
		} else {
			out[m.axis[axis]] += p[axis]
		}
	}
	return out
}

func (m gridMapping) toGrid(source [3]int) [3]int {
	out := m.offset
	for axis := 0; axis < 3; axis++ {
		if m.flipped[axis] {
			out[m.axis[axis]] -= source[axis]
		} else {
			out[m.axis[axis]] += source[axis]
		}
	}
	return out
}

func (m gridMapping) toNIfTI(target [3]int, dimensions [4]int) ([3]int, bool) {
	var out [3]int
	for axis := 0; axis < 3; axis++ {
		delta := target[m.axis[axis]] - m.offset[m.axis[axis]]
		if m.flipped[axis] {
			delta = -delta
		}
		if delta < 0 || delta >= dimensions[axis] {
			return out, false
		}
		out[axis] = delta
	}
	return out, true
}

// validateLabels checks every voxel of the selected time point: each non-zero
// value must be an integer label with a definition that lands inside the
// reference grid. It returns the ascending set of labels present.
func (v *Volume) validateLabels(options SEGOptions, grid referenceGrid, mapping gridMapping) ([]int, error) {
	present := map[int]bool{}
	bounds := [3]int{grid.columns, grid.rows, len(grid.references)}
	d := v.Dimensions
	for k := 0; k < d[2]; k++ {
		for j := 0; j < d[1]; j++ {
			for i := 0; i < d[0]; i++ {
				value := v.Value(v.Index(i, j, k, options.TimeIndex))
				if value == 0 {
					continue
				}
				if !finite(value) || value != math.Trunc(value) || value < 0 || value > math.MaxUint16 {
					return nil, segError("label", ErrSEGInvalidLabel)
				}
				label := int(value)
				if options.Segments != nil {
					if _, ok := options.Segments[label]; !ok {
						return nil, segError("label", ErrSEGInvalidLabel)
					}
				}
				target := mapping.toGrid([3]int{i, j, k})
				for axis := 0; axis < 3; axis++ {
					if target[axis] < 0 || target[axis] >= bounds[axis] {
						return nil, segError("label", ErrSEGOutsideReference)
					}
				}
				present[label] = true
			}
		}
	}
	if len(present) == 0 {
		return nil, segError("label", ErrSEGEmpty)
	}
	labels := make([]int, 0, len(present))
	for label := range present {
		labels = append(labels, label)
	}
	sort.Ints(labels)
	return labels, nil
}

func segmentDefinitions(labels []int, definitions map[int]seg.Segment) (map[int]seg.Segment, map[int]int, error) {
	segments := make(map[int]seg.Segment, len(labels))
	numbers := make(map[int]int, len(labels))
	used := map[int]bool{}
	for _, label := range labels {
		number := definitions[label].Number
		if number == 0 {
			continue
		}
		if number < 1 || number > math.MaxUint16 || used[number] {
			return nil, nil, segError("segment", ErrSEGInvalidLabel)
		}
		used[number] = true
	}
	for index, label := range labels {
		segment, ok := definitions[label]
		if !ok {
			segment = seg.Segment{Label: fmt.Sprintf("Label %d", label)}
		}
		if segment.Number == 0 {
			segment.Number = index + 1
			for used[segment.Number] {
				segment.Number++
			}
		}
		if segment.Number < 1 || segment.Number > math.MaxUint16 {
			return nil, nil, segError("segment", ErrSEGInvalidLabel)
		}
		used[segment.Number] = true
		segments[label] = segment
		numbers[label] = segment.Number
	}
	return segments, numbers, nil
}
