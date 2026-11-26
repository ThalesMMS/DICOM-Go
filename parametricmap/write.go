package parametricmap

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/derivation"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/render"
)

// Writing Parametric Maps (#1149): Write encodes a Document as a Parametric
// Map instance (PS3.3 A.75) with 32-bit float pixels, one frame per plane,
// the quantity and its units in a shared Real World Value Mapping (identity
// slope, so stored values are the quantity), Plane Position, Plane
// Orientation and Pixel Measures functional groups, Image Position
// (Patient) as the frame dimension, and the source images in the Derivation
// Image functional group and the Common Instance Reference module. The
// object is labelled Content Qualification RESEARCH unless set otherwise.
// Read reads it back.

// Identification is the Series Number, Instance Number and Content
// Identification Macro of a written map (Content Label defaults to
// PARAMETRIC_MAP).
type Identification = derivedio.Identification

// DefaultContentLabel is the Content Label of a map without one.
const DefaultContentLabel = "PARAMETRIC_MAP"

// Document describes a Parametric Map to write.
type Document struct {
	SOPInstanceUID, StudyInstanceUID, SeriesInstanceUID, FrameOfReferenceUID string
	// Modality is the modality of the source images (default MR).
	Modality string
	// SeriesDescription is optional.
	SeriesDescription string
	Rows, Columns     int
	// Frames are the planes of the map, in the order to store them.
	Frames []DocumentFrame
	// Quantity and Units describe the values (for example (113065, DCM,
	// "T2") in (ms, UCUM, "millisecond")); both are required.
	Quantity, Units Code
	// DerivationCode is the Derivation Code of the source references and
	// DerivationDescription their description; the code defaults to the
	// quantity.
	DerivationCode        Code
	DerivationDescription string
	// LUTLabel names the mapping (default the quantity's meaning, at most
	// 16 characters).
	LUTLabel string
	// DerivedPixelContrast is Value 4 of Image Type and Frame Type (for
	// example T2_MAP or ADC; default QUANTITY).
	DerivedPixelContrast string
	// ContentQualification is PRODUCT, RESEARCH or SERVICE (default
	// RESEARCH).
	ContentQualification string
	// SourceSeriesInstanceUID is the series of the source images, for the
	// Common Instance Reference module; required when a frame has a source.
	SourceSeriesInstanceUID string
	// Common fills the Patient, Study, Frame of Reference and equipment
	// modules from a source instance.
	Common         object.DerivedCommon
	Identification Identification
	// ContentDate and ContentTime default to the current time.
	ContentDate, ContentTime string
}

// DocumentFrame is one plane of a map.
type DocumentFrame struct {
	// Geometry places the plane (Origin is the centre of the first pixel).
	Geometry render.SliceGeometry
	// Values are Rows × Columns values, row by row; NaN marks pixels
	// without a value.
	Values []float32
	// Sources are the images the plane was computed from.
	Sources []Reference
}

