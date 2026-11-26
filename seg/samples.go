package seg

import (
	"context"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/internal/objectview"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/roi"
)

// readSampleLayout checks Bits Allocated against the segmentation type
// (PS3.3 C.8.20.2: 1 for BINARY, 8 for FRACTIONAL, 8 or 16 for LABELMAP) and
// reads the FRACTIONAL attributes and the label-map background. An absent
// Bits Allocated is taken as the type's value.
func readSampleLayout(obj *object.Object, doc *Document) error {
	bitsAllocated := derivedio.Int(obj, derivedio.TagBitsAllocated)
	switch {
	case doc.SOPClassUID == LabelMapSegmentationStorage:
		if bitsAllocated == 0 {
			bitsAllocated = 16
		}
		if bitsAllocated != 8 && bitsAllocated != 16 {
			return fmt.Errorf("%w: label map with Bits Allocated %d", ErrUnsupportedPixelData, bitsAllocated)
		}
		doc.LabelMapBitsAllocated = bitsAllocated
		doc.LabelMapBackground = readLabelMapBackground(obj)
	case doc.SegmentationType == SegmentationTypeFractional:
		if bitsAllocated != 0 && bitsAllocated != 8 {
			return fmt.Errorf("%w: FRACTIONAL segmentation with Bits Allocated %d", ErrUnsupportedPixelData, bitsAllocated)
		}
		doc.FractionalType = derivedio.CleanString(obj, tagSegmentationFractionalType)
		if doc.FractionalType != FractionalTypeProbability && doc.FractionalType != FractionalTypeOccupancy {
			return fmt.Errorf("%w: Segmentation Fractional Type %q", ErrInvalidObject, doc.FractionalType)
		}
		doc.MaxFractionalValue = derivedio.Int(obj, tagMaximumFractionalValue)
		if doc.MaxFractionalValue < 1 || doc.MaxFractionalValue > 255 {
			return fmt.Errorf("%w: Maximum Fractional Value %d is outside 1-255", ErrInvalidObject, doc.MaxFractionalValue)
		}
	default:
		if bitsAllocated != 0 && bitsAllocated != 1 {
			return fmt.Errorf("%w: BINARY segmentation with Bits Allocated %d", ErrUnsupportedPixelData, bitsAllocated)
		}
	}
	return nil
}

func readFractionalFrames(obj *object.Object, doc *Document, frameItems []*object.Object) []Frame {
	var frames []Frame
	if objectview.VisitNativePixels(obj, derivedio.TagPixelData, func(data objectview.Raw) {
		frames = readFractionalFramesFromPixelData(data, obj, doc, frameItems)
	}) {
		return frames
	}
	return readFractionalFramesFromPixelData(objectview.Raw{}, obj, doc, frameItems)
}

func readFractionalFramesFromPixelData(data objectview.Raw, obj *object.Object, doc *Document, frameItems []*object.Object) []Frame {
	frames, _ := readFractionalFramesFromPixelDataContext(context.Background(), data, obj, doc, frameItems)
	return frames
}

func readFractionalFramesFromPixelDataContext(ctx context.Context, data objectview.Raw, obj *object.Object, doc *Document, frameItems []*object.Object) ([]Frame, error) {
	samplesPerFrame := doc.Rows * doc.Columns
	spatialDimensionIndex := spatialDimensionValueIndex(obj)
	frames := make([]Frame, 0, len(frameItems))
	for i, item := range frameItems {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		frame := readFrameItem(obj, item, i, spatialDimensionIndex, doc.Geometry)
		if frame.SegmentNumber == 0 && len(doc.Segments) == 1 {
			frame.SegmentNumber = doc.Segments[0].Number
		}
		start := i * samplesPerFrame
		end := min(start+samplesPerFrame, data.Len())
		if start < end {
			frame.Fractional = make([]uint8, samplesPerFrame)
			for index := start; index < end; index++ {
				if index&4095 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				frame.Fractional[index-start] = data.Byte(index)
			}
		}
		frames = append(frames, frame)
	}
	return frames, nil
}

// DefaultFractionalThreshold is used when a caller supplies a threshold of zero.
const DefaultFractionalThreshold = 0.5

func normalizeFractionalThreshold(threshold float64) (float64, error) {
	if threshold == 0 {
		return DefaultFractionalThreshold, nil
	}
	if !(threshold > 0 && threshold <= 1) {
		return 0, fmt.Errorf("%w: fractional threshold %v is outside (0, 1]", ErrInvalidObject, threshold)
	}
	return threshold, nil
}

// FractionalCutoff is the minimum stored sample included in a binary mask
// at the requested fraction of maxValue. Zero threshold selects the default
// and sample zero is always excluded.
func FractionalCutoff(maxValue int, threshold float64) (int, error) {
	threshold, err := normalizeFractionalThreshold(threshold)
	if err != nil {
		return 0, err
	}
	if maxValue < 1 || maxValue > 255 {
		return 0, fmt.Errorf("%w: Maximum Fractional Value %d is outside 1-255", ErrInvalidObject, maxValue)
	}
	// Preserve the whole-volume conversion tolerance at integer cutoffs.
	return max(1, int(math.Ceil(threshold*float64(maxValue)-1e-9))), nil
}

// fractionalMask is the mask of the samples at or above threshold times the
// Maximum Fractional Value (and above zero).
func fractionalMask(doc *Document, frame Frame, threshold float64) (*roi.RasterMask, error) {
	cut, err := FractionalCutoff(doc.MaxFractionalValue, threshold)
	if err != nil {
		return nil, err
	}
	if doc.Columns <= 0 {
		return nil, fmt.Errorf("%w: fractional frame has non-positive columns", ErrInvalidObject)
	}
	mask := roi.NewRasterMask(doc.Columns, doc.Rows)
	for y := 0; y < doc.Rows; y++ {
		row := y * doc.Columns
		runStart := -1
		for x := 0; x <= doc.Columns; x++ {
			inside := x < doc.Columns && row+x < len(frame.Fractional) && int(frame.Fractional[row+x]) >= cut
			switch {
			case inside && runStart < 0:
				runStart = x
			case !inside && runStart >= 0:
				mask.SetRun(y, runStart, x)
				runStart = -1
			}
		}
	}
	return mask, nil
}

// segmentsOverlapValue computes Segments Overlap (0062,0013): NO for a label
// map; YES or NO from the frames otherwise (a FRACTIONAL sample above zero
// counts as presence).
func segmentsOverlapValue(doc *Document, sopClassUID string) (string, error) {
	if sopClassUID == LabelMapSegmentationStorage {
		return SegmentsOverlapNo, nil
	}
	report, err := Overlaps(doc)
	if err != nil {
		return "", err
	}
	if len(report.Overlaps) > 0 {
		return SegmentsOverlapYes, nil
	}
	return SegmentsOverlapNo, nil
}

// readLabelMapBackground returns the background of a label map: its Pixel
// Padding Value (0028,0120) (PS3.3 A.51.4), or 0 when absent. A label map's
// Pixel Representation is unsigned, so a value encoded as SS is used when it
// is not negative and dropped otherwise, as DCMTK dcmseg does.
func readLabelMapBackground(obj *object.Object) uint16 {
	values := derivedio.Ints(obj, tagPixelPaddingValue)
	if len(values) == 0 || values[0] < 0 || values[0] > math.MaxUint16 {
		return 0
	}
	return uint16(values[0])
}
