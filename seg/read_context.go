package seg

import (
	"context"

	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/internal/objectview"
	"github.com/ThalesMMS/dicom-go/object"
)

// readFramesContext retains the native reader's sample layout and recovery
// coverage, with cancellation during materialization as well as decoding.
func readFramesContext(ctx context.Context, obj *object.Object, doc *Document) ([]Frame, ReadReport, error) {
	return readFramesLayoutContext(ctx, obj, doc, false)
}

func readFramesLayoutContext(ctx context.Context, obj *object.Object, doc *Document, continuousBinary bool) ([]Frame, ReadReport, error) {
	if err := ctx.Err(); err != nil {
		return nil, ReadReport{}, err
	}
	items := derivedio.Sequence(obj, tagPerFrameFunctionalGroups)
	if len(items) == 0 {
		items = make([]*object.Object, derivedio.Int(obj, derivedio.TagNumberOfFrames))
	}
	var report ReadReport
	var frames []Frame
	var err error
	materialize := func(data objectview.Raw) {
		report = pixelDataCoverage(doc, len(items), data.Len())
		switch {
		case doc.SOPClassUID == LabelMapSegmentationStorage:
			frames, err = readLabelMapFramesFromPixelDataContext(ctx, data, obj, doc, items)
		case doc.SegmentationType == SegmentationTypeFractional:
			frames, err = readFractionalFramesFromPixelDataContext(ctx, data, obj, doc, items)
		default:
			if continuousBinary {
				frames, err = readBinaryFramesWithStrideContext(ctx, data, obj, doc, items, doc.Rows*doc.Columns)
			} else {
				frames, err = readBinaryFramesFromPixelDataContext(ctx, data, obj, doc, items)
			}
		}
	}
	if !objectview.VisitNativePixels(obj, derivedio.TagPixelData, materialize) {
		materialize(objectview.Raw{})
	}
	if err != nil {
		return nil, report, err
	}
	return frames, report, nil
}
