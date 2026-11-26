package seg

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/roi"
)

// Resampling a segmentation onto another grid of its Frame of Reference
// (#1114), such as an AI segmentation made on a resampled volume displayed
// over the source series. The caller checks that both share one Frame of
// Reference.
//
//   - BINARY segments may overlap, so each is resampled on its own as a
//     two-label image (roi.MaskStack).
//   - LABELMAP frames are resampled together as one label image, so the
//     Gaussian mode's argmax arbitrates between neighbouring labels.
//   - FRACTIONAL samples are interpolated linearly and thresholded like
//     ToROIWithOptions.
//
// The frames' masks and samples are shared, not copied, so a Resampler costs
// little beyond the document; it must be rebuilt after the document changes.

// ErrNoGeometry reports a segmentation whose frames cannot be placed in
// patient space on one grid.
var ErrNoGeometry = errors.New("dicom/seg: segmentation has no usable plane geometry")

// ResampleOptions configure NewResampler.
type ResampleOptions struct {
	// Mode resamples BINARY and LABELMAP segments; the zero value is nearest
	// neighbour. FRACTIONAL segments are always interpolated linearly.
	Mode roi.LabelResampleMode
	// FractionalThreshold is the fraction (0, 1] of the Maximum Fractional
	// Value at or above which an interpolated FRACTIONAL sample is inside;
	// zero means 0.5, as ToROIWithOptions.
	FractionalThreshold float64
	// SliceThickness is the slab (mm) that a single-plane segmentation
	// covers; zero uses the document's slice spacing, else its smaller pixel
	// spacing.
	SliceThickness float64
}

// Resampler holds a segmentation placed on its own patient-space grid, ready
// to be resampled onto target grids. It is safe for concurrent use.
type Resampler struct {
	grid       roi.Grid
	layout     string
	mode       roi.LabelResampleMode
	cut        float64
	numbers    []int
	binary     map[int]*roi.MaskStack
	fractional map[int]*roi.FractionStack
	labels     *roi.LabelStack
	// background is the label-map background. roi treats label 0 as
	// background, so a nonzero background swaps places with 0 in labels.
	background uint16
}

// Resampled is a segmentation resampled onto a target grid.
type Resampled struct {
	Grid roi.Grid
	// Coverage marks the target voxels inside the segmentation's grid; the
	// others carry no segmentation information.
	Coverage roi.ResampleCoverage
	// Segments lists, by ascending number, every segment with source frames
	// (BINARY, FRACTIONAL) or every label other than the background found
	// under the target (LABELMAP).
	Segments []ResampledSegment
}

// ResampledSegment is one segment on the target grid.
type ResampledSegment struct {
	Number int
	// Masks holds the segment's pixels per target slice (nil when none).
	Masks  []*roi.RasterMask
	Voxels int
	// Preserved reports that the Gaussian mode kept this segment on its
	// strongest voxel after the argmax dropped it.
	Preserved bool
	// Vanished reports a segment with source voxels under the target but no
	// target voxel (nearest neighbour between target samples, or a
	// FRACTIONAL segment below the threshold is not reported).
	Vanished bool
}

// Resample resamples doc onto target; see NewResampler.
func Resample(ctx context.Context, doc *Document, target roi.Grid, opts ResampleOptions) (*Resampled, error) {
	r, err := NewResampler(ctx, doc, opts)
	if err != nil {
		return nil, err
	}
	return r.Resample(ctx, target)
}

