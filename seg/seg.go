package seg

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/derivation"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/internal/dicomcolor"
	"github.com/ThalesMMS/dicom-go/internal/objectview"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/roi"
)

const (
	SegmentationStorage         = "1.2.840.10008.5.1.4.1.1.66.4"
	LabelMapSegmentationStorage = "1.2.840.10008.5.1.4.1.1.66.7"

	SegmentationTypeBinary     = "BINARY"
	SegmentationTypeFractional = "FRACTIONAL"
	SegmentationTypeLabelMap   = "LABELMAP"

	// DefaultContentLabel is the Content Label Write uses when the
	// document has none.
	DefaultContentLabel = "SEGMENTATION"

	// Segmentation Fractional Type (0062,0010) enumerated values.
	FractionalTypeProbability = "PROBABILITY"
	FractionalTypeOccupancy   = "OCCUPANCY"

	// Segments Overlap (0062,0013) enumerated values.
	SegmentsOverlapYes       = "YES"
	SegmentsOverlapNo        = "NO"
	SegmentsOverlapUndefined = "UNDEFINED"

	// Segment Algorithm Type (0062,0008) enumerated values. AUTOMATIC and
	// SEMIAUTOMATIC segments need a Segment Algorithm Name (0062,0009).
	AlgorithmManual        = "MANUAL"
	AlgorithmAutomatic     = "AUTOMATIC"
	AlgorithmSemiautomatic = "SEMIAUTOMATIC"

	// MaxReadFrames bounds the number of frame objects materialized by Read.
	MaxReadFrames = 100_000
	// MaxReadPixels bounds the total number of pixels visited while decoding.
	MaxReadPixels int64 = 1 << 30

	maxReadDimension = 1<<16 - 1
)

var (
	ErrUnsupportedSOPClass         = errors.New("dicom/seg: unsupported SOP class")
	ErrUnsupportedSegmentationType = errors.New("dicom/seg: unsupported segmentation type")
	ErrUnsupportedPixelData        = errors.New("dicom/seg: unsupported Pixel Data")
	ErrUnsupportedFramePayload     = errors.New("dicom/seg: unsupported frame payload")
	ErrResourceLimitExceeded       = errors.New("dicom/seg: resource limit exceeded")
	ErrInvalidObject               = errors.New("dicom/seg: invalid object")
	ErrMissingReference            = errors.New("dicom/seg: missing reference")
	// ErrTruncatedPixelData reports Pixel Data shorter than Number of Frames
	// × Rows × Columns samples. ReadWithOptions accepts it on request.
	ErrTruncatedPixelData = errors.New("dicom/seg: truncated Pixel Data")
)

// ReadOptions relaxes Read.
type ReadOptions struct {
	// AllowTruncatedPixelData reads Pixel Data shorter than its frames: the
	// missing samples read as zero (background) and the ReadReport lists the
	// incomplete frames. Read rejects such data with ErrTruncatedPixelData.
	AllowTruncatedPixelData bool
}

// ReadReport describes what ReadWithOptions recovered.
type ReadReport struct {
	// PixelDataBytes is the length of Pixel Data and ExpectedPixelDataBytes
	// the length its frames require.
	PixelDataBytes         int64
	ExpectedPixelDataBytes int64
	// IncompleteFrames are the indexes, in Document.Frames, of the frames
	// whose samples were missing in part or in full and were read as zero.
	IncompleteFrames []int
}

// Truncated reports whether Pixel Data was shorter than its frames.
func (r ReadReport) Truncated() bool { return len(r.IncompleteFrames) > 0 }

var (
	tagSourceImageSequence           = core.NewTag(0x0008, 0x2112)
	tagDerivationImageSequence       = core.NewTag(0x0008, 0x9124)
	tagReferencedSeriesSequence      = core.NewTag(0x0008, 0x1115)
	tagReferencedInstanceSequence    = core.NewTag(0x0008, 0x114A)
	tagCodeValue                     = core.NewTag(0x0008, 0x0100)
	tagCodingSchemeDesignator        = core.NewTag(0x0008, 0x0102)
	tagCodeMeaning                   = core.NewTag(0x0008, 0x0104)
	tagSliceThickness                = core.NewTag(0x0018, 0x0050)
	tagSpacingBetweenSlices          = core.NewTag(0x0018, 0x0088)
	tagImagePositionPatient          = core.NewTag(0x0020, 0x0032)
	tagImageOrientationPatient       = core.NewTag(0x0020, 0x0037)
	tagInStackPositionNumber         = core.NewTag(0x0020, 0x9057)
	tagPixelPaddingValue             = core.NewTag(0x0028, 0x0120)
	tagStackID                       = core.NewTag(0x0020, 0x9056)
	tagFrameContentSequence          = core.NewTag(0x0020, 0x9111)
	tagSegmentSequence               = core.NewTag(0x0062, 0x0002)
	tagSegmentationType              = core.NewTag(0x0062, 0x0001)
	tagSegmentedPropertyCategorySeq  = core.NewTag(0x0062, 0x0003)
	tagSegmentNumber                 = core.NewTag(0x0062, 0x0004)
	tagSegmentLabel                  = core.NewTag(0x0062, 0x0005)
	tagSegmentAlgorithmType          = core.NewTag(0x0062, 0x0008)
	tagSegmentAlgorithmName          = core.NewTag(0x0062, 0x0009)
	tagSegmentIdentificationSequence = core.NewTag(0x0062, 0x000A)
	tagRecommendedDisplayCIELabValue = core.NewTag(0x0062, 0x000D)
	tagSegmentedPropertyTypeSeq      = core.NewTag(0x0062, 0x000F)
	tagMaximumFractionalValue        = core.NewTag(0x0062, 0x000E)
	tagSegmentationFractionalType    = core.NewTag(0x0062, 0x0010)
	tagSegmentsOverlap               = core.NewTag(0x0062, 0x0013)
	tagPixelSpacing                  = core.NewTag(0x0028, 0x0030)
	tagPixelMeasuresSequence         = core.NewTag(0x0028, 0x9110)
	tagDimensionIndexValues          = core.NewTag(0x0020, 0x9157)
	tagDimensionIndexPointer         = core.NewTag(0x0020, 0x9165)
	tagFunctionalGroupPointer        = core.NewTag(0x0020, 0x9167)
	tagDimensionIndexSequence        = core.NewTag(0x0020, 0x9222)
	tagPlanePositionSequence         = core.NewTag(0x0020, 0x9113)
	tagPlaneOrientationSequence      = core.NewTag(0x0020, 0x9116)
	tagSharedFunctionalGroups        = core.NewTag(0x5200, 0x9229)
	tagImageType                     = core.NewTag(0x0008, 0x0008)
	tagLossyImageCompression         = core.NewTag(0x0028, 0x2110)
	tagDimensionOrganizationSequence = core.NewTag(0x0020, 0x9221)
	tagDimensionOrganizationUID      = core.NewTag(0x0020, 0x9164)
	tagPerFrameFunctionalGroups      = core.NewTag(0x5200, 0x9230)
)

type ReferencedImage struct {
	SeriesInstanceUID string
	SOPClassUID       string
	SOPInstanceUID    string
	Frames            []int
}

type CodedEntry struct {
	CodeValue              string
	CodingSchemeDesignator string
	CodeMeaning            string
}

type Segment struct {
	Number        int
	Label         string
	AlgorithmType string
	// AlgorithmName is Segment Algorithm Name (0062,0009), required when
	// AlgorithmType is AUTOMATIC or SEMIAUTOMATIC.
	AlgorithmName string
	// AlgorithmIdentification is an optional single-item Segmentation Algorithm
	// Identification Sequence (0062,0007). The zero value omits the sequence.
	AlgorithmIdentification   AlgorithmIdentification
	RecommendedDisplayCIELab  [3]uint16
	SegmentedPropertyCategory CodedEntry
	SegmentedPropertyType     CodedEntry
}

// ValidateAlgorithm checks the segment's algorithm metadata without supplying
// defaults. Imported segments with unknown metadata can still be displayed,
// but must not be edited and saved as if their provenance were known.
func (segment Segment) ValidateAlgorithm() error {
	algorithmType := strings.TrimSpace(segment.AlgorithmType)
	switch algorithmType {
	case AlgorithmManual:
	case AlgorithmAutomatic, AlgorithmSemiautomatic:
		if strings.TrimSpace(segment.AlgorithmName) == "" {
			return fmt.Errorf("%w: segment %d is %s without a Segment Algorithm Name", ErrInvalidObject, segment.Number, algorithmType)
		}
	default:
		return fmt.Errorf("%w: segment %d algorithm type %q", ErrInvalidObject, segment.Number, algorithmType)
	}
	return segment.AlgorithmIdentification.Validate()
}

// RGBToDICOMCIELab converts an 8-bit sRGB color into DICOM's PCS-encoded
// CIELab triplet (PS3.3 C.10.7.1.1).
func RGBToDICOMCIELab(rgb [3]int) [3]uint16 {
	return dicomcolor.RGBToCIELab(rgb)
}

// DICOMCIELabToRGB converts a DICOM PCS-encoded CIELab triplet to 8-bit sRGB.
func DICOMCIELabToRGB(encoded [3]uint16) [3]int {
	return dicomcolor.CIELabToRGB(encoded)
}

type Frame struct {
	SegmentNumber            int
	SliceIndex               int
	ReferencedSOPInstanceUID string
	ReferencedFrameNumber    int
	Geometry                 render.SliceGeometry
	Mask                     *roi.RasterMask
	// DerivationDescription is optional per-frame provenance (0008,2111)
	// inside the Derivation Image Functional Group. It is not a coded claim of
	// clinical correctness; callers describe the actual processing performed.
	DerivationDescription string
	// LabelMap holds the segment number of every pixel of a LABELMAP frame,
	// row by row.
	LabelMap []uint16
	// Fractional holds the samples of a FRACTIONAL frame, row by row, from 0
	// to the document's MaxFractionalValue. An empty slice is all zero.
	Fractional []uint8
}

// Identification is the Series Number, Instance Number and Content
// Identification Macro (PS3.3 10.9) of a segmentation: Content Label (a CS
// value), Content Description and Content Creator's Name.
type Identification = derivedio.Identification

type Document struct {
	// Common fills the Patient, Study and equipment modules from a source
	// instance of the same patient (object.InheritCommonModules): Patient
	// Name, ID, Birth Date and Sex, Study Date and Time, Referring
	// Physician's Name, Study ID, Accession Number and Position Reference
	// Indicator are copied when the source has them and written empty (Type
	// 2) otherwise, and Manufacturer, Manufacturer's Model Name, Device Serial
	// Number and Software Versions (Enhanced General Equipment, Type 1) come
	// from Common.Equipment, dicom-go by default.
	Common              object.DerivedCommon
	SOPClassUID         string
	SOPInstanceUID      string
	StudyInstanceUID    string
	SeriesInstanceUID   string
	FrameOfReferenceUID string
	// Identification is the Series Number, Instance Number (both 1 by
	// default) and the Content Identification (Content Label SEGMENTATION
	// by default) of the Segmentation Series and Image modules.
	Identification Identification
	// ContentDate and ContentTime (Type 1) default to the time of Write.
	ContentDate      string
	ContentTime      string
	Rows             int
	Columns          int
	SegmentationType string
	// FractionalType (PROBABILITY or OCCUPANCY) and MaxFractionalValue (the
	// sample of probability 1 or full occupancy, 1 to 255) describe FRACTIONAL
	// segmentations.
	FractionalType     string
	MaxFractionalValue int
	// LabelMapBitsAllocated is 8 or 16 for a label map. Write picks 8 when it
	// is zero and every segment number fits.
	LabelMapBitsAllocated int
	// LabelMapBackground is the pixel value a label map treats as background
	// (not belonging to any segment), recorded as Pixel Padding Value
	// (0028,0120) (PS3.3 A.51.4, C.7.5.1.1.2 Note 6). Read takes it from that
	// attribute, or 0 when it is absent (a negative SS value is dropped as
	// non-conformant); Write records it for every label map and pads short
	// frames with it. It is ignored for BINARY and FRACTIONAL segmentations,
	// which must not carry Pixel Padding Value.
	LabelMapBackground uint16
	// SegmentsOverlap is Segments Overlap (0062,0013) as read (YES, NO,
	// UNDEFINED or empty). Write computes it from the frames instead.
	SegmentsOverlap  string
	Segments         []Segment
	ReferencedImages []ReferencedImage
	Geometry         render.VolumeGeometry
	Frames           []Frame
}