var (
	tagImageType                = core.NewTag(0x0008, 0x0008)
	tagFrameType                = core.NewTag(0x0008, 0x9007)
	tagParametricMapFrameType   = core.NewTag(0x0040, 0x9092)
	tagContentQualification     = core.NewTag(0x0018, 0x9004)
	tagLossyImageCompression    = core.NewTag(0x0028, 0x2110)
	tagBurnedInAnnotation       = core.NewTag(0x0028, 0x0301)
	tagRecognizableVisual       = core.NewTag(0x0028, 0x0302)
	tagPresentationLUTShape     = core.NewTag(0x2050, 0x0020)
	tagPixelMeasuresSeq         = core.NewTag(0x0028, 0x9110)
	tagPixelSpacing             = core.NewTag(0x0028, 0x0030)
	tagSliceThickness           = core.NewTag(0x0018, 0x0050)
	tagSpacingBetweenSlices     = core.NewTag(0x0018, 0x0088)
	tagPlaneOrientationSeq      = core.NewTag(0x0020, 0x9116)
	tagImageOrientationPatient  = core.NewTag(0x0020, 0x0037)
	tagPlanePositionSeq         = core.NewTag(0x0020, 0x9113)
	tagFrameAnatomySeq          = core.NewTag(0x0020, 0x9071)
	tagFrameLaterality          = core.NewTag(0x0020, 0x9072)
	tagAnatomicRegionSeq        = core.NewTag(0x0008, 0x2218)
	tagDimensionOrganizationSeq = core.NewTag(0x0020, 0x9221)
	tagDimensionOrganizationUID = core.NewTag(0x0020, 0x9164)
	tagFunctionalGroupPointer   = core.NewTag(0x0020, 0x9167)
	tagDerivationImageSeq       = core.NewTag(0x0008, 0x9124)
	tagReferencedSeriesSeq      = core.NewTag(0x0008, 0x1115)
	tagReferencedInstanceSeq    = core.NewTag(0x0008, 0x114A)
	tagAcquisitionContextSeq    = core.NewTag(0x0040, 0x0555)
	tagLUTExplanation           = core.NewTag(0x0028, 0x3003)
	tagLUTLabel                 = core.NewTag(0x0040, 0x9210)
	tagValueType                = core.NewTag(0x0040, 0xA040)
	tagSeriesDescription        = core.NewTag(0x0008, 0x103E)
)