// NewResampler places every frame of doc on one patient-space grid: the
// document's volume geometry when every frame lies on it, otherwise the
// stack of its distinct frame planes, with the missing planes of a regular
// stack (empty frames omitted by the writer) restored as empty slices.
// Frames must share the document's size, spacing and orientation.
func NewResampler(ctx context.Context, doc *Document, opts ResampleOptions) (*Resampler, error) {
	if doc == nil {
		return nil, fmt.Errorf("%w: document is nil", ErrInvalidObject)
	}
	if doc.Rows <= 0 || doc.Columns <= 0 {
		return nil, fmt.Errorf("%w: Rows=%d Columns=%d", ErrInvalidObject, doc.Rows, doc.Columns)
	}
	threshold := opts.FractionalThreshold
	if threshold == 0 {
		threshold = 0.5
	}
	if !(threshold > 0 && threshold <= 1) {
		return nil, fmt.Errorf("%w: fractional threshold %v is outside (0, 1]", ErrInvalidObject, threshold)
	}
	grid, frameSlices, err := resampleGrid(doc, opts.SliceThickness)
	if err != nil {
		return nil, err
	}
	r := &Resampler{grid: grid, mode: opts.Mode, layout: resampleLayout(doc)}
	pixels := doc.Rows * doc.Columns
	switch r.layout {
	case SegmentationTypeLabelMap:
		r.background = doc.LabelMapBackground
		slices := make([][]uint16, grid.Slices)
		for i, frame := range doc.Frames {
			if i%256 == 0 && ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if len(frame.LabelMap) == 0 {
				continue
			}
			if len(frame.LabelMap) != pixels {
				return nil, fmt.Errorf("%w: label-map frame %d has %d values", ErrInvalidObject, i, len(frame.LabelMap))
			}
			z := frameSlices[i]
			labels := frame.LabelMap
			if r.background != 0 {
				labels = make([]uint16, len(frame.LabelMap))
				for j, value := range frame.LabelMap {
					labels[j] = r.sourceLabel(value)
				}
			}
			if slices[z] == nil {
				slices[z] = labels
				continue
			}
			merged := append([]uint16(nil), slices[z]...)
			for j, value := range labels {
				if value != 0 {
					merged[j] = value
				}
			}
			slices[z] = merged
		}
		if r.labels, err = roi.NewLabelStack(grid, slices); err != nil {
			return nil, err
		}
	case SegmentationTypeFractional:
		if doc.MaxFractionalValue < 1 || doc.MaxFractionalValue > 255 {
			return nil, fmt.Errorf("%w: Maximum Fractional Value %d is outside 1-255", ErrInvalidObject, doc.MaxFractionalValue)
		}
		r.cut = float64(max(1, int(math.Ceil(threshold*float64(doc.MaxFractionalValue)-1e-9))))
		stacks := map[int][][]uint8{}
		for i, frame := range doc.Frames {
			if i%256 == 0 && ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if frame.SegmentNumber <= 0 {
				continue
			}
			slices := stacks[frame.SegmentNumber]
			if slices == nil {
				slices = make([][]uint8, grid.Slices)
				stacks[frame.SegmentNumber] = slices
			}
			if len(frame.Fractional) == 0 {
				continue
			}
			if len(frame.Fractional) != pixels {
				return nil, fmt.Errorf("%w: fractional frame %d has %d samples", ErrInvalidObject, i, len(frame.Fractional))
			}
			z := frameSlices[i]
			if slices[z] == nil {
				slices[z] = frame.Fractional
				continue
			}
			merged := append([]uint8(nil), slices[z]...)
			for j, value := range frame.Fractional {
				merged[j] = max(merged[j], value)
			}
			slices[z] = merged
		}
		r.fractional = map[int]*roi.FractionStack{}
		for number, slices := range stacks {
			if r.fractional[number], err = roi.NewFractionStack(grid, slices); err != nil {
				return nil, err
			}
			r.numbers = append(r.numbers, number)
		}
	default:
		stacks := map[int][]*roi.RasterMask{}
		shared := map[*roi.RasterMask]bool{}
		for i, frame := range doc.Frames {
			if i%256 == 0 && ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if frame.SegmentNumber <= 0 {
				continue
			}
			masks := stacks[frame.SegmentNumber]
			if masks == nil {
				masks = make([]*roi.RasterMask, grid.Slices)
				stacks[frame.SegmentNumber] = masks
			}
			if frame.Mask == nil || frame.Mask.Empty() {
				continue
			}
			if frame.Mask.Columns != doc.Columns || frame.Mask.Rows != doc.Rows {
				return nil, fmt.Errorf("%w: frame %d mask is %dx%d", ErrInvalidObject, i, frame.Mask.Columns, frame.Mask.Rows)
			}
			z := frameSlices[i]
			switch {
			case masks[z] == nil:
				masks[z] = frame.Mask
				shared[frame.Mask] = true
			case shared[masks[z]]:
				merged := masks[z].Clone()
				merged.Union(frame.Mask)
				masks[z] = merged
			default:
				masks[z].Union(frame.Mask)
			}
		}
		r.binary = map[int]*roi.MaskStack{}
		for number, masks := range stacks {
			if number > math.MaxUint16 {
				return nil, fmt.Errorf("%w: segment number %d", ErrInvalidObject, number)
			}
			if r.binary[number], err = roi.NewMaskStack(grid, masks, 1); err != nil {
				return nil, err
			}
			r.numbers = append(r.numbers, number)
		}
	}
	sort.Ints(r.numbers)
	return r, nil
}

// Grid returns the segmentation's own patient-space grid.
func (r *Resampler) Grid() roi.Grid { return r.grid }

// Layout returns BINARY, LABELMAP or FRACTIONAL.
func (r *Resampler) Layout() string { return r.layout }