type FromROIOptions struct {
	SOPInstanceUID      string
	StudyInstanceUID    string
	SeriesInstanceUID   string
	FrameOfReferenceUID string
	Segment             Segment
	ReferencedImages    []ReferencedImage
	Segmentation        *roi.Segmentation3D
}

type ROIMask struct {
	Segment     Segment
	SliceIndex  int
	SourceImage ReferencedImage
	Geometry    render.SliceGeometry
	Mask        *roi.RasterMask
}

type FromROIMasksOptions struct {
	SOPInstanceUID      string
	StudyInstanceUID    string
	SeriesInstanceUID   string
	FrameOfReferenceUID string
	Rows                int
	Columns             int
	Geometry            render.VolumeGeometry
	Masks               []ROIMask
}

// FromROI converts a 3D ROI segmentation into a binary segmentation document.
// It returns ErrInvalidObject if the segmentation is nil.
func FromROI(opts FromROIOptions) (*Document, error) {
	if opts.Segmentation == nil {
		return nil, fmt.Errorf("%w: segmentation is required", ErrInvalidObject)
	}
	segment := opts.Segment
	if segment.Number == 0 {
		segment.Number = 1
	}
	masks := make([]ROIMask, 0, len(opts.Segmentation.Slices()))
	for _, slice := range opts.Segmentation.Slices() {
		mask, _ := opts.Segmentation.MaskAt(slice)
		var ref ReferencedImage
		if slice >= 0 && slice < len(opts.ReferencedImages) {
			ref = opts.ReferencedImages[slice]
		}
		masks = append(masks, ROIMask{
			Segment:     segment,
			SliceIndex:  slice,
			SourceImage: ref,
			Mask:        mask,
		})
	}
	doc, err := FromROIMasks(FromROIMasksOptions{
		SOPInstanceUID:      opts.SOPInstanceUID,
		StudyInstanceUID:    opts.StudyInstanceUID,
		SeriesInstanceUID:   opts.SeriesInstanceUID,
		FrameOfReferenceUID: opts.FrameOfReferenceUID,
		Rows:                opts.Segmentation.Rows,
		Columns:             opts.Segmentation.Columns,
		Geometry:            opts.Segmentation.Geometry,
		Masks:               masks,
	})
	if err != nil {
		return nil, err
	}
	doc.ReferencedImages = append([]ReferencedImage(nil), opts.ReferencedImages...)
	return doc, nil
}

func FromROIMasks(opts FromROIMasksOptions) (*Document, error) {
	if len(opts.Masks) == 0 {
		return nil, fmt.Errorf("%w: at least one ROI mask is required", ErrInvalidObject)
	}
	rows, columns := opts.Rows, opts.Columns
	if rows == 0 || columns == 0 {
		for _, input := range opts.Masks {
			if input.Mask != nil {
				rows = input.Mask.Rows
				columns = input.Mask.Columns
				break
			}
		}
	}
	if rows <= 0 || columns <= 0 {
		return nil, fmt.Errorf("%w: non-positive mask dimensions", ErrInvalidObject)
	}
	doc := &Document{
		SOPClassUID:         SegmentationStorage,
		SOPInstanceUID:      opts.SOPInstanceUID,
		StudyInstanceUID:    opts.StudyInstanceUID,
		SeriesInstanceUID:   opts.SeriesInstanceUID,
		FrameOfReferenceUID: opts.FrameOfReferenceUID,
		Rows:                rows,
		Columns:             columns,
		SegmentationType:    SegmentationTypeBinary,
		Geometry:            opts.Geometry,
	}
	segmentIndexes := map[int]int{}
	referenceIndexes := map[string]int{}
	for i, input := range opts.Masks {
		if input.Mask == nil {
			return nil, fmt.Errorf("%w: ROI mask %d is nil", ErrInvalidObject, i)
		}
		if input.Mask.Empty() {
			return nil, fmt.Errorf("%w: ROI mask %d is empty", ErrInvalidObject, i)
		}
		if input.Mask.Rows != rows || input.Mask.Columns != columns {
			return nil, fmt.Errorf("%w: ROI mask %d dimensions %dx%d do not match %dx%d", ErrInvalidObject, i, input.Mask.Columns, input.Mask.Rows, columns, rows)
		}
		ref := normalizeReference(input.SourceImage)
		if ref.SeriesInstanceUID == "" || ref.SOPClassUID == "" || ref.SOPInstanceUID == "" {
			return nil, fmt.Errorf("%w: ROI mask %d missing source image reference", ErrMissingReference, i)
		}
		segment := normalizeSegment(input.Segment, i+1)
		if existing, ok := segmentIndexes[segment.Number]; ok {
			if !sameSegmentDefinition(doc.Segments[existing], segment) {
				return nil, fmt.Errorf("%w: conflicting segment metadata for segment %d", ErrInvalidObject, segment.Number)
			}
		} else {
			segmentIndexes[segment.Number] = len(doc.Segments)
			doc.Segments = append(doc.Segments, segment)
		}
		key := referenceKey(ref)
		if _, ok := referenceIndexes[key]; !ok {
			referenceIndexes[key] = len(doc.ReferencedImages)
			doc.ReferencedImages = append(doc.ReferencedImages, ref)
		}
		frame := Frame{
			SegmentNumber:            segment.Number,
			SliceIndex:               input.SliceIndex,
			ReferencedSOPInstanceUID: ref.SOPInstanceUID,
			Geometry:                 input.Geometry,
			Mask:                     input.Mask.Clone(),
		}
		if len(ref.Frames) > 0 {
			frame.ReferencedFrameNumber = ref.Frames[0]
		}
		doc.Frames = append(doc.Frames, frame)
	}
	return doc, nil
}

func GenericROICategoryCode() CodedEntry {
	return CodedEntry{CodeValue: "ROI", CodingSchemeDesignator: "99TWIN", CodeMeaning: "Region of Interest"}
}

func GenericROITypeCode() CodedEntry {
	return CodedEntry{CodeValue: "ROI_MASK", CodingSchemeDesignator: "99TWIN", CodeMeaning: "ROI mask"}
}

func normalizeSegment(segment Segment, fallbackNumber int) Segment {
	if segment.Number == 0 {
		segment.Number = fallbackNumber
	}
	segment.Label = strings.TrimSpace(segment.Label)
	if segment.Label == "" {
		segment.Label = fmt.Sprintf("Segment %d", segment.Number)
	}
	segment.AlgorithmType = strings.TrimSpace(segment.AlgorithmType)
	segment.AlgorithmName = strings.TrimSpace(segment.AlgorithmName)
	if segment.AlgorithmType == "" {
		segment.AlgorithmType = AlgorithmManual
	}
	segment.SegmentedPropertyCategory = normalizeCode(segment.SegmentedPropertyCategory, GenericROICategoryCode())
	segment.SegmentedPropertyType = normalizeCode(segment.SegmentedPropertyType, GenericROITypeCode())
	return segment
}

func normalizeCode(code CodedEntry, fallback CodedEntry) CodedEntry {
	code.CodeValue = strings.TrimSpace(code.CodeValue)
	code.CodingSchemeDesignator = strings.TrimSpace(code.CodingSchemeDesignator)
	code.CodeMeaning = strings.TrimSpace(code.CodeMeaning)
	if code.CodeValue == "" && code.CodingSchemeDesignator == "" && code.CodeMeaning == "" {
		return fallback
	}
	return code
}

func sameSegmentDefinition(a, b Segment) bool {
	return a.Number == b.Number &&
		a.Label == b.Label &&
		a.AlgorithmType == b.AlgorithmType &&
		a.AlgorithmName == b.AlgorithmName &&
		a.AlgorithmIdentification == b.AlgorithmIdentification &&
		a.RecommendedDisplayCIELab == b.RecommendedDisplayCIELab &&
		a.SegmentedPropertyCategory == b.SegmentedPropertyCategory &&
		a.SegmentedPropertyType == b.SegmentedPropertyType
}

func normalizeReference(ref ReferencedImage) ReferencedImage {
	ref.SeriesInstanceUID = strings.TrimSpace(ref.SeriesInstanceUID)
	ref.SOPClassUID = strings.TrimSpace(ref.SOPClassUID)
	ref.SOPInstanceUID = strings.TrimSpace(ref.SOPInstanceUID)
	ref.Frames = append([]int(nil), ref.Frames...)
	return ref
}

func referenceKey(ref ReferencedImage) string {
	var b strings.Builder
	b.WriteString(ref.SeriesInstanceUID)
	b.WriteByte('|')
	b.WriteString(ref.SOPClassUID)
	b.WriteByte('|')
	b.WriteString(ref.SOPInstanceUID)
	for _, frame := range ref.Frames {
		b.WriteString(fmt.Sprintf("|%d", frame))
	}
	return b.String()
}

// ToROI reconstructs a three-dimensional ROI segmentation from a DICOM segmentation document for a specified segment. If segmentNumber is zero, it uses the first segment's number.
// FRACTIONAL frames are thresholded at half the Maximum Fractional Value
// (see ToROIWithOptions).
//
// Returns the reconstructed segmentation, or an error if doc is nil.
func ToROI(doc *Document, segmentNumber int) (*roi.Segmentation3D, error) {
	return ToROIWithOptions(doc, segmentNumber, ROIOptions{})
}

// ROIOptions configure ToROIWithOptions.
type ROIOptions struct {
	// FractionalThreshold is the fraction of the Maximum Fractional Value at
	// or above which a FRACTIONAL sample is inside the ROI; zero means 0.5.
	// Samples of zero are never inside.
	FractionalThreshold float64
}

// ToROIWithOptions is ToROI with a chosen threshold for FRACTIONAL frames.
func ToROIWithOptions(doc *Document, segmentNumber int, opts ROIOptions) (*roi.Segmentation3D, error) {
	if doc == nil {
		return nil, fmt.Errorf("%w: document is nil", ErrInvalidObject)
	}
	threshold, err := normalizeFractionalThreshold(opts.FractionalThreshold)
	if err != nil {
		return nil, err
	}
	if segmentNumber == 0 {
		// A label map may describe its background as a segment.
		background := labelMapBackground(doc)
		for _, segment := range doc.Segments {
			if segment.Number != 0 && segment.Number != background {
				segmentNumber = segment.Number
				break
			}
		}
	}
	out := roi.NewSegmentation3D(doc.Geometry, doc.Columns, doc.Rows)
	for _, frame := range doc.Frames {
		// A label-map frame holds every segment of its position.
		if len(frame.LabelMap) == 0 && frame.SegmentNumber != segmentNumber {
			continue
		}
		switch {
		case len(frame.Fractional) > 0 || doc.SegmentationType == SegmentationTypeFractional:
			mask, err := fractionalMask(doc, frame, threshold)
			if err != nil {
				return nil, err
			}
			out.SetMask(frame.SliceIndex, mask)
		case frame.Mask != nil:
			out.SetMask(frame.SliceIndex, frame.Mask)
		case len(frame.LabelMap) > 0:
			if doc.Columns <= 0 {
				return nil, fmt.Errorf("%w: label-map frame has non-positive columns", ErrInvalidObject)
			}
			mask := roi.NewRasterMask(doc.Columns, doc.Rows)
			for idx, value := range frame.LabelMap {
				if int(value) == segmentNumber {
					mask.Set(idx%doc.Columns, idx/doc.Columns, true)
				}
			}
			out.SetMask(frame.SliceIndex, mask)
		}
	}
	return out, nil
}

