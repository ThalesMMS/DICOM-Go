package surfaceseg

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/mesh"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/seg"
)

const (
	// SurfaceSegmentationStorage is the SOP Class UID.
	SurfaceSegmentationStorage = "1.2.840.10008.5.1.4.1.1.66.5"

	// DefaultContentLabel is the Content Label Write uses when the
	// document has none.
	DefaultContentLabel = "SURFACE_SEG"

	// Recommended Presentation Type (0066,000D) enumerated values.
	PresentationSurface   = "SURFACE"
	PresentationWireframe = "WIREFRAME"
	PresentationPoints    = "POINTS"

	// Finite Volume (0066,000E) and Manifold (0066,0010) enumerated values.
	Yes     = "YES"
	No      = "NO"
	Unknown = "UNKNOWN"

	// MaxReadPoints and MaxReadTriangles bound the points and triangles of
	// all the surfaces Read decodes.
	MaxReadPoints    = 1 << 26
	MaxReadTriangles = 1 << 27
	// MaxReadPrimitives and MaxReadIndices bound all primitive and index
	// counts. MaxReadGeometryBytes bounds geometry payloads and owned arrays.
	// Write enforces the same defaults before constructing encoded arrays.
	MaxReadPrimitives    = 1 << 27
	MaxReadIndices       = 1 << 29
	MaxReadGeometryBytes = 256 << 20
)

var (
	// ErrInvalidObject reports a document or data set that is not a valid
	// Surface Segmentation.
	ErrInvalidObject = errors.New("dicom/surfaceseg: invalid object")
	// ErrUnsupportedSOPClass reports a data set of another SOP Class.
	ErrUnsupportedSOPClass = errors.New("dicom/surfaceseg: unsupported SOP class")
	// ErrResourceLimitExceeded reports geometry beyond the read/write limits.
	ErrResourceLimitExceeded = errors.New("dicom/surfaceseg: resource limit exceeded")
	// ErrNonTrianglePrimitives reports that a mesh-only export would discard
	// explicit point, edge or line geometry.
	ErrNonTrianglePrimitives = errors.New("dicom/surfaceseg: triangle-mesh export cannot preserve non-triangle primitives")
)

// Algorithm is the Algorithm Identification Macro (PS3.3 C.8.23.2).
type Algorithm struct {
	// Family is the Algorithm Family Code Sequence (Type 1, CID 7162).
	Family seg.CodedEntry
	// NameCode is the optional Algorithm Name Code Sequence.
	NameCode seg.CodedEntry
	// Name and Version are Type 1; Parameters is optional.
	Name, Version, Parameters string
}

// SurfaceReference is one item of a segment's Referenced Surface
// Sequence: a surface of the segment, the algorithm that generated it and
// the instances it was derived from.
type SurfaceReference struct {
	// Number is the Referenced Surface Number, a Surface.Number.
	Number int
	// Algorithm is the Segment Surface Generation Algorithm
	// Identification (Type 1).
	Algorithm Algorithm
	// SourceInstances is the Segment Surface Source Instance Sequence
	// (Type 2): SOPClassUID, SOPInstanceUID and, for multi-frame or
	// segmentation sources, Frames. SeriesInstanceUID is not encoded here.
	SourceInstances []seg.ReferencedImage
}

// Segment is a segment: its Segment Description and its surfaces.
type Segment struct {
	seg.Segment
	Surfaces []SurfaceReference
}

// Processing describes the processing applied to a surface (Surface
// Processing YES).
type Processing struct {
	// Ratio is the Surface Processing Ratio, for example the fraction of
	// points kept by decimation.
	Ratio       float64
	Description string
	Algorithm   Algorithm
}