// Write encodes doc as a Parametric Map file.
func Write(doc *Document) (*object.File, error) {
	if err := doc.validate(); err != nil {
		return nil, err
	}
	identification, err := doc.Identification.Elements(DefaultContentLabel)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidObject, err)
	}
	now := time.Now()
	contentDate, contentTime := doc.ContentDate, doc.ContentTime
	if contentDate == "" {
		contentDate = now.Format("20060102")
	}
	if contentTime == "" {
		contentTime = now.Format("150405")
	}
	modality := strings.TrimSpace(doc.Modality)
	if modality == "" {
		modality = "MR"
	}
	contrast := strings.TrimSpace(doc.DerivedPixelContrast)
	if contrast == "" {
		contrast = "QUANTITY"
	}
	qualification := strings.TrimSpace(doc.ContentQualification)
	if qualification == "" {
		qualification = "RESEARCH"
	}
	imageType := []string{"DERIVED", "PRIMARY", "VOLUME", contrast}
	organization := derivedio.UI(tagDimensionOrganizationUID, derivedio.DerivedUID(doc.SOPInstanceUID, "dimension-organization"))
	first := doc.Frames[0].Geometry
	elements := []core.Element{
		derivedio.UI(derivedio.TagSOPClassUID, ParametricMapStorage),
		derivedio.UI(derivedio.TagSOPInstanceUID, doc.SOPInstanceUID),
		derivedio.UI(derivedio.TagStudyInstanceUID, doc.StudyInstanceUID),
		derivedio.UI(derivedio.TagSeriesInstanceUID, doc.SeriesInstanceUID),
		derivedio.UI(derivedio.TagFrameOfReferenceUID, doc.FrameOfReferenceUID),
		derivedio.CS(derivedio.TagModality, modality),
		derivedio.Strings(tagImageType, core.VRCS, imageType),
		derivedio.Str(derivedio.TagContentDate, core.VRDA, contentDate),
		derivedio.Str(derivedio.TagContentTime, core.VRTM, contentTime),
		derivedio.CS(tagContentQualification, qualification),
		derivedio.CS(tagLossyImageCompression, "00"),
		derivedio.CS(tagBurnedInAnnotation, "NO"),
		derivedio.CS(tagRecognizableVisual, "NO"),
		derivedio.CS(tagPresentationLUTShape, "IDENTITY"),
		derivedio.US(derivedio.TagRows, uint16(doc.Rows)),
		derivedio.US(derivedio.TagColumns, uint16(doc.Columns)),
		derivedio.IS(derivedio.TagNumberOfFrames, len(doc.Frames)),
		derivedio.US(derivedio.TagSamplesPerPixel, 1),
		derivedio.CS(derivedio.TagPhotometricInterpretation, "MONOCHROME2"),
		derivedio.US(derivedio.TagBitsAllocated, 32),
		derivedio.Seq(tagAcquisitionContextSeq),
		derivedio.Seq(tagDimensionOrganizationSeq, derivedio.DataSet(organization)),
		derivedio.Seq(tagDimensionIndexSeq, derivedio.DataSet(
			pointerElement(tagDimensionIndexPointer, tagImagePositionPatient),
			pointerElement(tagFunctionalGroupPointer, tagPlanePositionSeq),
			organization,
		)),
		derivedio.Seq(tagSharedFunctionalGroups, derivedio.DataSet(
			derivedio.Seq(tagPixelMeasuresSeq, derivedio.DataSet(pixelMeasures(doc)...)),
			derivedio.Seq(tagPlaneOrientationSeq, derivedio.DataSet(
				derivedio.DS(tagImageOrientationPatient, first.RowDir.X, first.RowDir.Y, first.RowDir.Z, first.ColDir.X, first.ColDir.Y, first.ColDir.Z),
			)),
			derivedio.Seq(tagRealWorldValueMappingSeq, doc.mappingItem()),
			derivedio.Seq(tagParametricMapFrameType, derivedio.DataSet(derivedio.Strings(tagFrameType, core.VRCS, imageType))),
			derivedio.Seq(tagFrameAnatomySeq, derivedio.DataSet(
				derivedio.CS(tagFrameLaterality, "U"),
				// The anatomy of a computed map is not known; Anatomic
				// Region Sequence is Type 1 with an item, so the generic
				// body region is recorded.
				derivedio.Seq(tagAnatomicRegionSeq, codeItem(Code{Value: "38266002", Scheme: "SCT", Meaning: "Entire body"})),
			)),
		)),
	}
	if description := strings.TrimSpace(doc.SeriesDescription); description != "" {
		elements = append(elements, derivedio.LO(tagSeriesDescription, description))
	}
	elements = append(elements, identification...)
	perFrame := make([]core.DataSet, len(doc.Frames))
	pixels := make([]byte, 0, len(doc.Frames)*doc.Rows*doc.Columns*4)
	var references []Reference
	seen := map[string]bool{}
	for i, frame := range doc.Frames {
		g := frame.Geometry
		items := []core.Element{
			derivedio.Seq(tagFrameContentSeq, derivedio.DataSet(
				derivedio.UL(tagDimensionIndexValues, uint32(i+1)),
			)),
			derivedio.Seq(tagPlanePositionSeq, derivedio.DataSet(
				derivedio.DS(tagImagePositionPatient, g.Origin.X, g.Origin.Y, g.Origin.Z),
			)),
		}
		if len(frame.Sources) > 0 {
			d := derivation.Derivation{Description: doc.DerivationDescription, Codes: []derivation.Code{derivation.Code(doc.derivationCode())}}
			for _, source := range frame.Sources {
				d.Sources = append(d.Sources, derivation.Source{SOPClassUID: source.SOPClassUID, SOPInstanceUID: source.SOPInstanceUID, Frames: source.Frames, Purpose: &derivation.SourceImageForProcessing})
				if !seen[source.SOPInstanceUID] {
					seen[source.SOPInstanceUID] = true
					references = append(references, source)
				}
			}
			item, err := d.Item()
			if err != nil {
				return nil, fmt.Errorf("%w: frame %d: %v", ErrInvalidObject, i+1, err)
			}
			items = append(items, derivedio.Seq(tagDerivationImageSeq, item))
		}
		perFrame[i] = derivedio.DataSet(items...)
		for _, v := range frame.Values {
			pixels = binary.LittleEndian.AppendUint32(pixels, math.Float32bits(v))
		}
	}
	elements = append(elements, derivedio.Seq(tagPerFrameFunctionalGroups, perFrame...))
	if len(references) > 0 {
		instances := make([]core.DataSet, len(references))
		for i, ref := range references {
			instances[i] = derivedio.DataSet(
				derivedio.UI(tagReferencedSOPClassUID, ref.SOPClassUID),
				derivedio.UI(tagReferencedSOPInstanceUID, ref.SOPInstanceUID),
			)
		}
		elements = append(elements, derivedio.Seq(tagReferencedSeriesSeq, derivedio.DataSet(
			derivedio.UI(derivedio.TagSeriesInstanceUID, doc.SourceSeriesInstanceUID),
			derivedio.Seq(tagReferencedInstanceSeq, instances...),
		)))
	}
	elements = append(elements, derivedio.Raw(tagFloatPixelData, core.VROF, pixels))
	dataset := derivedio.Object(elements...)
	if err := doc.Common.Apply(dataset, object.InheritAll); err != nil {
		return nil, err
	}
	return derivedio.File(ParametricMapStorage, doc.SOPInstanceUID, dataset)
}