// Write serializes a Document into a DICOM segmentation file. If the Document's SOP class UID is not set, it defaults to SegmentationStorage (binary masks). It returns an error if the Document is nil, the SOP class UID is unsupported, or source-image references are incomplete, conflicting, or unresolved.
func Write(doc *Document) (*object.File, error) {
	return writeDocument(doc, true, "")
}

// writeDocument shares the unchanged metadata builder with the incremental
// path. That path validates its source separately and supplies exact overlap.
func writeDocument(doc *Document, pixels bool, overlap string) (*object.File, error) {
	if doc == nil {
		return nil, fmt.Errorf("%w: document is nil", ErrInvalidObject)
	}
	sopClassUID := doc.SOPClassUID
	if sopClassUID == "" {
		sopClassUID = SegmentationStorage
	}
	if sopClassUID != SegmentationStorage && sopClassUID != LabelMapSegmentationStorage {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedSOPClass, sopClassUID)
	}
	if err := validateRepresentableSegmentationType(sopClassUID, doc.SegmentationType); err != nil {
		return nil, err
	}
	sourceReferences, err := newSourceReferenceIndex(doc.ReferencedImages)
	if err != nil {
		return nil, err
	}
	resolvedSourceReferences, err := sourceReferences.resolveFrames(doc.Frames)
	if err != nil {
		return nil, err
	}
	if pixels {
		if err := validateFramePayloads(doc, sopClassUID); err != nil {
			return nil, err
		}
	}
	if err := validateWriteDimensions(doc); err != nil {
		return nil, err
	}
	if err := validateUnsignedShortFields(doc); err != nil {
		return nil, err
	}
	identification, err := doc.Identification.Elements(DefaultContentLabel)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidObject, err)
	}
	now := time.Now()
	contentDate, contentTime := doc.ContentDate, doc.ContentTime
	if contentDate == "" {
		contentDate = now.Format("20060102")
	}
	if contentTime == "" {
		contentTime = now.Format("150405")
	}
	elements := []core.Element{
		derivedio.UI(derivedio.TagSOPClassUID, sopClassUID),
		derivedio.UI(derivedio.TagSOPInstanceUID, doc.SOPInstanceUID),
		derivedio.CS(derivedio.TagModality, "SEG"),
		// A segmentation is derived, lossless and dated (Segmentation
		// Image and Multi-frame Functional Groups modules).
		derivedio.Strings(tagImageType, core.VRCS, []string{"DERIVED", "PRIMARY"}),
		derivedio.CS(tagLossyImageCompression, "00"),
		derivedio.Str(derivedio.TagContentDate, core.VRDA, contentDate),
		derivedio.Str(derivedio.TagContentTime, core.VRTM, contentTime),
		dimensionOrganizationSequence(doc),
		derivedio.UI(derivedio.TagStudyInstanceUID, doc.StudyInstanceUID),
		derivedio.UI(derivedio.TagSeriesInstanceUID, doc.SeriesInstanceUID),
		derivedio.UI(derivedio.TagFrameOfReferenceUID, doc.FrameOfReferenceUID),
		derivedio.US(derivedio.TagRows, uint16(doc.Rows)),
		derivedio.US(derivedio.TagColumns, uint16(doc.Columns)),
		derivedio.IS(derivedio.TagNumberOfFrames, len(doc.Frames)),
		derivedio.US(derivedio.TagSamplesPerPixel, 1),
		derivedio.CS(derivedio.TagPhotometricInterpretation, "MONOCHROME2"),
		derivedio.CS(tagSegmentationType, segmentationType(doc)),
		segmentSequence(doc.Segments, sopClassUID == LabelMapSegmentationStorage),
		referencedSeriesSequence(doc.ReferencedImages),
		dimensionIndexSequence(doc, sopClassUID),
	}
	elements = append(elements, identification...)
	// Shared Functional Groups Sequence is Type 2 with zero or one item; an
	// empty item keeps readers that expect exactly one (DCMTK) working.
	if shared, ok := sharedFunctionalGroupsSequence(doc); ok {
		elements = append(elements, shared)
	} else {
		elements = append(elements, derivedio.Seq(tagSharedFunctionalGroups, derivedio.DataSet()))
	}
	perFrame, err := perFrameSequence(doc, sopClassUID, resolvedSourceReferences)
	if err != nil {
		return nil, err
	}
	elements = append(elements, perFrame)
	if overlap == "" {
		overlap, err = segmentsOverlapValue(doc, sopClassUID)
		if err != nil {
			return nil, err
		}
	}
	elements = append(elements, derivedio.CS(tagSegmentsOverlap, overlap))
	switch {
	case sopClassUID == LabelMapSegmentationStorage:
		bitsAllocated, err := labelMapBitsAllocated(doc)
		if err != nil {
			return nil, err
		}
		pixelVR := core.VROW
		if bitsAllocated == 8 {
			pixelVR = core.VROB
		}
		elements = append(elements,
			derivedio.US(derivedio.TagBitsAllocated, uint16(bitsAllocated)),
			derivedio.US(derivedio.TagBitsStored, uint16(bitsAllocated)),
			derivedio.US(derivedio.TagHighBit, uint16(bitsAllocated-1)),
			derivedio.US(derivedio.TagPixelRepresentation, 0),
			// The background of a label map (PS3.3 A.51.4); US, as Pixel
			// Representation is 0.
			derivedio.US(tagPixelPaddingValue, doc.LabelMapBackground),
		)
		if pixels {
			elements = append(elements, derivedio.Raw(derivedio.TagPixelData, pixelVR, labelMapPixelData(doc, bitsAllocated)))
		}
	case segmentationType(doc) == SegmentationTypeFractional:
		elements = append(elements,
			derivedio.CS(tagSegmentationFractionalType, doc.FractionalType),
			derivedio.US(tagMaximumFractionalValue, uint16(doc.MaxFractionalValue)),
			derivedio.US(derivedio.TagBitsAllocated, 8),
			derivedio.US(derivedio.TagBitsStored, 8),
			derivedio.US(derivedio.TagHighBit, 7),
			derivedio.US(derivedio.TagPixelRepresentation, 0),
		)
		if pixels {
			elements = append(elements, derivedio.Raw(derivedio.TagPixelData, core.VROB, fractionalPixelData(doc)))
		}
	default:
		elements = append(elements,
			derivedio.US(derivedio.TagBitsAllocated, 1),
			derivedio.US(derivedio.TagBitsStored, 1),
			derivedio.US(derivedio.TagHighBit, 0),
			derivedio.US(derivedio.TagPixelRepresentation, 0),
		)
		if pixels {
			elements = append(elements, derivedio.Raw(derivedio.TagPixelData, core.VROB, binaryPixelData(doc)))
		}
	}
	dataset := derivedio.Object(elements...)
	if err := doc.Common.Apply(dataset, object.InheritAll); err != nil {
		return nil, err
	}
	return derivedio.File(sopClassUID, doc.SOPInstanceUID, dataset)
}

// Read parses a DICOM segmentation dataset into a Document.
// It returns an error if the dataset is nil, if the SOP class UID is not
// SegmentationStorage or LabelMapSegmentationStorage, if its segmentation
// type cannot be represented by Document, or ErrTruncatedPixelData if Pixel
// Data is shorter than its frames.
func Read(obj *object.Object) (*Document, error) {
	doc, _, err := ReadWithOptions(obj, ReadOptions{})
	return doc, err
}

// ReadWithOptions is Read with opt-in recovery; the report describes what was
// recovered.
func ReadWithOptions(obj *object.Object, opts ReadOptions) (*Document, ReadReport, error) {
	return readWithOptionsContext(context.Background(), obj, opts)
}

func readWithOptionsContext(ctx context.Context, obj *object.Object, opts ReadOptions) (*Document, ReadReport, error) {
	return readWithLayoutContext(ctx, obj, opts, false)
}

func readWithLayoutContext(ctx context.Context, obj *object.Object, opts ReadOptions, continuousBinary bool) (*Document, ReadReport, error) {
	var report ReadReport
	if err := ctx.Err(); err != nil {
		return nil, report, err
	}
	doc, err := readDocumentHeader(obj)
	if err != nil {
		return nil, report, err
	}
	return readDocumentPixelsContext(ctx, obj, doc, opts, continuousBinary)
}

func readDocumentPixelsContext(ctx context.Context, obj *object.Object, doc *Document, opts ReadOptions, continuousBinary bool) (*Document, ReadReport, error) {
	var report ReadReport
	if err := validateNativePixelData(obj); err != nil {
		return nil, report, err
	}
	geometry, err := readVolumeGeometryContext(ctx, obj, doc.Rows, doc.Columns)
	if err != nil {
		return nil, report, err
	}
	doc.Geometry = geometry
	doc.Frames, report, err = readFramesLayoutContext(ctx, obj, doc, continuousBinary)
	if err != nil {
		return nil, report, err
	}
	if report.Truncated() && !opts.AllowTruncatedPixelData {
		return nil, report, fmt.Errorf("%w: %d bytes for %d frames of %d x %d need %d (%d incomplete frames)", ErrTruncatedPixelData,
			report.PixelDataBytes, len(doc.Frames), doc.Rows, doc.Columns, report.ExpectedPixelDataBytes, len(report.IncompleteFrames))
	}
	sourceReferences, err := newSourceReferenceIndex(doc.ReferencedImages)
	if err != nil {
		return nil, report, err
	}
	if _, err := sourceReferences.resolveFrames(doc.Frames); err != nil {
		return nil, report, err
	}
	if err := ctx.Err(); err != nil {
		return nil, report, err
	}
	return doc, report, nil
}

// ReadHeader reads SEG identity, segment definitions and source references
// without materializing or validating pixel samples. Apply bounded metadata
// read options to the source Object; use Read/ReadContext for pixel validation.
func ReadHeader(obj *object.Object) (*Document, error) {
	doc, err := readDocumentHeader(obj)
	if err != nil {
		return nil, err
	}
	if len(doc.Segments) == 0 || len(doc.Segments) > 65535 {
		return nil, fmt.Errorf("%w: invalid segment count", ErrInvalidObject)
	}
	seen := map[int]bool{}
	for _, segment := range doc.Segments {
		if segment.Number < 1 || segment.Number > 65535 || seen[segment.Number] {
			return nil, fmt.Errorf("%w: invalid/duplicate segment number", ErrInvalidObject)
		}
		seen[segment.Number] = true
	}
	return doc, nil
}

func readDocumentHeader(obj *object.Object) (*Document, error) {
	if obj == nil {
		return nil, fmt.Errorf("%w: dataset is nil", ErrInvalidObject)
	}
	sopClassUID := derivedio.CleanUID(obj, derivedio.TagSOPClassUID)
	if sopClassUID != SegmentationStorage && sopClassUID != LabelMapSegmentationStorage {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedSOPClass, sopClassUID)
	}
	segments, err := readSegments(obj)
	if err != nil {
		return nil, err
	}
	doc := &Document{
		SOPClassUID:         sopClassUID,
		SOPInstanceUID:      derivedio.CleanUID(obj, derivedio.TagSOPInstanceUID),
		StudyInstanceUID:    derivedio.CleanUID(obj, derivedio.TagStudyInstanceUID),
		SeriesInstanceUID:   derivedio.CleanUID(obj, derivedio.TagSeriesInstanceUID),
		FrameOfReferenceUID: derivedio.CleanUID(obj, derivedio.TagFrameOfReferenceUID),
		Identification:      derivedio.ReadIdentification(obj),
		ContentDate:         derivedio.CleanString(obj, derivedio.TagContentDate),
		ContentTime:         derivedio.CleanString(obj, derivedio.TagContentTime),
		Rows:                derivedio.Int(obj, derivedio.TagRows),
		Columns:             derivedio.Int(obj, derivedio.TagColumns),
		SegmentationType:    derivedio.CleanString(obj, tagSegmentationType),
		SegmentsOverlap:     derivedio.CleanString(obj, tagSegmentsOverlap),
		Segments:            segments,
		ReferencedImages:    readReferencedImages(obj),
	}
	if err := validateRepresentableSegmentationType(doc.SOPClassUID, doc.SegmentationType); err != nil {
		return nil, err
	}
	if err := readSampleLayout(obj, doc); err != nil {
		return nil, err
	}
	if err := validateReadDimensions(obj, doc); err != nil {
		return nil, err
	}
	return doc, nil
}