// Surface is one item of the Surface Sequence.
type Surface struct {
	// Number is the Surface Number; Write numbers zero values 1 to N in
	// order and requires set ones to be.
	Number   int
	Comments string
	// Mesh holds the points (Vertices, patient mm), the optional unit
	// normals and the triangles (0-based).
	Mesh *mesh.Mesh
	// VectorAccuracy holds optional standard deviations for Mesh.Normals.
	// Supply one shared value or three XYZ values, each finite and
	// nonnegative. An empty slice omits the attribute.
	VectorAccuracy []float32
	// Primitives holds explicit vertices, edges and directed polylines.
	// Its indices reference the same points as Mesh.Triangles.
	Primitives Primitives
	// RetiredIndexLists writes 16-bit OW lists instead of the default 32-bit
	// Long lists. It requires at most 65535 points. Read preserves this choice
	// for surfaces that use only retired lists; mixed encodings normalize to Long.
	RetiredIndexLists bool
	// RecommendedDisplayGrayscale and RecommendedDisplayCIELab are the
	// display colour (Type 1); a zero CIELab takes the colour of the first
	// segment using the surface, and a zero grayscale its CIELab lightness.
	RecommendedDisplayGrayscale uint16
	RecommendedDisplayCIELab    [3]uint16
	// Opacity is the Recommended Presentation Opacity in [0, 1]; Write uses
	// 1 when it is zero and OpacitySet is false.
	Opacity    float64
	OpacitySet bool
	// PresentationType is SURFACE (the default), WIREFRAME or POINTS.
	PresentationType string
	// FiniteVolume and Manifold are YES, NO or UNKNOWN; empty values are
	// derived from the mesh (mesh.CheckClosure): a closed mesh encloses a
	// finite volume, and one without non-manifold or inconsistent edges is
	// a manifold.
	FiniteVolume, Manifold string
	// Processing is nil for an unprocessed surface (Surface Processing NO).
	Processing *Processing
	// Deprecated: explicit vertices, edges and lines are now preserved in
	// Primitives. Read leaves IgnoredPrimitives zero.
	IgnoredPrimitives int
}

// Document is a Surface Segmentation instance.
type Document struct {
	// Common fills the Patient, Study, Frame of Reference and equipment
	// modules from a source instance (object.InheritCommonModules).
	Common              object.DerivedCommon
	SOPInstanceUID      string
	StudyInstanceUID    string
	SeriesInstanceUID   string
	FrameOfReferenceUID string
	// Identification is the Series Number, Instance Number and Content
	// Identification (Content Label SURFACE_SEG by default).
	Identification seg.Identification
	// ContentDate and ContentTime default to the time of Write.
	ContentDate, ContentTime string
	Segments                 []Segment
	Surfaces                 []Surface
	// ReferencedImages is the Common Instance Reference Module's
	// Referenced Series Sequence: the instances of this study the surfaces
	// derive from.
	ReferencedImages []seg.ReferencedImage
}

// Surface returns the surface with the given number.
func (d *Document) Surface(number int) (*Surface, bool) {
	if d == nil {
		return nil, false
	}
	for i := range d.Surfaces {
		if d.Surfaces[i].Number == number {
			return &d.Surfaces[i], true
		}
	}
	return nil, false
}