func (doc *Document) validate() error {
	if doc == nil {
		return fmt.Errorf("%w: document is nil", ErrInvalidObject)
	}
	for name, uid := range map[string]string{"SOP Instance": doc.SOPInstanceUID, "Study Instance": doc.StudyInstanceUID,
		"Series Instance": doc.SeriesInstanceUID, "Frame of Reference": doc.FrameOfReferenceUID} {
		if strings.TrimSpace(uid) == "" {
			return fmt.Errorf("%w: %s UID is required", ErrInvalidObject, name)
		}
	}
	if doc.Rows <= 0 || doc.Columns <= 0 || doc.Rows > math.MaxUint16 || doc.Columns > math.MaxUint16 || len(doc.Frames) == 0 {
		return fmt.Errorf("%w: %d×%d pixels in %d frames", ErrInvalidObject, doc.Columns, doc.Rows, len(doc.Frames))
	}
	if voxels, ok := checkedProduct(doc.Rows, doc.Columns, len(doc.Frames)); !ok || voxels > 64*1024*1024 {
		return fmt.Errorf("%w: %d×%d×%d samples", ErrMemoryLimit, doc.Columns, doc.Rows, len(doc.Frames))
	}
	if doc.Quantity.String() == "" || doc.Units.String() == "" || doc.Quantity.Value == "" || doc.Units.Value == "" {
		return fmt.Errorf("%w: the quantity and its units are required", ErrMissingMapping)
	}
	first := doc.Frames[0].Geometry
	for i, frame := range doc.Frames {
		if len(frame.Values) != doc.Rows*doc.Columns {
			return fmt.Errorf("%w: frame %d has %d values, want %d", ErrInvalidObject, i+1, len(frame.Values), doc.Rows*doc.Columns)
		}
		g := frame.Geometry
		if !finiteVec(g.Origin) || !finiteVec(g.RowDir) || !finiteVec(g.ColDir) || !positiveFinite(g.RowSpacing) || !positiveFinite(g.ColSpacing) ||
			math.Abs(g.RowDir.Length()-1) > 1e-4 || math.Abs(g.ColDir.Length()-1) > 1e-4 || math.Abs(g.RowDir.Dot(g.ColDir)) > 1e-4 {
			return fmt.Errorf("%w: frame %d has no valid patient geometry", ErrGeometryMismatch, i+1)
		}
		if g.RowDir.Sub(first.RowDir).Length() > 1e-4 || g.ColDir.Sub(first.ColDir).Length() > 1e-4 ||
			math.Abs(g.RowSpacing-first.RowSpacing) > 1e-6 || math.Abs(g.ColSpacing-first.ColSpacing) > 1e-6 {
			return fmt.Errorf("%w: frame %d differs in orientation or spacing", ErrUnsupportedDimensions, i+1)
		}
		for _, source := range frame.Sources {
			if strings.TrimSpace(source.SOPClassUID) == "" || strings.TrimSpace(source.SOPInstanceUID) == "" {
				return fmt.Errorf("%w: frame %d has a source without SOP Class or Instance UID", ErrInvalidObject, i+1)
			}
			if strings.TrimSpace(doc.SourceSeriesInstanceUID) == "" {
				return fmt.Errorf("%w: sources need the source Series Instance UID", ErrInvalidObject)
			}
		}
	}
	return nil
}