func validateReadDimensions(obj *object.Object, doc *Document) error {
	if doc.Rows <= 0 || doc.Columns <= 0 {
		return fmt.Errorf("%w: Rows=%d Columns=%d", ErrInvalidObject, doc.Rows, doc.Columns)
	}
	if doc.Rows > maxReadDimension || doc.Columns > maxReadDimension {
		return fmt.Errorf("%w: Rows=%d Columns=%d exceed %d", ErrResourceLimitExceeded, doc.Rows, doc.Columns, maxReadDimension)
	}

	frameCount := len(derivedio.Sequence(obj, tagPerFrameFunctionalGroups))
	if frameCount == 0 {
		value, err := obj.GetInt(derivedio.TagNumberOfFrames)
		if err != nil || value <= 0 {
			return fmt.Errorf("%w: invalid Number Of Frames", ErrInvalidObject)
		}
		if value > int64(MaxReadFrames) {
			return fmt.Errorf("%w: Number Of Frames=%d exceeds %d", ErrResourceLimitExceeded, value, MaxReadFrames)
		}
		frameCount = int(value)
	}
	if frameCount > MaxReadFrames {
		return fmt.Errorf("%w: %d per-frame items exceed %d", ErrResourceLimitExceeded, frameCount, MaxReadFrames)
	}

	pixelsPerFrame := int64(doc.Rows) * int64(doc.Columns)
	if pixelsPerFrame > MaxReadPixels/int64(frameCount) {
		return fmt.Errorf("%w: %d x %d x %d pixels exceed %d", ErrResourceLimitExceeded, doc.Rows, doc.Columns, frameCount, MaxReadPixels)
	}
	return nil
}

func validateWriteDimensions(doc *Document) error {
	if doc.Rows <= 0 || doc.Columns <= 0 {
		return fmt.Errorf("%w: Rows=%d Columns=%d", ErrInvalidObject, doc.Rows, doc.Columns)
	}
	if doc.Rows > maxReadDimension || doc.Columns > maxReadDimension {
		return fmt.Errorf("%w: Rows=%d Columns=%d exceed %d", ErrResourceLimitExceeded, doc.Rows, doc.Columns, maxReadDimension)
	}
	if len(doc.Frames) <= 0 {
		return fmt.Errorf("%w: invalid Number Of Frames", ErrInvalidObject)
	}
	if len(doc.Frames) > MaxReadFrames {
		return fmt.Errorf("%w: Number Of Frames=%d exceeds %d", ErrResourceLimitExceeded, len(doc.Frames), MaxReadFrames)
	}
	return nil
}

func validateNativePixelData(obj *object.Object) error {
	elem, ok := obj.Get(derivedio.TagPixelData)
	if !ok {
		return fmt.Errorf("%w: Pixel Data is missing", ErrInvalidObject)
	}
	switch elem.Value.(type) {
	case core.RawValue:
		return nil
	case core.FragmentSequence:
		return fmt.Errorf("%w: encapsulated Pixel Data", ErrUnsupportedPixelData)
	default:
		return fmt.Errorf("%w: Pixel Data has value type %T", ErrInvalidObject, elem.Value)
	}
}

func readVolumeGeometryContext(ctx context.Context, obj *object.Object, rows, columns int) (render.VolumeGeometry, error) {
	template, fallbackSpacing, ok := readSliceGeometryTemplate(obj, rows, columns)
	if !ok {
		return render.VolumeGeometry{}, nil
	}

	frameItems := derivedio.Sequence(obj, tagPerFrameFunctionalGroups)
	spatialDimensionIndex := spatialDimensionValueIndex(obj)
	origins := make(map[int]render.Vec3, len(frameItems))
	firstIndex, lastIndex := -1, -1
	for i, item := range frameItems {
		if err := ctx.Err(); err != nil {
			return render.VolumeGeometry{}, err
		}
		index := i
		if values, valuesOK := readDimensionIndexValues(item); valuesOK {
			if decoded, indexOK := sliceIndexFromDimensionValues(values, spatialDimensionIndex); indexOK {
				index = decoded
			}
		}
		if index < 0 || index >= MaxReadFrames {
			return render.VolumeGeometry{}, fmt.Errorf("%w: slice index %d exceeds %d", ErrResourceLimitExceeded, index, MaxReadFrames-1)
		}
		origin, originOK := readFrameImagePosition(obj, i)
		if !originOK {
			continue
		}
		origins[index] = origin
		if firstIndex < 0 || index < firstIndex {
			firstIndex = index
		}
		if index > lastIndex {
			lastIndex = index
		}
	}
	if len(origins) == 0 {
		return render.VolumeGeometry{
			RowDir:            template.RowDir,
			ColDir:            template.ColDir,
			Normal:            template.Normal,
			MeanSpacing:       fallbackSpacing,
			Regular:           true,
			OrientationStable: true,
			Classification:    render.VolumeSingleSlice,
		}, nil
	}

	step := template.Normal.Scale(fallbackSpacing)
	if firstIndex != lastIndex {
		candidate := origins[lastIndex].Sub(origins[firstIndex]).Scale(1 / float64(lastIndex-firstIndex))
		if candidate.Length() > 0 {
			step = candidate
		}
	}

	slices := make([]render.SliceGeometry, 0, lastIndex+1)
	anchor := origins[firstIndex]
	if step.Length() > 0 {
		for index := 0; index <= lastIndex; index++ {
			if err := ctx.Err(); err != nil {
				return render.VolumeGeometry{}, err
			}
			geometry := template
			geometry.Origin = anchor.Add(step.Scale(float64(index - firstIndex)))
			if origin, exists := origins[index]; exists {
				geometry.Origin = origin
			}
			slices = append(slices, geometry)
		}
	} else {
		for index := 0; index <= lastIndex; index++ {
			if err := ctx.Err(); err != nil {
				return render.VolumeGeometry{}, err
			}
			origin, exists := origins[index]
			if !exists {
				continue
			}
			geometry := template
			geometry.Origin = origin
			slices = append(slices, geometry)
		}
	}

	geometry := render.BuildVolumeGeometry(slices, render.DefaultGeometryTolerances())
	if geometry.MeanSpacing <= 0 && fallbackSpacing > 0 {
		geometry.MeanSpacing = fallbackSpacing
	}
	return geometry, nil
}

// readSliceGeometryTemplate reads the Pixel Measures and Plane Orientation of
// the first frame, shared or per-frame, through the functional group resolver.
func readSliceGeometryTemplate(obj *object.Object, rows, columns int) (render.SliceGeometry, float64, bool) {
	pixelMeasures, _, _ := obj.FunctionalGroupItem(0, tagPixelMeasuresSequence)
	orientationItem, _, _ := obj.FunctionalGroupItem(0, tagPlaneOrientationSequence)
	if pixelMeasures == nil || orientationItem == nil {
		return render.SliceGeometry{}, 0, false
	}

	rowSpacing, columnSpacing, spacingValues, spacingErr := pixelMeasures.SpacingPair(tagPixelSpacing)
	spacing := []float64{rowSpacing, columnSpacing}
	orientation, orientationErr := orientationItem.GetFloats(tagImageOrientationPatient)
	if spacingErr != nil || spacingValues == object.SpacingValuesAbsent ||
		orientationErr != nil || len(orientation) < 6 {
		return render.SliceGeometry{}, 0, false
	}
	rowDir := render.Vec3{X: orientation[0], Y: orientation[1], Z: orientation[2]}.Normalize()
	colRaw := render.Vec3{X: orientation[3], Y: orientation[4], Z: orientation[5]}
	colDir := colRaw.Sub(rowDir.Scale(colRaw.Dot(rowDir))).Normalize()
	normal := rowDir.Cross(colDir).Normalize()
	if rowDir == (render.Vec3{}) || colDir == (render.Vec3{}) || normal == (render.Vec3{}) {
		return render.SliceGeometry{}, 0, false
	}

	fallbackSpacing := positiveFloat(pixelMeasures, tagSpacingBetweenSlices)
	if fallbackSpacing == 0 {
		fallbackSpacing = positiveFloat(pixelMeasures, tagSliceThickness)
	}
	return render.SliceGeometry{
		RowDir: rowDir, ColDir: colDir, Normal: normal,
		RowSpacing: spacing[0], ColSpacing: spacing[1], Rows: rows, Columns: columns,
	}, fallbackSpacing, true
}

func positiveFloat(obj *object.Object, tag core.Tag) float64 {
	values, err := obj.GetFloats(tag)
	if err != nil || len(values) == 0 || !positiveFinite(values[0]) {
		return 0
	}
	return values[0]
}