var (
	tagReferencedSeriesSequence   = core.NewTag(0x0008, 0x1115)
	tagReferencedInstanceSequence = core.NewTag(0x0008, 0x114A)
	tagCodeValue                  = core.NewTag(0x0008, 0x0100)
	tagCodingSchemeDesignator     = core.NewTag(0x0008, 0x0102)
	tagCodeMeaning                = core.NewTag(0x0008, 0x0104)
	tagSegmentSequence            = core.NewTag(0x0062, 0x0002)
	tagSegmentedPropertyCategory  = core.NewTag(0x0062, 0x0003)
	tagSegmentNumber              = core.NewTag(0x0062, 0x0004)
	tagSegmentLabel               = core.NewTag(0x0062, 0x0005)
	tagSegmentAlgorithmType       = core.NewTag(0x0062, 0x0008)
	tagSegmentAlgorithmName       = core.NewTag(0x0062, 0x0009)
	tagReferencedSegmentNumber    = core.NewTag(0x0062, 0x000B)
	tagRecommendedGrayscale       = core.NewTag(0x0062, 0x000C)
	tagRecommendedCIELab          = core.NewTag(0x0062, 0x000D)
	tagSegmentedPropertyType      = core.NewTag(0x0062, 0x000F)
	tagNumberOfSurfaces           = core.NewTag(0x0066, 0x0001)
	tagSurfaceSequence            = core.NewTag(0x0066, 0x0002)
	tagSurfaceNumber              = core.NewTag(0x0066, 0x0003)
	tagSurfaceComments            = core.NewTag(0x0066, 0x0004)
	tagSurfaceProcessing          = core.NewTag(0x0066, 0x0009)
	tagSurfaceProcessingRatio     = core.NewTag(0x0066, 0x000A)
	tagSurfaceProcessingDescr     = core.NewTag(0x0066, 0x000B)
	tagPresentationOpacity        = core.NewTag(0x0066, 0x000C)
	tagPresentationType           = core.NewTag(0x0066, 0x000D)
	tagFiniteVolume               = core.NewTag(0x0066, 0x000E)
	tagManifold                   = core.NewTag(0x0066, 0x0010)
	tagSurfacePointsSequence      = core.NewTag(0x0066, 0x0011)
	tagSurfaceNormalsSequence     = core.NewTag(0x0066, 0x0012)
	tagMeshPrimitivesSequence     = core.NewTag(0x0066, 0x0013)
	tagNumberOfSurfacePoints      = core.NewTag(0x0066, 0x0015)
	tagPointCoordinatesData       = core.NewTag(0x0066, 0x0016)
	tagPointsBoundingBox          = core.NewTag(0x0066, 0x001A)
	tagNumberOfVectors            = core.NewTag(0x0066, 0x001E)
	tagVectorDimensionality       = core.NewTag(0x0066, 0x001F)
	tagVectorAccuracy             = core.NewTag(0x0066, 0x0020)
	tagVectorCoordinateData       = core.NewTag(0x0066, 0x0021)
	tagTrianglePointIndexList     = core.NewTag(0x0066, 0x0023) // retired, OW
	tagEdgePointIndexList         = core.NewTag(0x0066, 0x0024) // retired, OW
	tagVertexPointIndexList       = core.NewTag(0x0066, 0x0025) // retired, OW
	tagTriangleStripSequence      = core.NewTag(0x0066, 0x0026)
	tagTriangleFanSequence        = core.NewTag(0x0066, 0x0027)
	tagLineSequence               = core.NewTag(0x0066, 0x0028)
	tagPrimitivePointIndexList    = core.NewTag(0x0066, 0x0029) // retired, OW
	tagSurfaceCount               = core.NewTag(0x0066, 0x002A)
	tagReferencedSurfaceSequence  = core.NewTag(0x0066, 0x002B)
	tagReferencedSurfaceNumber    = core.NewTag(0x0066, 0x002C)
	tagGenerationAlgorithmSeq     = core.NewTag(0x0066, 0x002D)
	tagSourceInstanceSequence     = core.NewTag(0x0066, 0x002E)
	tagAlgorithmFamilyCodeSeq     = core.NewTag(0x0066, 0x002F)
	tagAlgorithmNameCodeSeq       = core.NewTag(0x0066, 0x0030)
	tagAlgorithmVersion           = core.NewTag(0x0066, 0x0031)
	tagAlgorithmParameters        = core.NewTag(0x0066, 0x0032)
	tagFacetSequence              = core.NewTag(0x0066, 0x0034)
	tagProcessingAlgorithmSeq     = core.NewTag(0x0066, 0x0035)
	tagAlgorithmName              = core.NewTag(0x0066, 0x0036)
	tagLongPrimitivePointIndex    = core.NewTag(0x0066, 0x0040)
	tagLongTrianglePointIndex     = core.NewTag(0x0066, 0x0041)
	tagLongEdgePointIndex         = core.NewTag(0x0066, 0x0042)
	tagLongVertexPointIndex       = core.NewTag(0x0066, 0x0043)
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidObject, fmt.Sprintf(format, args...))
}