// Resample resamples every segment onto target.
func (r *Resampler) Resample(ctx context.Context, target roi.Grid) (*Resampled, error) {
	return r.resample(ctx, target, nil)
}

// ResampleMapped resamples every segment onto a target grid in another Frame
// of Reference: mapper takes each target voxel centre into the
// segmentation's patient space (a registration), and voxels it cannot map
// stay uncovered. Segments are never reported Preserved or Vanished.
func (r *Resampler) ResampleMapped(ctx context.Context, target roi.Grid, mapper roi.PointMapper) (*Resampled, error) {
	if mapper == nil {
		return nil, fmt.Errorf("%w: nil mapper", ErrInvalidObject)
	}
	return r.resample(ctx, target, mapper)
}

func (r *Resampler) resample(ctx context.Context, target roi.Grid, mapper roi.PointMapper) (*Resampled, error) {
	out := &Resampled{Grid: target}
	mode := r.mode
	mode.Mapper = mapper
	coverageSet := false
	switch r.layout {
	case SegmentationTypeLabelMap:
		result, err := roi.ResampleLabelMasks(ctx, r.labels, target, mode)
		if err != nil {
			return nil, err
		}
		out.Coverage, coverageSet = result.Coverage, true
		numbers := map[uint16]bool{}
		for label := range result.Counts {
			numbers[label] = true
		}
		for _, label := range result.SourceLabels {
			numbers[label] = true
		}
		preserved := labelSet(result.Preserved)
		vanished := labelSet(result.Vanished)
		for label := range numbers {
			out.Segments = append(out.Segments, ResampledSegment{
				Number: int(r.sourceLabel(label)), Masks: result.Masks[label], Voxels: result.Counts[label],
				Preserved: preserved[label], Vanished: vanished[label],
			})
		}
	case SegmentationTypeFractional:
		for _, number := range r.numbers {
			result, err := roi.ResampleFractionMask(ctx, r.fractional[number], target, r.cut, roi.FractionResampleOptions{MaxVoxels: r.mode.MaxVoxels, Mapper: mapper})
			if err != nil {
				return nil, err
			}
			out.Coverage, coverageSet = result.Coverage, true
			out.Segments = append(out.Segments, ResampledSegment{Number: number, Masks: result.Masks, Voxels: result.Count})
		}
	default:
		for _, number := range r.numbers {
			result, err := roi.ResampleLabelMasks(ctx, r.binary[number], target, mode)
			if err != nil {
				return nil, err
			}
			out.Coverage, coverageSet = result.Coverage, true
			out.Segments = append(out.Segments, ResampledSegment{
				Number: number, Masks: result.Masks[1], Voxels: result.Counts[1],
				Preserved: len(result.Preserved) > 0, Vanished: len(result.Vanished) > 0,
			})
		}
	}
	if !coverageSet && mapper != nil {
		empty, err := roi.NewLabelStack(r.grid, make([][]uint16, r.grid.Slices))
		if err != nil {
			return nil, err
		}
		result, err := roi.ResampleLabelMasks(ctx, empty, target, mode)
		if err != nil {
			return nil, err
		}
		out.Coverage, coverageSet = result.Coverage, true
	}
	if !coverageSet {
		coverage, err := roi.GridCoverage(ctx, r.grid, target)
		if err != nil {
			return nil, err
		}
		out.Coverage = coverage
	}
	sort.Slice(out.Segments, func(i, j int) bool { return out.Segments[i].Number < out.Segments[j].Number })
	return out, nil
}

// sourceLabel swaps the label-map background and 0, which roi resamples as
// background; the swap is its own inverse.
func (r *Resampler) sourceLabel(label uint16) uint16 {
	switch {
	case r.background == 0:
		return label
	case label == r.background:
		return 0
	case label == 0:
		return r.background
	}
	return label
}

func labelSet(labels []uint16) map[uint16]bool {
	out := make(map[uint16]bool, len(labels))
	for _, label := range labels {
		out[label] = true
	}
	return out
}

func resampleLayout(doc *Document) string {
	switch {
	case doc.SOPClassUID == LabelMapSegmentationStorage || doc.SegmentationType == SegmentationTypeLabelMap:
		return SegmentationTypeLabelMap
	case doc.SegmentationType == SegmentationTypeFractional:
		return SegmentationTypeFractional
	}
	for _, frame := range doc.Frames {
		if len(frame.LabelMap) > 0 {
			return SegmentationTypeLabelMap
		}
		if len(frame.Fractional) > 0 {
			return SegmentationTypeFractional
		}
	}
	return SegmentationTypeBinary
}