func positiveFinite(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func validateRepresentableSegmentationType(sopClassUID string, segmentationType string) error {
	switch sopClassUID {
	case SegmentationStorage:
		switch segmentationType {
		case "", SegmentationTypeBinary, SegmentationTypeFractional:
			return nil
		default:
			return fmt.Errorf("%w: %q for Segmentation Storage", ErrUnsupportedSegmentationType, segmentationType)
		}
	case LabelMapSegmentationStorage:
		if segmentationType == "" || segmentationType == SegmentationTypeLabelMap {
			return nil
		}
		return fmt.Errorf("%w: %q for Label Map Segmentation Storage", ErrUnsupportedSegmentationType, segmentationType)
	default:
		return fmt.Errorf("%w: %s", ErrUnsupportedSOPClass, sopClassUID)
	}
}

// segmentationType returns the segmentation type string for the document.
func segmentationType(doc *Document) string {
	if doc.SegmentationType != "" {
		return doc.SegmentationType
	}
	if doc.SOPClassUID == LabelMapSegmentationStorage {
		return SegmentationTypeLabelMap
	}
	return SegmentationTypeBinary
}

// segmentSequence encodes the Segment Sequence. Label-map segment numbers are
// pixel values and are kept as given, zero (a described background) included;
// other segments with no number are numbered by position.
func segmentSequence(segments []Segment, labelMap bool) core.Element {
	items := make([]core.DataSet, 0, len(segments))
	for i, segment := range segments {
		number := segment.Number
		hasLabel := strings.TrimSpace(segment.Label) != ""
		segment = normalizeSegment(segment, i+1)
		if labelMap {
			segment.Number = number
			if !hasLabel && number == 0 {
				segment.Label = "Segment 0"
			}
		}
		elements := []core.Element{
			derivedio.US(tagSegmentNumber, uint16(segment.Number)),
			derivedio.LO(tagSegmentLabel, segment.Label),
			derivedio.CS(tagSegmentAlgorithmType, segment.AlgorithmType),
			codeSequence(tagSegmentedPropertyCategorySeq, segment.SegmentedPropertyCategory),
			codeSequence(tagSegmentedPropertyTypeSeq, segment.SegmentedPropertyType),
		}
		if segment.AlgorithmName != "" {
			elements = append(elements, derivedio.LO(tagSegmentAlgorithmName, segment.AlgorithmName))
		}
		if segment.AlgorithmIdentification != (AlgorithmIdentification{}) {
			elements = append(elements, algorithmIdentificationSequence(segment.AlgorithmIdentification))
		}
		if segment.RecommendedDisplayCIELab != ([3]uint16{}) {
			elements = append(elements, derivedio.US(tagRecommendedDisplayCIELabValue,
				segment.RecommendedDisplayCIELab[0],
				segment.RecommendedDisplayCIELab[1],
				segment.RecommendedDisplayCIELab[2],
			))
		}
		items = append(items, derivedio.DataSet(elements...))
	}
	return derivedio.Seq(tagSegmentSequence, items...)
}

func codeSequence(tag core.Tag, code CodedEntry) core.Element {
	return derivedio.Seq(tag, derivedio.DataSet(
		derivedio.SH(tagCodeValue, code.CodeValue),
		derivedio.SH(tagCodingSchemeDesignator, code.CodingSchemeDesignator),
		derivedio.LO(tagCodeMeaning, code.CodeMeaning),
	))
}

// referencedSeriesSequence constructs a DICOM referenced series sequence from the provided images.
func referencedSeriesSequence(refs []ReferencedImage) core.Element {
	type seriesGroup struct {
		seriesUID string
		images    []core.DataSet
	}
	var groups []seriesGroup
	index := map[string]int{}
	for _, ref := range refs {
		if i, ok := index[ref.SeriesInstanceUID]; ok {
			groups[i].images = append(groups[i].images, refDataSet(ref))
			continue
		}
		index[ref.SeriesInstanceUID] = len(groups)
		groups = append(groups, seriesGroup{seriesUID: ref.SeriesInstanceUID, images: []core.DataSet{refDataSet(ref)}})
	}
	items := make([]core.DataSet, 0, len(groups))
	for _, group := range groups {
		items = append(items, derivedio.DataSet(
			derivedio.UI(derivedio.TagSeriesInstanceUID, group.seriesUID),
			derivedio.Seq(tagReferencedInstanceSequence, group.images...),
		))
	}
	return derivedio.Seq(tagReferencedSeriesSequence, items...)
}

func validateFramePayloads(doc *Document, sopClassUID string) error {
	return validateFramePayloadsContext(context.Background(), doc, sopClassUID)
}

func validateFramePayloadsContext(ctx context.Context, doc *Document, sopClassUID string) error {
	fractional := sopClassUID == SegmentationStorage && segmentationType(doc) == SegmentationTypeFractional
	if fractional {
		if doc.FractionalType != FractionalTypeProbability && doc.FractionalType != FractionalTypeOccupancy {
			return fmt.Errorf("%w: Segmentation Fractional Type %q", ErrInvalidObject, doc.FractionalType)
		}
		if doc.MaxFractionalValue < 1 || doc.MaxFractionalValue > 255 {
			return fmt.Errorf("%w: Maximum Fractional Value %d is outside 1-255", ErrInvalidObject, doc.MaxFractionalValue)
		}
	}
	pixels := doc.Rows * doc.Columns
	var described map[int]bool
	if sopClassUID == LabelMapSegmentationStorage {
		described = make(map[int]bool, len(doc.Segments))
		for _, segment := range doc.Segments {
			described[segment.Number] = true
		}
	}
	for i, frame := range doc.Frames {
		if err := ctx.Err(); err != nil {
			return err
		}
		if frame.Mask != nil && (frame.Mask.Rows != doc.Rows || frame.Mask.Columns != doc.Columns) {
			return fmt.Errorf("%w: frame %d mask dimensions %dx%d do not match %dx%d", ErrInvalidObject, i, frame.Mask.Columns, frame.Mask.Rows, doc.Columns, doc.Rows)
		}
		if len(frame.Fractional) > 0 && !fractional {
			return fmt.Errorf("%w: frame %d has fractional samples in a %s segmentation", ErrUnsupportedFramePayload, i, segmentationType(doc))
		}
		switch {
		case fractional:
			if len(frame.Fractional) == 0 && frame.Mask != nil && !frame.Mask.Empty() {
				return fmt.Errorf("%w: fractional frame %d contains only a binary mask", ErrUnsupportedFramePayload, i)
			}
			if len(frame.Fractional) != 0 && len(frame.Fractional) != pixels {
				return fmt.Errorf("%w: fractional frame %d has %d samples, want %d", ErrInvalidObject, i, len(frame.Fractional), pixels)
			}
			for j, sample := range frame.Fractional {
				if j%4096 == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				if int(sample) > doc.MaxFractionalValue {
					return fmt.Errorf("%w: fractional frame %d sample %d exceeds Maximum Fractional Value %d", ErrInvalidObject, i, sample, doc.MaxFractionalValue)
				}
			}
			continue
		case sopClassUID == LabelMapSegmentationStorage:
			if len(frame.LabelMap) == 0 && frame.Mask != nil && !frame.Mask.Empty() {
				return fmt.Errorf("%w: label-map frame %d contains only a binary mask", ErrUnsupportedFramePayload, i)
			}
			if len(frame.LabelMap) > pixels {
				return fmt.Errorf("%w: label-map frame %d has %d values, want %d", ErrInvalidObject, i, len(frame.LabelMap), pixels)
			}
			// Every encoded value, including the background that pads a
			// short frame, must be a described segment (PS3.3 C.8.20.2.3.3).
			if len(frame.LabelMap) < pixels && !described[int(doc.LabelMapBackground)] {
				return fmt.Errorf("%w: label-map frame %d pads with background %d, which no segment describes", ErrInvalidObject, i, doc.LabelMapBackground)
			}
			for j, value := range frame.LabelMap {
				if j%4096 == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				if !described[int(value)] {
					return fmt.Errorf("%w: label-map frame %d uses value %d, which no segment describes", ErrInvalidObject, i, value)
				}
			}
		}
		switch sopClassUID {
		case SegmentationStorage:
			if frame.Mask == nil && labelMapHasValues(frame.LabelMap) {
				return fmt.Errorf("%w: binary frame %d contains only label-map values", ErrUnsupportedFramePayload, i)
			}
		case LabelMapSegmentationStorage:
			if len(frame.LabelMap) == 0 && frame.Mask != nil && !frame.Mask.Empty() {
				return fmt.Errorf("%w: label-map frame %d contains only a binary mask", ErrUnsupportedFramePayload, i)
			}
		}
	}
	return nil
}

func validateUnsignedShortFields(doc *Document) error {
	if doc.Rows <= 0 || doc.Rows > 1<<16-1 || doc.Columns <= 0 || doc.Columns > 1<<16-1 {
		return fmt.Errorf("%w: dimensions %dx%d are outside US", ErrInvalidObject, doc.Columns, doc.Rows)
	}
	labelMap := doc.SOPClassUID == LabelMapSegmentationStorage
	seen := make(map[int]bool, len(doc.Segments))
	for i, segment := range doc.Segments {
		number := segment.Number
		if !labelMap {
			number = normalizeSegment(segment, i+1).Number
		}
		if number < 0 || (number == 0 && !labelMap) || number > 1<<16-1 {
			return fmt.Errorf("%w: segment %d number %d is outside US", ErrInvalidObject, i, number)
		}
		if seen[number] {
			return fmt.Errorf("%w: segment number %d is not unique", ErrInvalidObject, number)
		}
		normalized := normalizeSegment(segment, i+1)
		normalized.Number = number
		if err := normalized.ValidateAlgorithm(); err != nil {
			return err
		}
		seen[number] = true
	}
	if labelMap {
		return nil
	}
	for i, frame := range doc.Frames {
		if frame.SegmentNumber <= 0 || frame.SegmentNumber > 1<<16-1 {
			return fmt.Errorf("%w: frame %d segment number %d is outside US", ErrInvalidObject, i, frame.SegmentNumber)
		}
	}
	return nil
}

func labelMapHasValues(values []uint16) bool {
	for _, value := range values {
		if value != 0 {
			return true
		}
	}
	return false
}

// refDataSet builds a DICOM dataset with the referenced image's SOP class and instance UIDs, and frame numbers if available.
func refDataSet(ref ReferencedImage) core.DataSet {
	elements := []core.Element{
		derivedio.UI(derivedio.TagRefSOPClassUID, ref.SOPClassUID),
		derivedio.UI(derivedio.TagRefSOPInstanceUID, ref.SOPInstanceUID),
	}
	if len(ref.Frames) > 0 {
		elements = append(elements, derivedio.IS(derivedio.TagRefFrameNumber, ref.Frames...))
	}
	return derivedio.DataSet(elements...)
}

func sharedFunctionalGroupsSequence(doc *Document) (core.Element, bool) {
	if doc == nil {
		return core.Element{}, false
	}
	geom, ok := sharedGeometry(doc)
	if !ok {
		return core.Element{}, false
	}
	elements := []core.Element{
		derivedio.Seq(tagPixelMeasuresSequence, derivedio.DataSet(
			derivedio.DS(tagPixelSpacing, geom.RowSpacing, geom.ColSpacing),
			derivedio.DS(tagSliceThickness, geometryThickness(doc.Geometry)),
		)),
		derivedio.Seq(tagPlaneOrientationSequence, derivedio.DataSet(
			derivedio.DS(tagImageOrientationPatient,
				geom.RowDir.X, geom.RowDir.Y, geom.RowDir.Z,
				geom.ColDir.X, geom.ColDir.Y, geom.ColDir.Z,
			),
		)),
	}
	if doc.Geometry.MeanSpacing > 0 {
		elements[0] = derivedio.Seq(tagPixelMeasuresSequence, derivedio.DataSet(
			derivedio.DS(tagPixelSpacing, geom.RowSpacing, geom.ColSpacing),
			derivedio.DS(tagSliceThickness, geometryThickness(doc.Geometry)),
			derivedio.DS(tagSpacingBetweenSlices, doc.Geometry.MeanSpacing),
		))
	}
	return derivedio.Seq(tagSharedFunctionalGroups, derivedio.DataSet(elements...)), true
}

func sharedGeometry(doc *Document) (render.SliceGeometry, bool) {
	for _, frame := range doc.Frames {
		if validSliceGeometry(frame.Geometry) {
			return frame.Geometry, true
		}
	}
	if len(doc.Geometry.Slices) > 0 {
		return doc.Geometry.Slices[0], true
	}
	return render.SliceGeometry{}, false
}

func geometryThickness(geometry render.VolumeGeometry) float64 {
	if geometry.MeanSpacing > 0 {
		return geometry.MeanSpacing
	}
	return 1
}

// dimensionIndexSequence describes the frame dimensions: the Referenced
// Segment Number (except in a label map, whose frames hold every segment)
// and the frame position, as Image Position (Patient) when every frame has
// one, otherwise as In-Stack Position Number in a single stack.
// dimensionOrganizationUID identifies the single Dimension Organization of
// a segmentation, derived from its SOP Instance UID.
func dimensionOrganizationUID(doc *Document) string {
	return derivedio.DerivedUID(doc.SOPInstanceUID, "dimension-organization")
}

func dimensionOrganizationSequence(doc *Document) core.Element {
	return derivedio.Seq(tagDimensionOrganizationSequence, derivedio.DataSet(derivedio.UI(tagDimensionOrganizationUID, dimensionOrganizationUID(doc))))
}

func dimensionIndexSequence(doc *Document, sopClassUID string) core.Element {
	organization := derivedio.UI(tagDimensionOrganizationUID, dimensionOrganizationUID(doc))
	var items []core.DataSet
	if sopClassUID != LabelMapSegmentationStorage {
		items = append(items, derivedio.DataSet(
			tagPointerElement(tagDimensionIndexPointer, tagReferencedSegmentNumber()),
			tagPointerElement(tagFunctionalGroupPointer, tagSegmentIdentificationSequence),
			organization,
		))
	}
	if framesHavePositions(doc) {
		items = append(items, derivedio.DataSet(
			tagPointerElement(tagDimensionIndexPointer, tagImagePositionPatient),
			tagPointerElement(tagFunctionalGroupPointer, tagPlanePositionSequence),
			organization,
		))
	} else {
		items = append(items, derivedio.DataSet(
			tagPointerElement(tagDimensionIndexPointer, tagInStackPositionNumber),
			tagPointerElement(tagFunctionalGroupPointer, tagFrameContentSequence),
			organization,
		))
	}
	return derivedio.Seq(tagDimensionIndexSequence, items...)
}

func framesHavePositions(doc *Document) bool {
	for _, frame := range doc.Frames {
		if _, ok := geometryForFrame(doc.Geometry, frame); !ok {
			return false
		}
	}
	return len(doc.Frames) > 0
}

func tagPointerElement(tag core.Tag, value core.Tag) core.Element {
	var raw [4]byte
	binary.LittleEndian.PutUint16(raw[0:], value.Group)
	binary.LittleEndian.PutUint16(raw[2:], value.Element)
	return core.NewRawElement(tag, core.VRAT, raw[:])
}

// perFrameSequence constructs the per-frame functional groups DICOM sequence, containing segment identification, dimension indices, and reference information for each frame.
func perFrameSequence(doc *Document, sopClassUID string, sourceReferences []ReferencedImage) (core.Element, error) {
	items := make([]core.DataSet, 0, len(doc.Frames))
	positioned := framesHavePositions(doc)
	for frameIndex, frame := range doc.Frames {
		var elements []core.Element
		// The Segmentation Functional Group is required unless the type is
		// LABELMAP, where it is absent (PS3.3 Table A.51-2).
		if sopClassUID != LabelMapSegmentationStorage {
			elements = append(elements, derivedio.Seq(tagSegmentIdentificationSequence, derivedio.DataSet(derivedio.US(tagReferencedSegmentNumber(), uint16(frame.SegmentNumber)))))
		}
		elements = append(elements, frameContentSequence(frame, sopClassUID != LabelMapSegmentationStorage, !positioned))
		if geom, ok := geometryForFrame(doc.Geometry, frame); ok {
			elements = append(elements, derivedio.Seq(tagPlanePositionSequence, derivedio.DataSet(
				derivedio.DS(tagImagePositionPatient, geom.Origin.X, geom.Origin.Y, geom.Origin.Z),
			)))
		}
		if frame.ReferencedSOPInstanceUID != "" {
			derivationImage, err := derivationImageSequence(sourceReferences[frameIndex], frame)
			if err != nil {
				return core.Element{}, fmt.Errorf("%w: frame %d: %v", ErrMissingReference, frameIndex+1, err)
			}
			elements = append(elements, derivationImage)
		}
		items = append(items, derivedio.DataSet(elements...))
	}
	return derivedio.Seq(tagPerFrameFunctionalGroups, items...), nil
}

func geometryForFrame(geometry render.VolumeGeometry, frame Frame) (render.SliceGeometry, bool) {
	if validSliceGeometry(frame.Geometry) {
		return frame.Geometry, true
	}
	if frame.SliceIndex >= 0 && frame.SliceIndex < len(geometry.Slices) {
		return geometry.Slices[frame.SliceIndex], true
	}
	if len(geometry.Slices) == 1 {
		return geometry.Slices[0], true
	}
	return render.SliceGeometry{}, false
}

func validSliceGeometry(geometry render.SliceGeometry) bool {
	return geometry.Rows > 0 &&
		geometry.Columns > 0 &&
		geometry.RowSpacing > 0 &&
		geometry.ColSpacing > 0 &&
		geometry.RowDir.Length() > 0 &&
		geometry.ColDir.Length() > 0
}

// derivationImageSequence is the Derivation Image Functional Group of a
// frame (PS3.3 C.8.20.3.1): derivation 113076 (Segmentation) from the source
// frame, referenced as the input of image processing (121322); both codes
// are Type 1 in the Derivation Image Macro.
func derivationImageSequence(ref ReferencedImage, frame Frame) (core.Element, error) {
	source := derivation.Source{SOPClassUID: ref.SOPClassUID, SOPInstanceUID: frame.ReferencedSOPInstanceUID, Purpose: &derivation.SourceImageForProcessing}
	if frame.ReferencedFrameNumber > 0 {
		source.Frames = []int{frame.ReferencedFrameNumber}
	}
	item, err := derivation.Derivation{Description: frame.DerivationDescription, Codes: []derivation.Code{derivation.Segmentation}, Sources: []derivation.Source{source}}.Item()
	if err != nil {
		return core.Element{}, err
	}
	return derivedio.Seq(tagDerivationImageSequence, item), nil
}

type sourceReferenceIndex struct {
	bySOPInstanceUID map[string]int
	entries          []sourceReferenceEntry
	byFrame          map[sourceReferenceFrameKey]indexedSourceReference
}

type sourceReferenceEntry struct {
	first              ReferencedImage
	firstIndex         int
	firstSelectorIndex int
	wildcard           indexedSourceReference
	hasWildcard        bool
}

type sourceReferenceFrameKey struct {
	sopInstanceUID string
	frameNumber    int
}

type indexedSourceReference struct {
	reference ReferencedImage
	index     int
}

// newSourceReferenceIndex validates source-reference selectors and indexes
// them without changing the order used by Referenced Series Sequence. Empty
// frame lists are wildcards; explicit lists for the same SOP instance may be
// split across entries only when they are disjoint and metadata agrees.
func newSourceReferenceIndex(refs []ReferencedImage) (*sourceReferenceIndex, error) {
	frameCount := 0
	for _, ref := range refs {
		frameCount += len(ref.Frames)
	}
	index := &sourceReferenceIndex{
		bySOPInstanceUID: make(map[string]int, len(refs)),
		entries:          make([]sourceReferenceEntry, 0, len(refs)),
		byFrame:          make(map[sourceReferenceFrameKey]indexedSourceReference, frameCount),
	}
	for refIndex, ref := range refs {
		if ref.SeriesInstanceUID == "" {
			return nil, fmt.Errorf("%w: referenced image %d missing Series Instance UID", ErrMissingReference, refIndex)
		}
		if ref.SOPClassUID == "" {
			return nil, fmt.Errorf("%w: referenced image %d missing SOP Class UID", ErrMissingReference, refIndex)
		}
		if ref.SOPInstanceUID == "" {
			return nil, fmt.Errorf("%w: referenced image %d missing SOP Instance UID", ErrMissingReference, refIndex)
		}
		entryIndex, exists := index.bySOPInstanceUID[ref.SOPInstanceUID]
		if !exists {
			entryIndex = len(index.entries)
			index.bySOPInstanceUID[ref.SOPInstanceUID] = entryIndex
			index.entries = append(index.entries, sourceReferenceEntry{
				first: ref, firstIndex: refIndex, firstSelectorIndex: -1,
			})
		}
		entry := &index.entries[entryIndex]
		if exists && (entry.first.SeriesInstanceUID != ref.SeriesInstanceUID || entry.first.SOPClassUID != ref.SOPClassUID) {
			return nil, fmt.Errorf(
				"%w: referenced images %d and %d conflict for SOP Instance UID %s",
				ErrInvalidObject, entry.firstIndex, refIndex, ref.SOPInstanceUID,
			)
		}

		if len(ref.Frames) == 0 {
			if entry.hasWildcard {
				return nil, duplicateSourceReferenceError(ref.SOPInstanceUID, 0, entry.wildcard.index, refIndex)
			}
			if entry.firstSelectorIndex >= 0 {
				return nil, overlappingSourceReferenceError(ref.SOPInstanceUID, entry.firstSelectorIndex, refIndex)
			}
			entry.wildcard = indexedSourceReference{reference: ref, index: refIndex}
			entry.hasWildcard = true
			entry.firstSelectorIndex = refIndex
			continue
		}
		if entry.hasWildcard {
			return nil, overlappingSourceReferenceError(ref.SOPInstanceUID, entry.wildcard.index, refIndex)
		}
		for _, frameNumber := range ref.Frames {
			if frameNumber <= 0 {
				return nil, fmt.Errorf(
					"%w: referenced image %d has non-positive frame number %d",
					ErrInvalidObject, refIndex, frameNumber,
				)
			}
			key := sourceReferenceFrameKey{sopInstanceUID: ref.SOPInstanceUID, frameNumber: frameNumber}
			if previous, exists := index.byFrame[key]; exists {
				return nil, duplicateSourceReferenceError(ref.SOPInstanceUID, frameNumber, previous.index, refIndex)
			}
			index.byFrame[key] = indexedSourceReference{reference: ref, index: refIndex}
			if entry.firstSelectorIndex < 0 {
				entry.firstSelectorIndex = refIndex
			}
		}
	}
	return index, nil
}

func duplicateSourceReferenceError(sopInstanceUID string, frameNumber, firstIndex, secondIndex int) error {
	selector := "all frames"
	if frameNumber > 0 {
		selector = fmt.Sprintf("frame %d", frameNumber)
	}
	return fmt.Errorf(
		"%w: referenced images %d and %d duplicate SOP Instance UID %s %s",
		ErrInvalidObject, firstIndex, secondIndex, sopInstanceUID, selector,
	)
}

func overlappingSourceReferenceError(sopInstanceUID string, firstIndex, secondIndex int) error {
	return fmt.Errorf(
		"%w: referenced images %d and %d overlap wildcard and explicit frames for SOP Instance UID %s",
		ErrInvalidObject, firstIndex, secondIndex, sopInstanceUID,
	)
}

func (index *sourceReferenceIndex) resolveFrames(frames []Frame) ([]ReferencedImage, error) {
	resolved := make([]ReferencedImage, len(frames))
	for frameIndex, frame := range frames {
		if frame.ReferencedSOPInstanceUID == "" {
			if frame.ReferencedFrameNumber != 0 {
				return nil, fmt.Errorf(
					"%w: frame %d has referenced frame number %d without a source SOP Instance UID",
					ErrMissingReference, frameIndex, frame.ReferencedFrameNumber,
				)
			}
			continue
		}
		ref, ok := index.lookup(frame)
		if !ok {
			return nil, fmt.Errorf(
				"%w: frame %d references unlisted source SOP Instance UID %s frame %d",
				ErrMissingReference, frameIndex, frame.ReferencedSOPInstanceUID, frame.ReferencedFrameNumber,
			)
		}
		resolved[frameIndex] = ref
	}
	return resolved, nil
}

func (index *sourceReferenceIndex) lookup(frame Frame) (ReferencedImage, bool) {
	if index != nil {
		if entryIndex, ok := index.bySOPInstanceUID[frame.ReferencedSOPInstanceUID]; ok {
			entry := &index.entries[entryIndex]
			if frame.ReferencedFrameNumber == 0 {
				return entry.first, true
			}
			key := sourceReferenceFrameKey{
				sopInstanceUID: frame.ReferencedSOPInstanceUID,
				frameNumber:    frame.ReferencedFrameNumber,
			}
			if ref, ok := index.byFrame[key]; ok {
				return ref.reference, true
			}
			if entry.hasWildcard {
				return entry.wildcard.reference, true
			}
		}
	}
	return ReferencedImage{}, false
}

// tagReferencedSegmentNumber returns the DICOM tag (0062,000B) for the referenced segment number.
func tagReferencedSegmentNumber() core.Tag {
	return core.NewTag(0x0062, 0x000B)
}

// binaryPixelData encodes binary segmentation masks from the document frames into packed DICOM PixelData bytes, where each bit represents a pixel value.
func binaryPixelData(doc *Document) []byte {
	bitsPerFrame := doc.Rows * doc.Columns
	if bitsPerFrame <= 0 {
		return nil
	}
	totalBits := bitsPerFrame * len(doc.Frames)
	out := make([]byte, (totalBits+7)/8)
	for frameIndex, frame := range doc.Frames {
		if frame.Mask == nil {
			continue
		}
		baseBit := frameIndex * bitsPerFrame
		frame.Mask.ForEachRun(func(y int, run roi.MaskRun) {
			rowBit := baseBit + y*doc.Columns
			setPackedBitRange(out, rowBit+run.Start, rowBit+run.End)
		})
	}
	return out
}

func setPackedBitRange(data []byte, startBit, endBit int) {
	if startBit < 0 {
		startBit = 0
	}
	dataBits := len(data) * 8
	if endBit > dataBits {
		endBit = dataBits
	}
	if startBit >= endBit {
		return
	}
	if offset := startBit & 7; offset != 0 {
		count := minInt(8-offset, endBit-startBit)
		data[startBit>>3] |= lowBitMask(count) << uint(offset)
		startBit += count
	}
	for endBit-startBit >= 8 {
		data[startBit>>3] = 0xFF
		startBit += 8
	}
	if startBit < endBit {
		data[startBit>>3] |= lowBitMask(endBit - startBit)
	}
}

func lowBitMask(count int) byte {
	if count <= 0 {
		return 0
	}
	if count >= 8 {
		return 0xFF
	}
	return byte(1<<uint(count)) - 1
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// labelMapPixelData returns the serialized pixel data for the label-map
// frames in a document, with one or two bytes per value.
func labelMapPixelData(doc *Document, bitsAllocated int) []byte {
	valuesPerFrame := doc.Rows * doc.Columns
	bytesPerValue := bitsAllocated / 8
	out := make([]byte, valuesPerFrame*len(doc.Frames)*bytesPerValue)
	for frameIndex, frame := range doc.Frames {
		base := frameIndex * valuesPerFrame * bytesPerValue
		for i := 0; i < valuesPerFrame; i++ {
			// A short frame is padded with the background.
			value := doc.LabelMapBackground
			if i < len(frame.LabelMap) {
				value = frame.LabelMap[i]
			}
			if bytesPerValue == 1 {
				out[base+i] = byte(value)
			} else {
				binary.LittleEndian.PutUint16(out[base+2*i:], value)
			}
		}
	}
	if len(out)%2 != 0 {
		out = append(out, 0)
	}
	return out
}

// labelMapBitsAllocated returns 8 or 16: the requested width, or 8 when every
// described segment number, pixel value and the background fit in a byte.
func labelMapBitsAllocated(doc *Document) (int, error) {
	// Pixel Padding Value is a sample value too.
	largest := int(doc.LabelMapBackground)
	for _, segment := range doc.Segments {
		largest = max(largest, segment.Number)
	}
	for _, frame := range doc.Frames {
		for _, value := range frame.LabelMap {
			largest = max(largest, int(value))
		}
	}
	switch doc.LabelMapBitsAllocated {
	case 0:
		if largest <= 0xFF {
			return 8, nil
		}
		return 16, nil
	case 8:
		if largest > 0xFF {
			return 0, fmt.Errorf("%w: label %d does not fit 8-bit label-map samples", ErrInvalidObject, largest)
		}
		return 8, nil
	case 16:
		return 16, nil
	default:
		return 0, fmt.Errorf("%w: label-map Bits Allocated %d is not 8 or 16", ErrInvalidObject, doc.LabelMapBitsAllocated)
	}
}

// fractionalPixelData concatenates the 8-bit samples of the frames.
func fractionalPixelData(doc *Document) []byte {
	samplesPerFrame := doc.Rows * doc.Columns
	out := make([]byte, samplesPerFrame*len(doc.Frames), samplesPerFrame*len(doc.Frames)+1)
	for frameIndex, frame := range doc.Frames {
		copy(out[frameIndex*samplesPerFrame:], frame.Fractional)
	}
	if len(out)%2 != 0 {
		out = append(out, 0)
	}
	return out
}

// readSegments reads segment definitions from a DICOM dataset object.
func readSegments(obj *object.Object) ([]Segment, error) {
	items := derivedio.Sequence(obj, tagSegmentSequence)
	out := make([]Segment, 0, len(items))
	for _, item := range items {
		algorithm, err := readAlgorithmIdentification(item)
		if err != nil {
			return nil, err
		}
		segment := Segment{
			Number:                    derivedio.Int(item, tagSegmentNumber),
			Label:                     derivedio.CleanString(item, tagSegmentLabel),
			AlgorithmType:             derivedio.CleanString(item, tagSegmentAlgorithmType),
			AlgorithmName:             derivedio.CleanString(item, tagSegmentAlgorithmName),
			AlgorithmIdentification:   algorithm,
			SegmentedPropertyCategory: readCodeSequence(item, tagSegmentedPropertyCategorySeq),
			SegmentedPropertyType:     readCodeSequence(item, tagSegmentedPropertyTypeSeq),
		}
		if values := derivedio.Ints(item, tagRecommendedDisplayCIELabValue); len(values) >= 3 && uint16ValuesRepresentable(values[:3]) {
			segment.RecommendedDisplayCIELab = [3]uint16{uint16(values[0]), uint16(values[1]), uint16(values[2])}
		}
		out = append(out, segment)
	}
	return out, nil
}

func uint16ValuesRepresentable(values []int64) bool {
	for _, value := range values {
		if value < 0 || value > 1<<16-1 {
			return false
		}
	}
	return true
}

func readCodeSequence(obj *object.Object, tag core.Tag) CodedEntry {
	items := derivedio.Sequence(obj, tag)
	if len(items) == 0 {
		return CodedEntry{}
	}
	return CodedEntry{
		CodeValue:              derivedio.CleanString(items[0], tagCodeValue),
		CodingSchemeDesignator: derivedio.CleanString(items[0], tagCodingSchemeDesignator),
		CodeMeaning:            derivedio.CleanString(items[0], tagCodeMeaning),
	}
}

// readReferencedImages extracts referenced image identifiers from a DICOM segmentation dataset.
// It reads the referenced series and instance sequences and returns the SOP class UIDs,
// SOP instance UIDs, and frame numbers for each referenced image.
func readReferencedImages(obj *object.Object) []ReferencedImage {
	seriesItems := derivedio.Sequence(obj, tagReferencedSeriesSequence)
	var out []ReferencedImage
	for _, series := range seriesItems {
		seriesUID := derivedio.CleanUID(series, derivedio.TagSeriesInstanceUID)
		for _, refObj := range derivedio.Sequence(series, tagReferencedInstanceSequence) {
			ref := ReferencedImage{
				SeriesInstanceUID: seriesUID,
				SOPClassUID:       derivedio.CleanUID(refObj, derivedio.TagRefSOPClassUID),
				SOPInstanceUID:    derivedio.CleanUID(refObj, derivedio.TagRefSOPInstanceUID),
			}
			if frames, err := refObj.GetInts(derivedio.TagRefFrameNumber); err == nil {
				for _, frame := range frames {
					ref.Frames = append(ref.Frames, int(frame))
				}
			}
			out = append(out, ref)
		}
	}
	return out
}

// readFrames extracts frame data from a DICOM segmentation dataset. If
// per-frame metadata is absent, frames are populated based on the dataset's
// frame count. Samples missing from a short Pixel Data read as zero; the
// report lists the frames they belong to.
func readFrames(obj *object.Object, doc *Document) ([]Frame, ReadReport) {
	frameItems := derivedio.Sequence(obj, tagPerFrameFunctionalGroups)
	if len(frameItems) == 0 {
		frameCount := derivedio.Int(obj, derivedio.TagNumberOfFrames)
		frameItems = make([]*object.Object, frameCount)
	}
	dataBytes := 0
	objectview.VisitRaw(obj, derivedio.TagPixelData, func(data objectview.Raw) { dataBytes = data.Len() })
	report := pixelDataCoverage(doc, len(frameItems), dataBytes)
	switch {
	case doc.SOPClassUID == LabelMapSegmentationStorage:
		return readLabelMapFrames(obj, doc, frameItems), report
	case doc.SegmentationType == SegmentationTypeFractional:
		return readFractionalFrames(obj, doc, frameItems), report
	default:
		return readBinaryFrames(obj, doc, frameItems), report
	}
}

// pixelDataCoverage compares dataBytes of Pixel Data with the length that
// frameCount frames of doc's sample layout require: 1 bit per pixel packed
// across frames for BINARY, a byte for FRACTIONAL and 8 or 16 bits for a
// label map.
func pixelDataCoverage(doc *Document, frameCount, dataBytes int) ReadReport {
	pixels := int64(doc.Rows) * int64(doc.Columns)
	report := ReadReport{PixelDataBytes: int64(dataBytes)}
	if pixels <= 0 || frameCount <= 0 {
		return report
	}
	// A frame is complete when its last sample (bit) lies within the data.
	var sampleBits int64
	switch {
	case doc.SOPClassUID == LabelMapSegmentationStorage:
		sampleBits = 16
		if doc.LabelMapBitsAllocated == 8 {
			sampleBits = 8
		}
	case doc.SegmentationType == SegmentationTypeFractional:
		sampleBits = 8
	default:
		sampleBits = 1
	}
	frameBits := pixels * sampleBits
	stride := frameBits
	if sampleBits == 1 {
		stride = int64(binaryFrameBitStride(int(pixels), frameCount, dataBytes))
	}
	report.ExpectedPixelDataBytes = (frameBits*int64(frameCount) + 7) / 8
	available := int64(dataBytes) * 8
	for frame := 0; frame < frameCount; frame++ {
		if int64(frame)*stride+frameBits > available {
			report.IncompleteFrames = append(report.IncompleteFrames, frame)
		}
	}
	return report
}

// readBinaryFrames reads binary DICOM pixel data and creates Frame objects with raster masks for each frame.
func readBinaryFrames(obj *object.Object, doc *Document, frameItems []*object.Object) []Frame {
	var frames []Frame
	if objectview.VisitNativePixels(obj, derivedio.TagPixelData, func(data objectview.Raw) {
		frames = readBinaryFramesFromPixelData(data, obj, doc, frameItems)
	}) {
		return frames
	}
	return readBinaryFramesFromPixelData(objectview.Raw{}, obj, doc, frameItems)
}

func readBinaryFramesFromPixelData(data objectview.Raw, obj *object.Object, doc *Document, frameItems []*object.Object) []Frame {
	frames, _ := readBinaryFramesFromPixelDataContext(context.Background(), data, obj, doc, frameItems)
	return frames
}

func readBinaryFramesFromPixelDataContext(ctx context.Context, data objectview.Raw, obj *object.Object, doc *Document, frameItems []*object.Object) ([]Frame, error) {
	stride := binaryFrameBitStride(doc.Rows*doc.Columns, len(frameItems), data.Len())
	return readBinaryFramesWithStrideContext(ctx, data, obj, doc, frameItems, stride)
}

func readBinaryFramesWithStrideContext(ctx context.Context, data objectview.Raw, obj *object.Object, doc *Document, frameItems []*object.Object, frameBitStride int) ([]Frame, error) {
	bitsPerFrame := doc.Rows * doc.Columns
	if bitsPerFrame <= 0 {
		return nil, nil
	}
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
		baseBit := i * frameBitStride
		var err error
		frame.Mask, err = readPackedBinaryMaskContext(ctx, data, baseBit, doc.Columns, doc.Rows)
		if err != nil {
			return nil, err
		}
		frames = append(frames, frame)
	}
	return frames, nil
}

func readPackedBinaryMask(data objectview.Raw, baseBit, columns, rows int) *roi.RasterMask {
	mask, _ := readPackedBinaryMaskContext(context.Background(), data, baseBit, columns, rows)
	return mask
}

func readPackedBinaryMaskContext(ctx context.Context, data objectview.Raw, baseBit, columns, rows int) (*roi.RasterMask, error) {
	mask := roi.NewRasterMask(columns, rows)
	if data.Len() == 0 || columns <= 0 || rows <= 0 {
		return mask, nil
	}
	dataBits := data.Len() * 8
	for y := 0; y < rows; y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rowStart := baseBit + y*columns
		if rowStart >= dataBits {
			break
		}
		rowEnd := rowStart + columns
		if rowEnd > dataBits {
			rowEnd = dataBits
		}
		activeStart, activeEnd := -1, -1
		for position := rowStart; position < rowEnd; {
			offset := position & 7
			count := minInt(8-offset, rowEnd-position)
			value := (data.Byte(position>>3) >> uint(offset)) & lowBitMask(count)
			consumed := 0
			for consumed < count {
				zeros := bits.TrailingZeros8(value >> uint(consumed))
				if zeros >= count-consumed {
					break
				}
				start := consumed + zeros
				ones := bits.TrailingZeros8(^(value >> uint(start)))
				if ones > count-start {
					ones = count - start
				}
				runStart := position - rowStart + start
				runEnd := runStart + ones
				if activeStart < 0 {
					activeStart, activeEnd = runStart, runEnd
				} else if runStart == activeEnd {
					activeEnd = runEnd
				} else {
					mask.SetRun(y, activeStart, activeEnd)
					activeStart, activeEnd = runStart, runEnd
				}
				consumed = start + ones
			}
			position += count
		}
		if activeStart >= 0 {
			mask.SetRun(y, activeStart, activeEnd)
		}
	}
	return mask, nil
}