// Validate checks the document against the requirements Write enforces.
func (d *Document) Validate() error {
	_, err := d.resolved()
	return err
}

// resolved validates the document and returns its surfaces with the
// defaults Write applies; the document is not changed.
func (d *Document) resolved() ([]Surface, error) {
	if d == nil {
		return nil, invalid("document is nil")
	}
	for name, uid := range map[string]string{
		"SOP Instance UID": d.SOPInstanceUID, "Study Instance UID": d.StudyInstanceUID,
		"Series Instance UID": d.SeriesInstanceUID, "Frame of Reference UID": d.FrameOfReferenceUID,
	} {
		if !core.IsValidUID(strings.TrimSpace(uid)) {
			return nil, invalid("%s %q is not a valid UID", name, uid)
		}
	}
	if len(d.Surfaces) == 0 || len(d.Segments) == 0 {
		return nil, invalid("%d segments and %d surfaces; at least one of each is required", len(d.Segments), len(d.Surfaces))
	}
	if err := validateWriteGeometryResources(d.Surfaces); err != nil {
		return nil, err
	}
	surfaces := make([]Surface, len(d.Surfaces))
	for i, surface := range d.Surfaces {
		if surface.Number == 0 {
			surface.Number = i + 1
		}
		if surface.Number != i+1 {
			return nil, invalid("surface %d has Surface Number %d; numbers run from 1 in order", i+1, surface.Number)
		}
		if err := surface.Mesh.Validate(); err != nil {
			return nil, invalid("surface %d: %v", surface.Number, err)
		}
		if len(surface.VectorAccuracy) > 0 && len(surface.Mesh.Normals) == 0 {
			return nil, invalid("surface %d Vector Accuracy requires normals", surface.Number)
		}
		if err := validateVectorAccuracy(surface.VectorAccuracy); err != nil {
			return nil, fmt.Errorf("surface %d: %w", surface.Number, err)
		}
		if len(surface.Mesh.Vertices) == 0 || uint64(len(surface.Mesh.Vertices)) >= math.MaxUint32 {
			return nil, invalid("surface %d has %d points", surface.Number, len(surface.Mesh.Vertices))
		}
		if err := surface.Primitives.validate(uint32(len(surface.Mesh.Vertices))); err != nil {
			return nil, invalid("surface %d: %v", surface.Number, err)
		}
		if len(surface.Mesh.Triangles) == 0 && surface.Primitives.Count() == 0 {
			return nil, invalid("surface %d has no mesh primitives", surface.Number)
		}
		if surface.RetiredIndexLists && len(surface.Mesh.Vertices) > math.MaxUint16 {
			return nil, invalid("surface %d retired indices require at most 65535 points", surface.Number)
		}
		if !surface.OpacitySet && surface.Opacity == 0 {
			surface.Opacity = 1
		}
		if !(surface.Opacity >= 0 && surface.Opacity <= 1) {
			return nil, invalid("surface %d opacity %v outside [0, 1]", surface.Number, surface.Opacity)
		}
		if surface.PresentationType == "" {
			surface.PresentationType = PresentationSurface
		}
		if !oneOf(surface.PresentationType, PresentationSurface, PresentationWireframe, PresentationPoints) {
			return nil, invalid("surface %d presentation type %q", surface.Number, surface.PresentationType)
		}
		if surface.FiniteVolume == "" || surface.Manifold == "" {
			closure := surface.Mesh.CheckClosure()
			if surface.FiniteVolume == "" {
				surface.FiniteVolume = yesNo(len(surface.Mesh.Triangles) > 0 && closure.Closed())
			}
			if surface.Manifold == "" {
				surface.Manifold = yesNo(len(surface.Mesh.Triangles) > 0 && closure.NonManifoldEdges == 0 && closure.InconsistentEdges == 0)
			}
		}
		if !oneOf(surface.FiniteVolume, Yes, No, Unknown) || !oneOf(surface.Manifold, Yes, No, Unknown) {
			return nil, invalid("surface %d Finite Volume %q, Manifold %q", surface.Number, surface.FiniteVolume, surface.Manifold)
		}
		if p := surface.Processing; p != nil {
			if !(p.Ratio >= 0) || math.IsInf(p.Ratio, 0) {
				return nil, invalid("surface %d processing ratio %v", surface.Number, p.Ratio)
			}
			if err := p.Algorithm.validate(); err != nil {
				return nil, invalid("surface %d processing: %v", surface.Number, err)
			}
		}
		surfaces[i] = surface
	}
	numbers := map[int]bool{}
	for _, segment := range d.Segments {
		if segment.Number < 1 || segment.Number > math.MaxUint16 || numbers[segment.Number] {
			return nil, invalid("segment number %d is not unique in 1..65535", segment.Number)
		}
		numbers[segment.Number] = true
		if strings.TrimSpace(segment.Label) == "" {
			return nil, invalid("segment %d has no label", segment.Number)
		}
		if !oneOf(segment.AlgorithmType, seg.AlgorithmManual, seg.AlgorithmAutomatic, seg.AlgorithmSemiautomatic) {
			return nil, invalid("segment %d algorithm type %q", segment.Number, segment.AlgorithmType)
		}
		if err := segment.ValidateAlgorithm(); err != nil {
			return nil, invalid("%v", err)
		}
		if !completeCode(segment.SegmentedPropertyCategory) || !completeCode(segment.SegmentedPropertyType) {
			return nil, invalid("segment %d needs complete Segmented Property Category and Type codes", segment.Number)
		}
		if len(segment.Surfaces) == 0 {
			return nil, invalid("segment %d references no surface", segment.Number)
		}
		seen := map[int]bool{}
		for _, ref := range segment.Surfaces {
			if ref.Number < 1 || ref.Number > len(surfaces) || seen[ref.Number] {
				return nil, invalid("segment %d references surface %d (there are %d, each referenced once per segment)", segment.Number, ref.Number, len(surfaces))
			}
			seen[ref.Number] = true
			if err := ref.Algorithm.validate(); err != nil {
				return nil, invalid("segment %d surface %d generation: %v", segment.Number, ref.Number, err)
			}
			for _, source := range ref.SourceInstances {
				if !core.IsValidUID(strings.TrimSpace(source.SOPClassUID)) || !core.IsValidUID(strings.TrimSpace(source.SOPInstanceUID)) {
					return nil, invalid("segment %d surface %d source instance %q of class %q needs valid UIDs", segment.Number, ref.Number, source.SOPInstanceUID, source.SOPClassUID)
				}
			}
			surface := &surfaces[ref.Number-1]
			if surface.RecommendedDisplayCIELab == ([3]uint16{}) {
				surface.RecommendedDisplayCIELab = segment.RecommendedDisplayCIELab
			}
		}
	}
	for i := range surfaces {
		surface := &surfaces[i]
		if surface.RecommendedDisplayCIELab == ([3]uint16{}) {
			// Mid-grey when neither the surface nor a segment has a colour.
			surface.RecommendedDisplayCIELab = seg.RGBToDICOMCIELab([3]int{128, 128, 128})
		}
		if surface.RecommendedDisplayGrayscale == 0 {
			surface.RecommendedDisplayGrayscale = surface.RecommendedDisplayCIELab[0]
		}
	}
	for _, ref := range d.ReferencedImages {
		for _, uid := range []string{ref.SeriesInstanceUID, ref.SOPClassUID, ref.SOPInstanceUID} {
			if !core.IsValidUID(strings.TrimSpace(uid)) {
				return nil, invalid("referenced image %q: %q is not a valid UID", ref.SOPInstanceUID, uid)
			}
		}
	}
	return surfaces, nil
}