// resampleGrid returns the grid of doc and the grid slice of every frame.
func resampleGrid(doc *Document, thickness float64) (roi.Grid, []int, error) {
	if len(doc.Frames) == 0 {
		return roi.Grid{}, nil, fmt.Errorf("%w: no frames", ErrNoGeometry)
	}
	planes := make([]render.SliceGeometry, len(doc.Frames))
	for i, frame := range doc.Frames {
		g, ok := geometryForFrame(doc.Geometry, frame)
		if !ok || !validSliceGeometry(g) {
			return roi.Grid{}, nil, fmt.Errorf("%w: frame %d has no plane geometry", ErrNoGeometry, i)
		}
		if g.Rows != doc.Rows || g.Columns != doc.Columns {
			return roi.Grid{}, nil, fmt.Errorf("%w: frame %d plane is %dx%d, document %dx%d", ErrNoGeometry, i, g.Columns, g.Rows, doc.Columns, doc.Rows)
		}
		planes[i] = g
	}
	if thickness <= 0 {
		thickness = doc.Geometry.MeanSpacing
	}
	if thickness <= 0 {
		thickness = math.Min(planes[0].RowSpacing, planes[0].ColSpacing)
	}
	// The document's own stack first, when every frame lies on it.
	if len(doc.Geometry.Slices) > 0 {
		if grid, err := stackGrid(doc.Geometry.Slices, thickness); err == nil {
			if slices, ok := placeFrames(grid, planes); ok {
				return grid, slices, nil
			}
		}
	}
	stack := distinctPlanes(planes)
	grid, err := stackGrid(stack, thickness)
	if err != nil {
		return roi.Grid{}, nil, fmt.Errorf("%w: %v", ErrNoGeometry, err)
	}
	slices, ok := placeFrames(grid, planes)
	if !ok {
		return roi.Grid{}, nil, fmt.Errorf("%w: frames do not share one grid", ErrNoGeometry)
	}
	return grid, slices, nil
}

func stackGrid(slices []render.SliceGeometry, thickness float64) (roi.Grid, error) {
	if len(slices) == 1 {
		grid := roi.GridFromSlice(slices[0], thickness)
		return grid, grid.Validate()
	}
	return roi.GridFromSlices(slices)
}

// placeFrames maps every frame plane onto a grid slice congruent with it
// (render.Grid.Congruent at the DICOM grid tolerances, #1115): the same
// orientation, and every pixel within a twentieth of a pixel of the slice's.
func placeFrames(grid roi.Grid, planes []render.SliceGeometry) ([]int, bool) {
	out := make([]int, len(planes))
	for i, plane := range planes {
		_, _, z, _ := grid.ContinuousIndex(plane.Origin)
		k := render.NearestIndex(z)
		if k < 0 || k >= grid.Slices {
			return nil, false
		}
		frame, okFrame := plane.Grid(1)
		slice, okSlice := grid.SliceGeometry(k).Grid(1)
		if !okFrame || !okSlice ||
			!slice.SameGeometry(frame, render.DICOMGridCoordinateTolerance, render.DICOMGridDirectionTolerance) {
			return nil, false
		}
		out[i] = k
	}
	return out, true
}

// distinctPlanes sorts the frame planes along the normal, drops duplicates
// and, when the gaps are whole multiples of the smallest, fills the missing
// planes so that omitted empty frames read as empty rather than being
// bridged by their neighbours.
func distinctPlanes(planes []render.SliceGeometry) []render.SliceGeometry {
	normal := planes[0].RowDir.Normalize().Cross(planes[0].ColDir.Normalize()).Normalize()
	sorted := append([]render.SliceGeometry(nil), planes...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Origin.Dot(normal) < sorted[j].Origin.Dot(normal)
	})
	out := sorted[:1]
	for _, plane := range sorted[1:] {
		if plane.Origin.Sub(out[len(out)-1].Origin).Length() > 1e-3 {
			out = append(out, plane)
		}
	}
	if len(out) < 3 {
		return out
	}
	step := math.Inf(1)
	for i := 1; i < len(out); i++ {
		step = math.Min(step, out[i].Origin.Sub(out[i-1].Origin).Length())
	}
	total := out[len(out)-1].Origin.Sub(out[0].Origin)
	count := int(math.Round(total.Length()/step)) + 1
	if count == len(out) || count > 4*len(out)+16 {
		return out
	}
	for i := 1; i < len(out); i++ {
		gap := out[i].Origin.Sub(out[i-1].Origin).Length() / step
		if math.Abs(gap-math.Round(gap)) > 0.01 {
			return out
		}
	}
	filled := make([]render.SliceGeometry, count)
	for k := range filled {
		filled[k] = out[0]
		filled[k].Origin = out[0].Origin.Add(total.Scale(float64(k) / float64(count-1)))
	}
	return filled
}