func binaryFrameBitStride(bitsPerFrame, frameCount, dataBytes int) int {
	bytesPerFrame := (bitsPerFrame + 7) / 8
	legacyBytes := bytesPerFrame * frameCount
	continuousBytes := (bitsPerFrame*frameCount + 7) / 8
	if bitsPerFrame%8 != 0 && dataBytes == legacyBytes && legacyBytes != continuousBytes {
		return bytesPerFrame * 8
	}
	return bitsPerFrame
}

func readLabelMapFrames(obj *object.Object, doc *Document, frameItems []*object.Object) []Frame {
	var frames []Frame
	if objectview.VisitNativePixels(obj, derivedio.TagPixelData, func(data objectview.Raw) {
		frames = readLabelMapFramesFromPixelData(data, obj, doc, frameItems)
	}) {
		return frames
	}
	return readLabelMapFramesFromPixelData(objectview.Raw{}, obj, doc, frameItems)
}

func readLabelMapFramesFromPixelData(data objectview.Raw, obj *object.Object, doc *Document, frameItems []*object.Object) []Frame {
	frames, _ := readLabelMapFramesFromPixelDataContext(context.Background(), data, obj, doc, frameItems)
	return frames
}

func readLabelMapFramesFromPixelDataContext(ctx context.Context, data objectview.Raw, obj *object.Object, doc *Document, frameItems []*object.Object) ([]Frame, error) {
	bytesPerValue := 2
	if doc.LabelMapBitsAllocated == 8 {
		bytesPerValue = 1
	}
	valueCount := data.Len() / bytesPerValue
	valuesPerFrame := doc.Rows * doc.Columns
	spatialDimensionIndex := spatialDimensionValueIndex(obj)
	frames := make([]Frame, 0, len(frameItems))
	for i, item := range frameItems {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		frame := readFrameItem(obj, item, i, spatialDimensionIndex, doc.Geometry)
		start := i * valuesPerFrame
		end := start + valuesPerFrame
		if start < valueCount {
			frame.LabelMap = make([]uint16, valuesPerFrame)
			if end > valueCount {
				end = valueCount
				// The samples missing from a truncated frame are background.
				for j := end - start; j < valuesPerFrame; j++ {
					frame.LabelMap[j] = doc.LabelMapBackground
				}
			}
			for valueIndex := start; valueIndex < end; valueIndex++ {
				if valueIndex&4095 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				if bytesPerValue == 1 {
					frame.LabelMap[valueIndex-start] = uint16(data.Byte(valueIndex))
				} else {
					frame.LabelMap[valueIndex-start] = data.Uint16LE(valueIndex)
				}
			}
		}
		frames = append(frames, frame)
	}
	return frames, nil
}