func (a Algorithm) validate() error {
	if !completeCode(a.Family) {
		return errors.New("Algorithm Family Code Sequence is incomplete")
	}
	if a.NameCode != (seg.CodedEntry{}) && !completeCode(a.NameCode) {
		return errors.New("Algorithm Name Code Sequence is incomplete")
	}
	if strings.TrimSpace(a.Name) == "" || strings.TrimSpace(a.Version) == "" {
		return errors.New("Algorithm Name and Algorithm Version are required")
	}
	return nil
}

func completeCode(c seg.CodedEntry) bool {
	return strings.TrimSpace(c.CodeValue) != "" && strings.TrimSpace(c.CodingSchemeDesignator) != "" && strings.TrimSpace(c.CodeMeaning) != ""
}

func validateVectorAccuracy(values []float32) error {
	if len(values) != 0 && len(values) != 1 && len(values) != 3 {
		return invalid("Vector Accuracy has %d values; expected one shared value or three XYZ values", len(values))
	}
	for _, value := range values {
		if !(value >= 0) || math.IsInf(float64(value), 1) {
			return invalid("Vector Accuracy values must be finite and nonnegative")
		}
	}
	return nil
}

func oneOf(value string, options ...string) bool {
	for _, option := range options {
		if value == option {
			return true
		}
	}
	return false
}

