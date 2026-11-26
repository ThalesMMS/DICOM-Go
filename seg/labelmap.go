package seg

import (
	"fmt"
	"sort"

	"github.com/ThalesMMS/dicom-go/roi"
)

// LabelMapOptions configure ToLabelMap.
type LabelMapOptions struct {
	// SOPInstanceUID and SeriesInstanceUID identify the new instance; the
	// SOP Instance UID is required.
	SOPInstanceUID    string
	SeriesInstanceUID string
	// BitsAllocated is 8, 16 or zero (8 when every segment number fits).
	BitsAllocated int
	// BackgroundLabel is the pixel value of unsegmented pixels, 0 by default.
	// It becomes the document's LabelMapBackground, which Write records as
	// Pixel Padding Value (0028,0120) (PS3.3 A.51.4). No source segment may
	// use it as its number.
	BackgroundLabel uint16
	// Background describes the background value, which every label map with
	// an unsegmented pixel uses (PS3.3 C.8.20.2.3.3). Nil gives a MANUAL
	// "Background" segment with the generic ROI codes; its Number is forced
	// to BackgroundLabel.
	Background *Segment
}

// ToLabelMap converts a BINARY segmentation whose segments do not overlap into
// a LABELMAP one (Label Map Segmentation Storage): one frame per frame
// position, whose pixels hold the number of their segment or the background
// label (opts.BackgroundLabel, 0 by default). Segment
// numbers are kept. When segments overlap it returns the overlap report and
// ErrOverlappingSegments; OverlapReport.DisjointGroups tells how to split
// them into several label maps.
func ToLabelMap(doc *Document, opts LabelMapOptions) (*Document, OverlapReport, error) {
	if doc == nil {
		return nil, OverlapReport{}, fmt.Errorf("%w: document is nil", ErrInvalidObject)
	}
	if doc.SOPClassUID == LabelMapSegmentationStorage || segmentationType(doc) != SegmentationTypeBinary {
		return nil, OverlapReport{}, fmt.Errorf("%w: %s segmentation is not BINARY", ErrUnsupportedSegmentationType, segmentationType(doc))
	}
	if opts.SOPInstanceUID == "" {
		return nil, OverlapReport{}, fmt.Errorf("%w: the label map needs its own SOP Instance UID", ErrInvalidObject)
	}
	report, err := Overlaps(doc)
	if err != nil {
		return nil, OverlapReport{}, err
	}
	if len(report.Overlaps) > 0 {
		return nil, report, fmt.Errorf("%w: %d segment pairs share pixels", ErrOverlappingSegments, len(report.Overlaps))
	}
	pixels := doc.Rows * doc.Columns
	if pixels <= 0 {
		return nil, report, fmt.Errorf("%w: Rows=%d Columns=%d", ErrInvalidObject, doc.Rows, doc.Columns)
	}

	backgroundLabel := int(opts.BackgroundLabel)
	background := Segment{Label: "Background", AlgorithmType: AlgorithmManual}
	if opts.Background != nil {
		background = *opts.Background
	}
	background.Number = backgroundLabel
	out := &Document{
		SOPClassUID:           LabelMapSegmentationStorage,
		SOPInstanceUID:        opts.SOPInstanceUID,
		StudyInstanceUID:      doc.StudyInstanceUID,
		SeriesInstanceUID:     firstNonEmpty(opts.SeriesInstanceUID, doc.SeriesInstanceUID),
		FrameOfReferenceUID:   doc.FrameOfReferenceUID,
		Rows:                  doc.Rows,
		Columns:               doc.Columns,
		SegmentationType:      SegmentationTypeLabelMap,
		LabelMapBitsAllocated: opts.BitsAllocated,
		LabelMapBackground:    opts.BackgroundLabel,
		Segments:              []Segment{normalizeSegment(background, 0)},
		ReferencedImages:      append([]ReferencedImage(nil), doc.ReferencedImages...),
		Geometry:              doc.Geometry,
	}
	out.Segments[0].Number = backgroundLabel
	for i, segment := range doc.Segments {
		segment = normalizeSegment(segment, i+1)
		if segment.Number == backgroundLabel {
			return nil, report, fmt.Errorf("%w: segment number %d is the label-map background", ErrInvalidObject, backgroundLabel)
		}
		out.Segments = append(out.Segments, segment)
	}

	// One label-map frame per position, in slice order.
	index := map[positionKey]int{}
	for _, frame := range doc.Frames {
		key := framePosition(frame)
		frameIndex, ok := index[key]
		if !ok {
			frameIndex = len(out.Frames)
			index[key] = frameIndex
			labels := make([]uint16, pixels)
			if opts.BackgroundLabel != 0 {
				for i := range labels {
					labels[i] = opts.BackgroundLabel
				}
			}
			out.Frames = append(out.Frames, Frame{
				SliceIndex:               frame.SliceIndex,
				ReferencedSOPInstanceUID: frame.ReferencedSOPInstanceUID,
				ReferencedFrameNumber:    frame.ReferencedFrameNumber,
				Geometry:                 frame.Geometry,
				LabelMap:                 labels,
			})
		}
		if frame.Mask == nil {
			continue
		}
		if frame.SegmentNumber <= 0 || frame.SegmentNumber > 1<<16-1 {
			return nil, report, fmt.Errorf("%w: frame segment number %d is outside US", ErrInvalidObject, frame.SegmentNumber)
		}
		labels := out.Frames[frameIndex].LabelMap
		frame.Mask.ForEachRun(func(y int, run roi.MaskRun) {
			for x := run.Start; x < run.End; x++ {
				labels[y*doc.Columns+x] = uint16(frame.SegmentNumber)
			}
		})
	}
	sort.SliceStable(out.Frames, func(i, j int) bool { return out.Frames[i].SliceIndex < out.Frames[j].SliceIndex })
	return out, report, nil
}