// readFrameItem extracts the metadata of frame frameIndex (also its default
// slice index): its functional group macros through the resolver, so shared
// macros apply too, and its Dimension Index Values from its per-frame item.
func readFrameItem(obj *object.Object, item *object.Object, frameIndex int, spatialDimensionIndex int, geometry render.VolumeGeometry) Frame {
	frame := Frame{SliceIndex: frameIndex}
	if segment, _, _ := obj.FunctionalGroupItem(frameIndex, tagSegmentIdentificationSequence); segment != nil {
		frame.SegmentNumber = derivedio.Int(segment, tagReferencedSegmentNumber())
	}
	if values, ok := readDimensionIndexValues(item); ok {
		if sliceIndex, ok := sliceIndexFromDimensionValues(values, spatialDimensionIndex); ok {
			frame.SliceIndex = sliceIndex
		}
	}
	if frame.SliceIndex >= 0 && frame.SliceIndex < len(geometry.Slices) {
		frame.Geometry = geometry.Slices[frame.SliceIndex]
	}
	if origin, ok := readFrameImagePosition(obj, frameIndex); ok {
		frame.Geometry.Origin = origin
		if sliceIndex, ok := sliceIndexFromPlanePosition(origin, geometry); ok {
			frame.SliceIndex = sliceIndex
			frame.Geometry = geometry.Slices[sliceIndex]
		}
	}
	// The source image is in the Derivation Image macro; dicom-go wrote it
	// directly in the per-frame item before.
	source := item
	if derivation, _, _ := obj.FunctionalGroupItem(frameIndex, tagDerivationImageSequence); derivation != nil {
		frame.DerivationDescription, _ = derivation.GetString(core.NewTag(0x0008, 0x2111))
		if sources := derivedio.Sequence(derivation, tagSourceImageSequence); len(sources) > 0 && sources[0] != nil {
			source = sources[0]
		}
	}
	if source != nil {
		frame.ReferencedSOPInstanceUID = derivedio.CleanUID(source, derivedio.TagRefSOPInstanceUID)
		frame.ReferencedFrameNumber = derivedio.Int(source, derivedio.TagRefFrameNumber)
	}
	return frame
}