func yesNo(b bool) string {
	if b {
		return Yes
	}
	return No
}

// Write validates and encodes the document as a Surface Segmentation
// Storage file in Explicit VR Little Endian.
func Write(doc *Document) (*object.File, error) {
	surfaces, err := doc.resolved()
	if err != nil {
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
		derivedio.UI(derivedio.TagSOPClassUID, SurfaceSegmentationStorage),
		derivedio.UI(derivedio.TagSOPInstanceUID, doc.SOPInstanceUID),
		derivedio.CS(derivedio.TagModality, "SEG"),
		derivedio.Str(derivedio.TagContentDate, core.VRDA, contentDate),
		derivedio.Str(derivedio.TagContentTime, core.VRTM, contentTime),
		derivedio.UI(derivedio.TagStudyInstanceUID, doc.StudyInstanceUID),
		derivedio.UI(derivedio.TagSeriesInstanceUID, doc.SeriesInstanceUID),
		derivedio.UI(derivedio.TagFrameOfReferenceUID, doc.FrameOfReferenceUID),
		segmentSequence(doc.Segments),
		derivedio.UL(tagNumberOfSurfaces, uint32(len(surfaces))),
		surfaceSequence(surfaces),
	}
	elements = append(elements, identification...)
	if len(doc.ReferencedImages) > 0 {
		elements = append(elements, referencedSeriesSequence(doc.ReferencedImages))
	}
	dataset := derivedio.Object(elements...)
	if err := doc.Common.Apply(dataset, object.InheritAll); err != nil {
		return nil, err
	}
	return derivedio.File(SurfaceSegmentationStorage, doc.SOPInstanceUID, dataset)
}

func codeSequence(tag core.Tag, code seg.CodedEntry) core.Element {
	return derivedio.Seq(tag, derivedio.DataSet(
		derivedio.SH(tagCodeValue, code.CodeValue),
		derivedio.SH(tagCodingSchemeDesignator, code.CodingSchemeDesignator),
		derivedio.LO(tagCodeMeaning, code.CodeMeaning),
	))
}

func algorithmItem(a Algorithm) core.DataSet {
	elements := []core.Element{
		codeSequence(tagAlgorithmFamilyCodeSeq, a.Family),
		derivedio.LO(tagAlgorithmName, a.Name),
		derivedio.LO(tagAlgorithmVersion, a.Version),
	}
	if a.NameCode != (seg.CodedEntry{}) {
		elements = append(elements, codeSequence(tagAlgorithmNameCodeSeq, a.NameCode))
	}
	if a.Parameters != "" {
		elements = append(elements, derivedio.Str(tagAlgorithmParameters, core.VRLT, a.Parameters))
	}
	return derivedio.DataSet(elements...)
}