func (doc *Document) derivationCode() Code {
	if doc.DerivationCode.Value != "" {
		return doc.DerivationCode
	}
	return doc.Quantity
}

// mappingItem is the identity Real World Value Mapping of the quantity.
func (doc *Document) mappingItem() core.DataSet {
	label := strings.TrimSpace(doc.LUTLabel)
	if label == "" {
		label = doc.Quantity.Meaning
	}
	if len(label) > 16 {
		label = label[:16]
	}
	minimum, maximum := math.Inf(1), math.Inf(-1)
	for _, frame := range doc.Frames {
		for _, v := range frame.Values {
			if f := float64(v); finite(f) {
				minimum, maximum = math.Min(minimum, f), math.Max(maximum, f)
			}
		}
	}
	if minimum > maximum {
		minimum, maximum = 0, 0
	}
	return derivedio.DataSet(
		derivedio.FD(tagDoubleRWVFirstMapped, minimum),
		derivedio.FD(tagDoubleRWVLastMapped, maximum),
		derivedio.Str(tagLUTExplanation, core.VRLO, doc.Quantity.Meaning),
		derivedio.SH(tagLUTLabel, label),
		derivedio.Seq(tagMeasurementUnitsCodeSeq, codeItem(doc.Units)),
		derivedio.Seq(tagQuantityDefinitionSeq, derivedio.DataSet(
			derivedio.CS(tagValueType, "CODE"),
			derivedio.Seq(tagConceptNameCodeSeq, codeItem(Code{Value: "246205007", Scheme: "SCT", Meaning: "Quantity"})),
			derivedio.Seq(tagConceptCodeSeq, codeItem(doc.Quantity)),
		)),
		derivedio.FD(tagRWVIntercept, 0),
		derivedio.FD(tagRWVSlope, 1),
	)
}

// pixelMeasures are the pixel spacing (row, column) and, with several
// frames, the spacing of their positions along the normal as thickness.
func pixelMeasures(doc *Document) []core.Element {
	first := doc.Frames[0].Geometry
	thickness := 1.0
	var spacing float64
	if len(doc.Frames) > 1 {
		normal := first.RowDir.Cross(first.ColDir).Normalize()
		last := doc.Frames[len(doc.Frames)-1].Geometry
		spacing = math.Abs(last.Origin.Sub(first.Origin).Dot(normal)) / float64(len(doc.Frames)-1)
		if spacing > 0 {
			thickness = spacing
		}
	}
	elements := []core.Element{
		derivedio.DS(tagPixelSpacing, first.RowSpacing, first.ColSpacing),
		derivedio.DS(tagSliceThickness, thickness),
	}
	if spacing > 0 {
		elements = append(elements, derivedio.DS(tagSpacingBetweenSlices, spacing))
	}
	return elements
}

func codeItem(code Code) core.DataSet {
	return derivedio.DataSet(
		derivedio.SH(tagCodeValue, code.Value),
		derivedio.SH(tagCodingSchemeDesignator, code.Scheme),
		derivedio.LO(tagCodeMeaning, code.Meaning),
	)
}

func pointerElement(tag core.Tag, value core.Tag) core.Element {
	var raw [4]byte
	binary.LittleEndian.PutUint16(raw[0:], value.Group)
	binary.LittleEndian.PutUint16(raw[2:], value.Element)
	return core.NewRawElement(tag, core.VRAT, raw[:])
}
