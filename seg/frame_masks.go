package seg

import (
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/roi"
)

// FrameMask is one segment's nonempty binary mask on a stored SEG frame's grid.
type FrameMask struct {
	SegmentNumber int
	Mask          *roi.RasterMask
}

// FrameMaskExtractor indexes the layout and labels of a SEG document for use
// with multiple frames. It does not place frames on a volume grid.
type FrameMaskExtractor struct {
	doc       *Document
	threshold float64
	pixels    int
	labels    map[uint16]struct{}
}

// NewFrameMaskExtractor prepares conversion of stored frames to binary masks.
// Use a new extractor if the document's dimensions, layout or label definitions
// change.
func NewFrameMaskExtractor(doc *Document, opts ROIOptions) (*FrameMaskExtractor, error) {
	if doc == nil {
		return nil, fmt.Errorf("%w: document is nil", ErrInvalidObject)
	}
	if doc.Columns <= 0 || doc.Rows <= 0 || doc.Columns > math.MaxInt/doc.Rows {
		return nil, fmt.Errorf("%w: Rows=%d Columns=%d", ErrInvalidObject, doc.Rows, doc.Columns)
	}
	extractor := &FrameMaskExtractor{
		doc:       doc,
		threshold: opts.FractionalThreshold,
		pixels:    doc.Rows * doc.Columns,
	}
	// Label-map masks leave out the background (Pixel Padding Value, 0 by
	// default); any other described value, 0 included, is a segment.
	background := labelMapBackground(doc)
	for _, segment := range doc.Segments {
		if segment.Number >= 0 && segment.Number <= math.MaxUint16 && segment.Number != background {
			if extractor.labels == nil {
				extractor.labels = make(map[uint16]struct{}, len(doc.Segments))
			}
			extractor.labels[uint16(segment.Number)] = struct{}{}
		}
	}
	return extractor, nil
}

// Masks returns nonempty masks with the stored segment numbers. LABELMAP masks
// are ordered by first appearance in row-major order, and ignore the
// background (the document's LabelMapBackground) and any label without a
// segment definition. FRACTIONAL masks use
// FractionalCutoff and, like LABELMAP masks, are detached from the frame data.
// BINARY masks are borrowed so a caller can retain the editable mask identity.
func (e *FrameMaskExtractor) Masks(frame Frame) ([]FrameMask, error) {
	doc := e.doc
	switch {
	case len(frame.LabelMap) > 0 || doc.SOPClassUID == LabelMapSegmentationStorage || doc.SegmentationType == SegmentationTypeLabelMap:
		if len(frame.LabelMap) != e.pixels {
			return nil, fmt.Errorf("%w: label-map frame has %d samples, want %d", ErrInvalidObject, len(frame.LabelMap), e.pixels)
		}
		return e.labelMapMasks(frame.LabelMap), nil
	case len(frame.Fractional) > 0 || doc.SegmentationType == SegmentationTypeFractional:
		if len(frame.Fractional) == 0 {
			// An omitted fractional payload represents all zero samples.
			return nil, nil
		}
		if len(frame.Fractional) != e.pixels {
			return nil, fmt.Errorf("%w: fractional frame has %d samples, want %d", ErrInvalidObject, len(frame.Fractional), e.pixels)
		}
		if frame.SegmentNumber <= 0 {
			return nil, fmt.Errorf("%w: fractional frame has segment number %d", ErrInvalidObject, frame.SegmentNumber)
		}
		mask, err := fractionalMask(doc, frame, e.threshold)
		if err != nil {
			return nil, err
		}
		if !mask.Empty() {
			return []FrameMask{{SegmentNumber: frame.SegmentNumber, Mask: mask}}, nil
		}
	case frame.Mask != nil:
		if frame.Mask.Columns != doc.Columns || frame.Mask.Rows != doc.Rows {
			return nil, fmt.Errorf("%w: binary frame mask is %dx%d, want %dx%d", ErrInvalidObject, frame.Mask.Columns, frame.Mask.Rows, doc.Columns, doc.Rows)
		}
		if !frame.Mask.Empty() {
			return []FrameMask{{SegmentNumber: frame.SegmentNumber, Mask: frame.Mask}}, nil
		}
	}
	return nil, nil
}

func (e *FrameMaskExtractor) labelMapMasks(samples []uint16) []FrameMask {
	masks := make(map[uint16]*roi.RasterMask)
	var out []FrameMask
	columns, rows := e.doc.Columns, e.doc.Rows
	for y := 0; y < rows; y++ {
		row := samples[y*columns : (y+1)*columns]
		for x := 0; x < columns; {
			label := row[x]
			end := x + 1
			for end < columns && row[end] == label {
				end++
			}
			if _, described := e.labels[label]; described {
				mask := masks[label]
				if mask == nil {
					mask = roi.NewRasterMask(columns, rows)
					masks[label] = mask
					out = append(out, FrameMask{SegmentNumber: int(label), Mask: mask})
				}
				mask.SetRun(y, x, end)
			}
			x = end
		}
	}
	return out
}