func segmentSequence(segments []Segment) core.Element {
	items := make([]core.DataSet, len(segments))
	for i, segment := range segments {
		elements := []core.Element{
			derivedio.US(tagSegmentNumber, uint16(segment.Number)),
			derivedio.LO(tagSegmentLabel, segment.Label),
			derivedio.CS(tagSegmentAlgorithmType, segment.AlgorithmType),
			codeSequence(tagSegmentedPropertyCategory, segment.SegmentedPropertyCategory),
			codeSequence(tagSegmentedPropertyType, segment.SegmentedPropertyType),
			derivedio.UL(tagSurfaceCount, uint32(len(segment.Surfaces))),
		}
		if segment.AlgorithmName != "" {
			elements = append(elements, derivedio.LO(tagSegmentAlgorithmName, segment.AlgorithmName))
		}
		if segment.RecommendedDisplayCIELab != ([3]uint16{}) {
			lab := segment.RecommendedDisplayCIELab
			elements = append(elements, derivedio.US(tagRecommendedCIELab, lab[0], lab[1], lab[2]))
		}
		refs := make([]core.DataSet, len(segment.Surfaces))
		for j, ref := range segment.Surfaces {
			sources := make([]core.DataSet, len(ref.SourceInstances))
			for k, source := range ref.SourceInstances {
				sources[k] = instanceReference(source)
			}
			refs[j] = derivedio.DataSet(
				derivedio.UL(tagReferencedSurfaceNumber, uint32(ref.Number)),
				derivedio.Seq(tagGenerationAlgorithmSeq, algorithmItem(ref.Algorithm)),
				derivedio.Seq(tagSourceInstanceSequence, sources...),
			)
		}
		elements = append(elements, derivedio.Seq(tagReferencedSurfaceSequence, refs...))
		items[i] = derivedio.DataSet(elements...)
	}
	return derivedio.Seq(tagSegmentSequence, items...)
}

// instanceReference is an Image SOP Instance Reference Macro item.
func instanceReference(ref seg.ReferencedImage) core.DataSet {
	elements := []core.Element{
		derivedio.UI(derivedio.TagRefSOPClassUID, ref.SOPClassUID),
		derivedio.UI(derivedio.TagRefSOPInstanceUID, ref.SOPInstanceUID),
	}
	if len(ref.Frames) > 0 {
		elements = append(elements, derivedio.IS(derivedio.TagRefFrameNumber, ref.Frames...))
	}
	return derivedio.DataSet(elements...)
}

func referencedSeriesSequence(refs []seg.ReferencedImage) core.Element {
	var order []string
	bySeries := map[string][]core.DataSet{}
	for _, ref := range refs {
		if _, ok := bySeries[ref.SeriesInstanceUID]; !ok {
			order = append(order, ref.SeriesInstanceUID)
		}
		bySeries[ref.SeriesInstanceUID] = append(bySeries[ref.SeriesInstanceUID], instanceReference(ref))
	}
	items := make([]core.DataSet, len(order))
	for i, series := range order {
		items[i] = derivedio.DataSet(
			derivedio.UI(derivedio.TagSeriesInstanceUID, series),
			derivedio.Seq(tagReferencedInstanceSequence, bySeries[series]...),
		)
	}
	return derivedio.Seq(tagReferencedSeriesSequence, items...)
}