// frameContentSequence is the Frame Content macro of a frame (PS3.3
// C.7.6.16.2.2): its Dimension Index Values (the segment, when segments are
// a dimension, then the one-based slice position) and, when frames are
// ordered as a stack, the Stack ID and In-Stack Position Number.
func frameContentSequence(frame Frame, segmentDimension, stack bool) core.Element {
	position := max(frame.SliceIndex+1, 1)
	values := []uint32{uint32(position)}
	if segmentDimension {
		values = []uint32{uint32(frame.SegmentNumber), uint32(position)}
	}
	raw := make([]byte, 4*len(values))
	for i, value := range values {
		binary.LittleEndian.PutUint32(raw[4*i:], value)
	}
	var elements []core.Element
	if stack {
		elements = append(elements,
			derivedio.SH(tagStackID, "1"),
			core.NewRawElement(tagInStackPositionNumber, core.VRUL, binary.LittleEndian.AppendUint32(nil, uint32(position))),
		)
	}
	elements = append(elements, core.NewRawElement(tagDimensionIndexValues, core.VRUL, raw))
	return derivedio.Seq(tagFrameContentSequence, derivedio.DataSet(elements...))
}

func spatialDimensionValueIndex(obj *object.Object) int {
	items := derivedio.Sequence(obj, tagDimensionIndexSequence)
	if len(items) == 0 {
		return -1
	}
	for i, item := range items {
		dimensionPointers := readTagValues(item, tagDimensionIndexPointer)
		if tagValueEquals(item, tagDimensionIndexPointer, tagImagePositionPatient) ||
			tagValueEquals(item, tagDimensionIndexPointer, tagInStackPositionNumber) {
			return i
		}
		if len(dimensionPointers) == 0 && tagValueEquals(item, tagFunctionalGroupPointer, tagPlanePositionSequence) {
			return i
		}
	}
	return -1
}

func tagValueEquals(obj *object.Object, sourceTag core.Tag, want core.Tag) bool {
	values := readTagValues(obj, sourceTag)
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func readTagValues(obj *object.Object, tag core.Tag) []core.Tag {
	if obj == nil {
		return nil
	}
	elem, ok := obj.Get(tag)
	if !ok || elem.VR() != core.VRAT {
		return nil
	}
	raw, ok := elem.RawBytes()
	if !ok || len(raw)%4 != 0 {
		return nil
	}
	values := make([]core.Tag, len(raw)/4)
	for i := range values {
		offset := i * 4
		values[i] = core.NewTag(
			binary.LittleEndian.Uint16(raw[offset:]),
			binary.LittleEndian.Uint16(raw[offset+2:]),
		)
	}
	return values
}

// readDimensionIndexValues reads the Dimension Index Values of a per-frame
// item from its Frame Content Sequence, or from the item itself, where
// dicom-go wrote them before.
func readDimensionIndexValues(obj *object.Object) ([]int, bool) {
	if obj == nil {
		return nil, false
	}
	if items := derivedio.Sequence(obj, tagFrameContentSequence); len(items) > 0 && items[0].Has(tagDimensionIndexValues) {
		obj = items[0]
	}
	elem, ok := obj.Get(tagDimensionIndexValues)
	if !ok {
		return nil, false
	}
	switch elem.VR() {
	case core.VRIS:
		values, err := obj.GetInts(tagDimensionIndexValues)
		if err != nil {
			return nil, false
		}
		out := make([]int, len(values))
		for i, value := range values {
			out[i] = int(value)
		}
		return out, true
	case core.VRUL:
		raw, ok := elem.RawBytes()
		if !ok || len(raw)%4 != 0 {
			return nil, false
		}
		out := make([]int, len(raw)/4)
		for i := range out {
			out[i] = int(binary.LittleEndian.Uint32(raw[i*4:]))
		}
		return out, true
	case core.VRUS:
		raw, ok := elem.RawBytes()
		if !ok || len(raw)%2 != 0 {
			return nil, false
		}
		out := make([]int, len(raw)/2)
		for i := range out {
			out[i] = int(binary.LittleEndian.Uint16(raw[i*2:]))
		}
		return out, true
	default:
		return nil, false
	}
}

func sliceIndexFromDimensionValues(values []int, spatialDimensionIndex int) (int, bool) {
	if spatialDimensionIndex < 0 || spatialDimensionIndex >= len(values) {
		return 0, false
	}
	value := values[spatialDimensionIndex]
	if value <= 0 {
		return 0, false
	}
	return value - 1, true
}

func readFrameImagePosition(obj *object.Object, frameIndex int) (render.Vec3, bool) {
	position, _, _ := obj.FunctionalGroupItem(frameIndex, tagPlanePositionSequence)
	if position == nil {
		return render.Vec3{}, false
	}
	values, err := position.GetFloats(tagImagePositionPatient)
	if err != nil || len(values) < 3 {
		return render.Vec3{}, false
	}
	return render.Vec3{X: values[0], Y: values[1], Z: values[2]}, true
}

func sliceIndexFromPlanePosition(origin render.Vec3, geometry render.VolumeGeometry) (int, bool) {
	if len(geometry.Slices) == 0 || geometry.Normal.Length() == 0 {
		return 0, false
	}
	position := origin.Dot(geometry.Normal)
	bestIndex := -1
	bestDistance := math.Inf(1)
	for i, slice := range geometry.Slices {
		distance := math.Abs(slice.PositionAlong(geometry.Normal) - position)
		if distance < bestDistance {
			bestDistance = distance
			bestIndex = i
		}
	}
	if bestIndex < 0 {
		return 0, false
	}
	// The DICOM grid tolerance (#1115): a twentieth of the smallest spacing.
	slice := geometry.Slices[bestIndex]
	spacing := math.Min(slice.RowSpacing, slice.ColSpacing)
	if geometry.MeanSpacing > 0 {
		spacing = math.Min(spacing, geometry.MeanSpacing)
	}
	if !(spacing > 0) {
		spacing = 1
	}
	if bestDistance > render.DICOMGridCoordinateTolerance*spacing {
		return 0, false
	}
	return bestIndex, true
}