// BinaryOptions configure ToBinary.
type BinaryOptions struct {
	// SOPInstanceUID and SeriesInstanceUID identify the new instance; the
	// SOP Instance UID is required.
	SOPInstanceUID    string
	SeriesInstanceUID string
	// IncludeBackground converts the label-map background (the document's
	// LabelMapBackground, Pixel Padding Value, 0 by default) into a segment
	// too; by default it is left out. Every other value, 0 included when the
	// background is another value, is a segment.
	IncludeBackground bool
	// IncludeZero is the former name of IncludeBackground, from when the
	// background was always 0; either one includes the background.
	//
	// Deprecated: use IncludeBackground.
	IncludeZero bool
}

// ToBinary converts a LABELMAP segmentation into a BINARY one: one frame for
// every segment present at a frame position. BINARY segment numbers must run
// from 1 without gaps (PS3.3 C.8.20.2.4), so the described label-map segments
// are renumbered in ascending order; the returned map gives the new number of
// each label-map value.
func ToBinary(doc *Document, opts BinaryOptions) (*Document, map[int]int, error) {
	if doc == nil {
		return nil, nil, fmt.Errorf("%w: document is nil", ErrInvalidObject)
	}
	if doc.SOPClassUID != LabelMapSegmentationStorage {
		return nil, nil, fmt.Errorf("%w: %s segmentation is not a label map", ErrUnsupportedSegmentationType, segmentationType(doc))
	}
	if opts.SOPInstanceUID == "" {
		return nil, nil, fmt.Errorf("%w: the binary segmentation needs its own SOP Instance UID", ErrInvalidObject)
	}
	if doc.Columns <= 0 || doc.Rows <= 0 {
		return nil, nil, fmt.Errorf("%w: Rows=%d Columns=%d", ErrInvalidObject, doc.Rows, doc.Columns)
	}
	background := labelMapBackground(doc)
	includeBackground := opts.IncludeBackground || opts.IncludeZero
	segments := append([]Segment(nil), doc.Segments...)
	sort.SliceStable(segments, func(i, j int) bool { return segments[i].Number < segments[j].Number })
	numbers := map[int]int{}
	out := &Document{
		SOPClassUID:         SegmentationStorage,
		SOPInstanceUID:      opts.SOPInstanceUID,
		StudyInstanceUID:    doc.StudyInstanceUID,
		SeriesInstanceUID:   firstNonEmpty(opts.SeriesInstanceUID, doc.SeriesInstanceUID),
		FrameOfReferenceUID: doc.FrameOfReferenceUID,
		Rows:                doc.Rows,
		Columns:             doc.Columns,
		SegmentationType:    SegmentationTypeBinary,
		ReferencedImages:    append([]ReferencedImage(nil), doc.ReferencedImages...),
		Geometry:            doc.Geometry,
	}
	for _, segment := range segments {
		if segment.Number == background && !includeBackground {
			continue
		}
		if _, dup := numbers[segment.Number]; dup {
			return nil, nil, fmt.Errorf("%w: segment number %d is not unique", ErrInvalidObject, segment.Number)
		}
		numbers[segment.Number] = len(out.Segments) + 1
		segment.Number = len(out.Segments) + 1
		out.Segments = append(out.Segments, segment)
	}

	type positioned struct {
		segment int
		order   int
		frame   Frame
	}
	var frames []positioned
	for order, frame := range doc.Frames {
		masks := map[int]*roi.RasterMask{}
		for index, value := range frame.LabelMap {
			number, ok := numbers[int(value)]
			if !ok {
				if int(value) != background || includeBackground {
					return nil, nil, fmt.Errorf("%w: label-map value %d has no segment", ErrInvalidObject, value)
				}
				continue
			}
			mask := masks[number]
			if mask == nil {
				mask = roi.NewRasterMask(doc.Columns, doc.Rows)
				masks[number] = mask
			}
			mask.Set(index%doc.Columns, index/doc.Columns, true)
		}
		for number, mask := range masks {
			frames = append(frames, positioned{number, order, Frame{
				SegmentNumber:            number,
				SliceIndex:               frame.SliceIndex,
				ReferencedSOPInstanceUID: frame.ReferencedSOPInstanceUID,
				ReferencedFrameNumber:    frame.ReferencedFrameNumber,
				Geometry:                 frame.Geometry,
				Mask:                     mask,
			}})
		}
	}
	sort.Slice(frames, func(i, j int) bool {
		if frames[i].segment != frames[j].segment {
			return frames[i].segment < frames[j].segment
		}
		return frames[i].order < frames[j].order
	})
	for _, frame := range frames {
		out.Frames = append(out.Frames, frame.frame)
	}
	return out, numbers, nil
}

// labelMapBackground is the pixel value that doc's label-map frames use for
// background: its Pixel Padding Value, 0 by default.
func labelMapBackground(doc *Document) int {
	return int(doc.LabelMapBackground)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