func surfaceSequence(surfaces []Surface) core.Element {
	items := make([]core.DataSet, len(surfaces))
	for i, s := range surfaces {
		m := s.Mesh
		lab := s.RecommendedDisplayCIELab
		elements := []core.Element{
			derivedio.UL(tagSurfaceNumber, uint32(s.Number)),
			derivedio.US(tagRecommendedGrayscale, s.RecommendedDisplayGrayscale),
			derivedio.US(tagRecommendedCIELab, lab[0], lab[1], lab[2]),
			derivedio.FL(tagPresentationOpacity, s.Opacity),
			derivedio.CS(tagPresentationType, s.PresentationType),
			derivedio.CS(tagFiniteVolume, s.FiniteVolume),
			derivedio.CS(tagManifold, s.Manifold),
			derivedio.Seq(tagSurfacePointsSequence, pointsItem(m.Vertices)),
			normalsSequence(m, s.VectorAccuracy),
			primitiveSequence(s),
		}
		if s.Comments != "" {
			elements = append(elements, derivedio.Str(tagSurfaceComments, core.VRLT, s.Comments))
		}
		if p := s.Processing; p != nil {
			elements = append(elements,
				derivedio.CS(tagSurfaceProcessing, Yes),
				derivedio.FL(tagSurfaceProcessingRatio, p.Ratio),
				derivedio.Seq(tagProcessingAlgorithmSeq, algorithmItem(p.Algorithm)),
			)
			if p.Description != "" {
				elements = append(elements, derivedio.LO(tagSurfaceProcessingDescr, p.Description))
			}
		} else {
			elements = append(elements, derivedio.CS(tagSurfaceProcessing, No))
		}
		items[i] = derivedio.DataSet(elements...)
	}
	return derivedio.Seq(tagSurfaceSequence, items...)
}

// pointsItem is the Points Macro of the vertices, with their bounding box.
func pointsItem(vertices [][3]float32) core.DataSet {
	data := make([]byte, 12*len(vertices))
	lo := [3]float32{float32(math.Inf(1)), float32(math.Inf(1)), float32(math.Inf(1))}
	hi := [3]float32{float32(math.Inf(-1)), float32(math.Inf(-1)), float32(math.Inf(-1))}
	for i, v := range vertices {
		for k := 0; k < 3; k++ {
			binary.LittleEndian.PutUint32(data[12*i+4*k:], math.Float32bits(v[k]))
			lo[k], hi[k] = min(lo[k], v[k]), max(hi[k], v[k])
		}
	}
	return derivedio.DataSet(
		derivedio.UL(tagNumberOfSurfacePoints, uint32(len(vertices))),
		derivedio.Raw(tagPointCoordinatesData, core.VROF, data),
		derivedio.FL(tagPointsBoundingBox, float64(lo[0]), float64(lo[1]), float64(lo[2]), float64(hi[0]), float64(hi[1]), float64(hi[2])),
	)
}

// normalsSequence is the Surface Points Normals Sequence: one Vectors
// Macro item with the normals, or empty (Type 2) without them.
func normalsSequence(m *mesh.Mesh, accuracy []float32) core.Element {
	if len(m.Normals) == 0 {
		return derivedio.Seq(tagSurfaceNormalsSequence)
	}
	data := make([]byte, 12*len(m.Normals))
	for i, n := range m.Normals {
		for k := 0; k < 3; k++ {
			binary.LittleEndian.PutUint32(data[12*i+4*k:], math.Float32bits(n[k]))
		}
	}
	elements := []core.Element{
		derivedio.UL(tagNumberOfVectors, uint32(len(m.Normals))),
		derivedio.US(tagVectorDimensionality, 3),
		derivedio.Raw(tagVectorCoordinateData, core.VROF, data),
	}
	if len(accuracy) > 0 {
		elements = append(elements, object.FLElement(tagVectorAccuracy, binary.LittleEndian, accuracy...))
	}
	return derivedio.Seq(tagSurfaceNormalsSequence, derivedio.DataSet(elements...))
}

// triangleIndexBytes encodes the triangles as 1-based OL indices.
func triangleIndexBytes(triangles [][3]uint32) []byte {
	data := make([]byte, 12*len(triangles))
	for i, t := range triangles {
		for k := 0; k < 3; k++ {
			binary.LittleEndian.PutUint32(data[12*i+4*k:], t[k]+1)
		}
	}
	return data
}
