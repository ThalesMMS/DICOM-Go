package sr

import (
	"errors"
	"fmt"
	"github.com/ThalesMMS/dicom-go/codes/dcm"
	"github.com/ThalesMMS/dicom-go/codes/sct"
	"github.com/ThalesMMS/dicom-go/codes/umls"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

const (
	EnhancedSRStorage        = "1.2.840.10008.5.1.4.1.1.88.22"
	ComprehensiveSRStorage   = "1.2.840.10008.5.1.4.1.1.88.33"
	Comprehensive3DSRStorage = "1.2.840.10008.5.1.4.1.1.88.34"

	// SCOORD3D Graphic Types (PS3.3 C.18.9.1.2). POINT has exactly one
	// point; POLYGON repeats its first point at the end.
	GraphicTypePoint3D      = "POINT"
	GraphicTypeMultipoint3D = "MULTIPOINT"
	GraphicTypePolyline3D   = "POLYLINE"
	GraphicTypePolygon3D    = "POLYGON"

	// DefaultObserverDeviceUID identifies dicom-go as the device observer of
	// reports that name no observer: a UUID-derived UID (name-based UUID of
	// https://github.com/ThalesMMS/dicom-go/sr).
	DefaultObserverDeviceUID = "2.25.5904973083307394922725280422832941272"

	// Template identifiers of the measurement report (DCMR).
	TemplateMeasurementReport      = "1500"
	TemplateMeasurementGroup       = "1501"
	TemplateVolumetricMeasurements = "1411"
)

var (
	ErrInvalidMeasurementReport = errors.New("dicom/sr: invalid measurement report")
	ErrUnsupportedSRStorage     = errors.New("dicom/sr: unsupported SR storage")
)

var (
	tagReferencedSegmentNumber = core.NewTag(0x0062, 0x000B)
)

// Concepts of TID 1500 and the templates it includes (PS3.16).
var (
	imagingMeasurementReportConcept   = FromCode(dcm.ImagingMeasurementReport)
	languageConcept                   = FromCode(dcm.LanguageOfContentItemAndDescendants)
	observerTypeConcept               = FromCode(dcm.ObserverType)
	personObserverType                = FromCode(dcm.Person)
	deviceObserverType                = FromCode(dcm.Device)
	personObserverNameConcept         = FromCode(dcm.PersonObserverName)
	personObserverOrganizationConcept = FromCode(dcm.PersonObserversOrganizationName)
	deviceObserverUIDConcept          = FromCode(dcm.DeviceObserverUID)
	deviceObserverNameConcept         = FromCode(dcm.DeviceObserverName)
	deviceObserverManufacturerConcept = FromCode(dcm.DeviceObserverManufacturer)
	deviceObserverModelConcept        = FromCode(dcm.DeviceObserverModelName)
	deviceObserverSerialConcept       = FromCode(dcm.DeviceObserverSerialNumber)
	procedureReportedConcept          = FromCode(dcm.ProcedureReported)
	imageLibraryConcept               = FromCode(dcm.ImageLibrary)
	imageLibraryGroupConcept          = FromCode(dcm.ImageLibraryGroup)
	imagingMeasurementsConcept        = FromCode(dcm.ImagingMeasurements)
	derivedImagingMeasurementsConcept = FromCode(dcm.DerivedImagingMeasurements)
	qualitativeEvaluationsConcept     = FromCode(umls.QualitativeEvaluations)
	measurementGroupConcept           = FromCode(dcm.MeasurementGroup)
	trackingIdentifierConcept         = FromCode(dcm.TrackingIdentifier)
	trackingUniqueIdentifierConcept   = FromCode(dcm.TrackingUniqueIdentifier)
	findingConcept                    = FromCode(dcm.Finding)
	findingSiteConcept                = FromCode(sct.FindingSite)
	referencedSegmentConcept          = FromCode(dcm.ReferencedSegment)
	sourceSeriesConcept               = FromCode(dcm.SourceSeriesForSegmentation)
	sourceOfMeasurementConcept        = FromCode(dcm.SourceOfMeasurement)
	derivationConcept                 = FromCode(dcm.Derivation)

	// DefaultLanguage is the language of reports that set none (TID 1204).
	DefaultLanguage = CodedEntry{CodeValue: "en-US", CodingSchemeDesignator: "RFC5646", CodeMeaning: "English (US)"}
	// DefaultProcedureReported is the Procedure reported of reports that set
	// none (CID 100).
	DefaultProcedureReported = FromCode(sct.ImagingProcedure)
)

type TrackingIdentifier struct {
	UID        string
	Identifier string
}

// SegmentReference references a segment of a Segmentation. With the Study
// and Series Instance UIDs it is also listed in the evidence of the report.
type SegmentReference struct {
	StudyInstanceUID  string
	SeriesInstanceUID string
	SOPClassUID       string
	SOPInstanceUID    string
	SegmentNumber     int
}

type Point3D struct {
	X float64
	Y float64
	Z float64
}

type SpatialReference struct {
	FrameOfReferenceUID string
	GraphicType         string
	Coordinates         []Point3D
	FiducialUID         string
}

// ReportMeasurement is a NUM item of a measurement group. Image and Spatial
// are written INFERRED FROM with the purpose "Source of Measurement"
// (TID 320).
type ReportMeasurement struct {
	ConceptName CodedEntry
	Value       float64
	Units       CodedEntry
	// Derivation (TID 300 row 3, CID 7464), such as Mean or Maximum, tells
	// apart measurements of the same concept in a group.
	Derivation CodedEntry
	Image      ImageReference
	Spatial    SpatialReference
}

// MeasurementGroup is a TID 1501 Measurement Group, or a TID 1411
// Volumetric ROI Measurements group when it references a segment.
type MeasurementGroup struct {
	// Template is the DCMR template of the group ("1501" or "1411"); empty
	// selects 1411 when ReferencedSegment is set and 1501 otherwise. Reading
	// sets it from the group's template identification.
	Template          string
	Tracking          TrackingIdentifier
	Finding           CodedEntry
	FindingSites      []CodedEntry
	ReferencedSegment SegmentReference
	// SourceSeriesUID is the Source series for segmentation of a TID 1411
	// group (required there).
	SourceSeriesUID string
	Measurements    []ReportMeasurement
}

// Observer is one observer of the Observation Context (TID 1002): a person
// when PersonName is set, otherwise a device identified by DeviceUID.
type Observer struct {
	PersonName         string
	OrganizationName   string
	DeviceUID          string
	DeviceName         string
	DeviceManufacturer string
	DeviceModelName    string
	DeviceSerialNumber string
}

// DefaultObserver is the device observer of reports that name none.
func DefaultObserver() Observer {
	return Observer{DeviceUID: DefaultObserverDeviceUID, DeviceName: "dicom-go"}
}

// MeasurementReport is a TID 1500 Imaging Measurement Report.
type MeasurementReport struct {
	SOPClassUID         string
	SOPInstanceUID      string
	StudyInstanceUID    string
	SeriesInstanceUID   string
	FrameOfReferenceUID string
	ContentDate         string
	ContentTime         string
	SeriesNumber        string
	InstanceNumber      string
	CompletionFlag      string
	VerificationFlag    string
	// CompletionFlagDescription, VerifyingObservers and PredecessorDocuments
	// are the document lifecycle of Document (Complete, Verify, Revise).
	CompletionFlagDescription string
	VerifyingObservers        []VerifyingObserver
	PredecessorDocuments      []DocumentReference
	ContinuityOfContent       string
	Title                     CodedEntry
	// Language of the content (TID 1204); empty selects DefaultLanguage.
	Language CodedEntry
	// Observers of the Observation Context (TID 1001); empty selects
	// DefaultObserver.
	Observers []Observer
	// ProceduresReported (121058); empty selects DefaultProcedureReported.
	ProceduresReported []CodedEntry
	// ImageLibrary lists the images of the Image Library (TID 1600); nil
	// lists the distinct images the measurements reference.
	ImageLibrary []ImageReference
	// ImageLibraryGroups and ImageLibraryDescriptors are the additive structured
	// representation. Authoring both fields is ambiguous. Rich reads also retain
	// a checked, detached legacy projection for existing consumers.
	ImageLibraryGroups         []ImageLibraryGroup
	ImageLibraryOrder          []int
	ImageLibraryDescriptors    []ContentItem
	ImageLibraryMetadata       *ImageLibraryMetadataBinding
	imageLibraryReadProjection bool
	Groups                     []MeasurementGroup
	// Legacy reports a report read in the layout written before TID 1500
	// conformance, with its measurement groups directly under the root.
	Legacy bool
	// Common fills the Patient, Study and equipment modules from a source
	// instance of the same patient (object.InheritCommonModules).
	Common object.DerivedCommon
}

// WriteMeasurementReport encodes a measurement report as a DICOM file with the
// content tree of TID 1500: the language (TID 1204), the observation context
// (TID 1001), the procedures reported, the image library (TID 1600) and the
// measurement groups (TID 1501 or 1411) in Imaging Measurements.
// It defaults report.SOPClassUID to Comprehensive3DSRStorage if empty and report.Title to a standard "Imaging Measurement Report" coded entry if empty.
// It returns ErrInvalidMeasurementReport if report is nil or its content tree
// violates the relationship content constraints of the SOP Class (PS3.3
// Tables A.35.x-2), or ErrUnsupportedSRStorage if the SOP Class UID is not
// one of the supported SR storage types.
func WriteMeasurementReport(report *MeasurementReport) (*object.File, error) {
	if report == nil {
		return nil, fmt.Errorf("%w: report is nil", ErrInvalidMeasurementReport)
	}
	sopClassUID := report.SOPClassUID
	if sopClassUID == "" {
		sopClassUID = Comprehensive3DSRStorage
	}
	if !measurementSOPClass(sopClassUID) {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedSRStorage, sopClassUID)
	}
	completion, verification, err := validateDocumentState(report.CompletionFlag, report.VerificationFlag, len(report.VerifyingObservers))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidMeasurementReport, err)
	}
	title := report.Title
	if title.Empty() {
		title = imagingMeasurementReportConcept
	}
	content, err := measurementReportContent(report)
	if err != nil {
		return nil, err
	}
	evidence, err := evidenceReferences(measurementEvidence(report), report.Common.Source)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidMeasurementReport, err)
	}
	elements := []core.Element{
		derivedio.UI(tagSOPClassUID, sopClassUID),
		derivedio.UI(tagSOPInstanceUID, report.SOPInstanceUID),
		derivedio.CS(tagModality, "SR"),
		derivedio.Str(tagSeriesNumber, core.VRIS, defaultString(report.SeriesNumber, "1")),
		derivedio.Str(tagInstanceNumber, core.VRIS, defaultString(report.InstanceNumber, "1")),
		derivedio.CS(tagValueType, string(ValueContainer)),
		derivedio.CS(tagContinuityOfContent, defaultString(report.ContinuityOfContent, "SEPARATE")),
		derivedio.CS(tagCompletionFlag, completion),
		derivedio.CS(tagVerificationFlag, verification),
		codeSequence(tagConceptNameCodeSeq, title),
		templateSequence(TemplateMeasurementReport),
		derivedio.Seq(tagReferencedPerformedProcedureStep),
		derivedio.Seq(tagPerformedProcedureCodeSequence),
	}
	if report.StudyInstanceUID != "" {
		elements = append(elements, derivedio.UI(derivedio.TagStudyInstanceUID, report.StudyInstanceUID))
	}
	elements = append(elements, derivedio.UI(derivedio.TagSeriesInstanceUID, documentSeriesUID(sopClassUID, report.SOPInstanceUID, report.SeriesInstanceUID)))
	if report.FrameOfReferenceUID != "" {
		elements = append(elements, derivedio.UI(derivedio.TagFrameOfReferenceUID, report.FrameOfReferenceUID))
	}
	contentDate, contentTime := report.ContentDate, report.ContentTime
	if contentDate == "" || contentTime == "" {
		currentDate, currentTime := currentContentDateTime()
		if contentDate == "" {
			contentDate = currentDate
		}
		if contentTime == "" {
			contentTime = currentTime
		}
	}
	elements = append(elements,
		derivedio.Str(tagContentDate, core.VRDA, contentDate),
		derivedio.Str(tagContentTime, core.VRTM, contentTime),
		derivedio.Seq(tagContentSequence, content...),
	)
	lifecycle, err := lifecycleElements(report.CompletionFlagDescription, report.VerifyingObservers, report.PredecessorDocuments)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidMeasurementReport, err)
	}
	elements = append(elements, lifecycle...)
	if len(evidence) > 0 {
		elements = append(elements, currentRequestedProcedureEvidenceSequence(evidence))
	}
	dataset := derivedio.Object(elements...)
	if err := validateEncodedRelationships(dataset); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidMeasurementReport, err)
	}
	if err := report.Common.Apply(dataset, object.InheritWithoutFrameOfReference); err != nil {
		return nil, err
	}
	return derivedio.File(sopClassUID, report.SOPInstanceUID, dataset)
}

// measurementReportContent returns the children of the TID 1500 root.
func measurementReportContent(report *MeasurementReport) ([]core.DataSet, error) {
	language := report.Language
	if language.Empty() {
		language = DefaultLanguage
	}
	content := []core.DataSet{codeItem(RelationshipHasConceptMod, languageConcept, language)}
	observers := report.Observers
	if len(observers) == 0 {
		observers = []Observer{DefaultObserver()}
	}
	for i, observer := range observers {
		items, err := observerItems(observer)
		if err != nil {
			return nil, fmt.Errorf("%w: observer %d: %w", ErrInvalidMeasurementReport, i, err)
		}
		content = append(content, items...)
	}
	procedures := report.ProceduresReported
	if len(procedures) == 0 {
		procedures = []CodedEntry{DefaultProcedureReported}
	}
	for _, procedure := range procedures {
		content = append(content, codeItem(RelationshipHasConceptMod, procedureReportedConcept, procedure))
	}
	structured, err := imageLibraryDataSets(report)
	if err != nil {
		return nil, err
	}
	if report.ImageLibraryGroups != nil || report.ImageLibraryDescriptors != nil {
		container, err := structuredLibraryContainer(report, structured)
		if err != nil {
			return nil, err
		}
		content = append(content, container)
	}
	library := report.ImageLibrary
	if report.ImageLibraryGroups != nil || report.ImageLibraryDescriptors != nil {
		library = nil // The checked read projection is not a second encoded library.
	}
	if library == nil && report.ImageLibraryGroups == nil && report.ImageLibraryDescriptors == nil {
		library = measuredImages(report.Groups)
	}
	if len(library) > 0 {
		entries := make([]core.DataSet, 0, len(library))
		for _, image := range library {
			entries = append(entries, derivedio.DataSet(
				derivedio.CS(tagRelationshipType, RelationshipContains),
				derivedio.CS(tagValueType, string(ValueImage)),
				referencedSOPSequence(image),
			))
		}
		content = append(content, containerItem(RelationshipContains, imageLibraryConcept, "",
			containerItem(RelationshipContains, imageLibraryGroupConcept, "", entries...)))
	}
	groups := make([]core.DataSet, 0, len(report.Groups))
	for i, group := range report.Groups {
		if ref := group.ReferencedSegment; hasSegmentReference(ref) && (ref.SegmentNumber < 1 || ref.SegmentNumber > 65535) {
			return nil, fmt.Errorf("%w: group %d: referenced segment number %d must be in 1..65535", ErrInvalidMeasurementReport, i, ref.SegmentNumber)
		}
		groups = append(groups, measurementGroupDataSet(group))
	}
	return append(content, containerItem(RelationshipContains, imagingMeasurementsConcept, "", groups...)), nil
}

// observerItems returns the Observation Context items of one observer (TID
// 1002 with TID 1003 or 1004).
func observerItems(observer Observer) ([]core.DataSet, error) {
	if observer.PersonName != "" {
		items := []core.DataSet{
			codeItem(RelationshipHasObsContext, observerTypeConcept, personObserverType),
			derivedio.DataSet(
				derivedio.CS(tagRelationshipType, RelationshipHasObsContext),
				derivedio.CS(tagValueType, string(ValuePName)),
				codeSequence(tagConceptNameCodeSeq, personObserverNameConcept),
				derivedio.Str(tagPersonNameValue, core.VRPN, observer.PersonName),
			),
		}
		if observer.OrganizationName != "" {
			items = append(items, textItem(RelationshipHasObsContext, personObserverOrganizationConcept, observer.OrganizationName))
		}
		return items, nil
	}
	if observer.DeviceUID == "" {
		return nil, errors.New("an observer needs a person name or a device UID")
	}
	items := []core.DataSet{
		codeItem(RelationshipHasObsContext, observerTypeConcept, deviceObserverType),
		uidItem(RelationshipHasObsContext, deviceObserverUIDConcept, observer.DeviceUID),
	}
	for _, field := range []struct {
		concept CodedEntry
		value   string
	}{
		{deviceObserverNameConcept, observer.DeviceName},
		{deviceObserverManufacturerConcept, observer.DeviceManufacturer},
		{deviceObserverModelConcept, observer.DeviceModelName},
		{deviceObserverSerialConcept, observer.DeviceSerialNumber},
	} {
		if field.value != "" {
			items = append(items, textItem(RelationshipHasObsContext, field.concept, field.value))
		}
	}
	return items, nil
}

// measurementEvidence returns the instances the report references (image
// library, measurement images and segmentations) once each, for the Current
// Requested Procedure Evidence Sequence; a duplicate completes the Study and
// Series Instance UIDs of the first reference to its instance.
func measurementEvidence(report *MeasurementReport) []ImageReference {
	var refs []ImageReference
	index := map[string]int{}
	add := func(ref ImageReference) {
		ref = ImageReference{StudyInstanceUID: ref.StudyInstanceUID, SeriesInstanceUID: ref.SeriesInstanceUID,
			SOPClassUID: ref.SOPClassUID, SOPInstanceUID: ref.SOPInstanceUID}
		i, seen := index[clean(ref.SOPInstanceUID)]
		if !seen {
			index[clean(ref.SOPInstanceUID)] = len(refs)
			refs = append(refs, ref)
			return
		}
		if clean(refs[i].StudyInstanceUID) == "" {
			refs[i].StudyInstanceUID = ref.StudyInstanceUID
		}
		if clean(refs[i].SeriesInstanceUID) == "" {
			refs[i].SeriesInstanceUID = ref.SeriesInstanceUID
		}
	}
	for _, ref := range report.ImageLibrary {
		add(ref)
	}
	for _, g := range report.ImageLibraryGroups {
		for _, entry := range g.Entries {
			add(entry.Image)
		}
	}
	for _, group := range report.Groups {
		if segment := group.ReferencedSegment; hasSegmentReference(segment) {
			add(ImageReference{StudyInstanceUID: segment.StudyInstanceUID, SeriesInstanceUID: segment.SeriesInstanceUID,
				SOPClassUID: segment.SOPClassUID, SOPInstanceUID: segment.SOPInstanceUID})
		}
		for _, measurement := range group.Measurements {
			if hasImageReference(measurement.Image) {
				add(measurement.Image)
			}
		}
	}
	return refs
}

// measuredImages returns the distinct images the measurements reference.
func measuredImages(groups []MeasurementGroup) []ImageReference {
	var out []ImageReference
	seen := map[string]bool{}
	for _, group := range groups {
		for _, measurement := range group.Measurements {
			ref := measurement.Image
			if ref.SOPInstanceUID == "" || seen[ref.SOPInstanceUID] {
				continue
			}
			seen[ref.SOPInstanceUID] = true
			out = append(out, ImageReference{StudyInstanceUID: ref.StudyInstanceUID, SeriesInstanceUID: ref.SeriesInstanceUID,
				SOPClassUID: ref.SOPClassUID, SOPInstanceUID: ref.SOPInstanceUID})
		}
	}
	return out
}

// validateEncodedRelationships reads back an encoded SR content tree and
// checks it against the relationship content constraints of its IOD.
func validateEncodedRelationships(dataset *object.Object) error {
	document, err := ReadDocument(dataset)
	if err != nil {
		return err
	}
	if _, err := ValidateRelationships(document, DefaultRelationshipOptions()); err != nil && !errors.Is(err, ErrUnconstrainedSOPClass) {
		return err
	}
	return nil
}

// ReadMeasurementReport deserializes a DICOM measurement report from an object
// dataset: a TID 1500 report, with its measurement groups in Imaging
// Measurements, or the earlier layout with the groups directly under the root
// (MeasurementReport.Legacy).
// It returns ErrNoDataset if obj is nil, or ErrUnsupportedSRStorage if the dataset's SOP Class UID is not supported.
func ReadMeasurementReport(obj *object.Object) (*MeasurementReport, error) {
	if obj == nil {
		return nil, ErrNoDataset
	}
	dec := &decoder{}
	sopClassUID := dec.string(obj, tagSOPClassUID)
	if dec.err != nil {
		return nil, dec.err
	}
	if !measurementSOPClass(sopClassUID) {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedSRStorage, sopClassUID)
	}
	report := &MeasurementReport{
		SOPClassUID:         sopClassUID,
		SOPInstanceUID:      dec.string(obj, tagSOPInstanceUID),
		StudyInstanceUID:    dec.string(obj, derivedio.TagStudyInstanceUID),
		SeriesInstanceUID:   dec.string(obj, derivedio.TagSeriesInstanceUID),
		FrameOfReferenceUID: dec.string(obj, derivedio.TagFrameOfReferenceUID),
		ContentDate:         dec.string(obj, tagContentDate),
		ContentTime:         dec.string(obj, tagContentTime),
		SeriesNumber:        dec.string(obj, tagSeriesNumber),
		InstanceNumber:      dec.string(obj, tagInstanceNumber),
		CompletionFlag:      dec.string(obj, tagCompletionFlag),
		VerificationFlag:    dec.string(obj, tagVerificationFlag),
		ContinuityOfContent: dec.string(obj, tagContinuityOfContent),
	}
	report.Title, _ = decodeCode(dec, obj, tagConceptNameCodeSeq)
	report.CompletionFlagDescription, report.VerifyingObservers, report.PredecessorDocuments = readLifecycle(dec, obj)
	children, _ := dec.sequence(obj, tagContentSequence)
	var observer *Observer
	for index, child := range children {
		concept, _ := decodeCode(dec, child, tagConceptNameCodeSeq)
		valueType := ValueType(dec.string(child, tagValueType))
		if observer != nil && !isObserverAttribute(concept) {
			report.Observers = append(report.Observers, *observer)
			observer = nil
		}
		switch {
		case matchesConcept(concept, languageConcept):
			report.Language, _ = decodeCode(dec, child, tagConceptCodeSeq)
		case matchesConcept(concept, observerTypeConcept):
			observer = &Observer{}
		case isObserverAttribute(concept):
			if observer == nil {
				observer = &Observer{}
			}
			readObserverAttribute(dec, child, concept, observer)
		case matchesConcept(concept, procedureReportedConcept):
			if code, ok := decodeCode(dec, child, tagConceptCodeSeq); ok {
				report.ProceduresReported = append(report.ProceduresReported, code)
			}
		case valueType != ValueContainer:
		case matchesConcept(concept, imageLibraryConcept):
			rich, err := structuredImageLibraryNeeded(child, DefaultImageLibraryOptions())
			if err != nil {
				return nil, err
			}
			if rich {
				if report.ImageLibraryGroups != nil || report.ImageLibrary != nil {
					return nil, imageLibraryError("duplicate-library", "")
				}
				descriptors, groups, binding, err := readGroupedImageLibrary(child, DefaultImageLibraryOptions())
				if err != nil {
					return nil, err
				}
				report.ImageLibraryDescriptors, report.ImageLibraryGroups, report.ImageLibraryMetadata = descriptors, groups, binding
				report.ImageLibraryOrder = append([]int(nil), binding.order...)
				report.ImageLibrary = readImageLibrary(dec, child)
				report.imageLibraryReadProjection = true
			} else {
				if report.ImageLibraryGroups != nil {
					return nil, imageLibraryError("duplicate-library", "")
				}
				report.ImageLibrary = append(report.ImageLibrary, readImageLibrary(dec, child)...)
			}
		case matchesConcept(concept, imagingMeasurementsConcept):
			groups, _ := dec.sequence(child, tagContentSequence)
			for groupIndex, groupObj := range groups {
				if ValueType(dec.string(groupObj, tagValueType)) != ValueContainer {
					continue
				}
				group, err := readMeasurementGroup(dec, groupObj)
				if err != nil {
					return nil, fmt.Errorf("measurement group %d: %w", groupIndex, err)
				}
				report.Groups = append(report.Groups, group)
			}
		case matchesConcept(concept, derivedImagingMeasurementsConcept), matchesConcept(concept, qualitativeEvaluationsConcept):
		default:
			// The earlier layout: measurement groups under the root.
			group, err := readMeasurementGroup(dec, child)
			if err != nil {
				return nil, fmt.Errorf("measurement group %d: %w", index, err)
			}
			report.Groups = append(report.Groups, group)
			report.Legacy = true
		}
	}
	if observer != nil {
		report.Observers = append(report.Observers, *observer)
	}
	applyMeasurementEvidence(report, readCurrentRequestedProcedureEvidence(dec, obj))
	if b := report.ImageLibraryMetadata; b != nil {
		var err error
		report.ImageLibraryMetadata, err = BindImageLibraryMetadataWithOrder(report.ImageLibraryDescriptors, report.ImageLibraryGroups, report.ImageLibraryOrder, b.Metadata(), b.NumericMetadata(), b.options)
		if err != nil {
			return nil, err
		}
	}
	if dec.err != nil {
		return nil, dec.err
	}
	return report, nil
}

// applyMeasurementEvidence sets the Study and Series Instance UIDs and the
// series Retrieve URL of the references the evidence lists.
func applyMeasurementEvidence(report *MeasurementReport, evidence map[string]evidenceHierarchy) {
	image := func(ref *ImageReference) {
		if hierarchy, ok := evidence[clean(ref.SOPInstanceUID)]; ok {
			ref.StudyInstanceUID, ref.SeriesInstanceUID, ref.RetrieveURL = hierarchy.studyUID, hierarchy.seriesUID, hierarchy.retrieveURL
		}
	}
	for i := range report.ImageLibrary {
		image(&report.ImageLibrary[i])
	}
	for i := range report.ImageLibraryGroups {
		for j := range report.ImageLibraryGroups[i].Entries {
			image(&report.ImageLibraryGroups[i].Entries[j].Image)
		}
	}
	for i := range report.Groups {
		group := &report.Groups[i]
		if hierarchy, ok := evidence[clean(group.ReferencedSegment.SOPInstanceUID)]; ok {
			group.ReferencedSegment.StudyInstanceUID, group.ReferencedSegment.SeriesInstanceUID = hierarchy.studyUID, hierarchy.seriesUID
		}
		for j := range group.Measurements {
			image(&group.Measurements[j].Image)
		}
	}
}

func isObserverAttribute(concept CodedEntry) bool {
	for _, attribute := range []CodedEntry{personObserverNameConcept, personObserverOrganizationConcept, deviceObserverUIDConcept,
		deviceObserverNameConcept, deviceObserverManufacturerConcept, deviceObserverModelConcept, deviceObserverSerialConcept} {
		if matchesConcept(concept, attribute) {
			return true
		}
	}
	return false
}

func readObserverAttribute(dec *decoder, item *object.Object, concept CodedEntry, observer *Observer) {
	switch {
	case matchesConcept(concept, personObserverNameConcept):
		observer.PersonName = dec.string(item, tagPersonNameValue)
	case matchesConcept(concept, personObserverOrganizationConcept):
		observer.OrganizationName = dec.string(item, tagTextValue)
	case matchesConcept(concept, deviceObserverUIDConcept):
		observer.DeviceUID = dec.string(item, tagUID)
	case matchesConcept(concept, deviceObserverNameConcept):
		observer.DeviceName = dec.string(item, tagTextValue)
	case matchesConcept(concept, deviceObserverManufacturerConcept):
		observer.DeviceManufacturer = dec.string(item, tagTextValue)
	case matchesConcept(concept, deviceObserverModelConcept):
		observer.DeviceModelName = dec.string(item, tagTextValue)
	case matchesConcept(concept, deviceObserverSerialConcept):
		observer.DeviceSerialNumber = dec.string(item, tagTextValue)
	}
}

// readImageLibrary returns the IMAGE items of an Image Library, at any depth
// of its groups.
func readImageLibrary(dec *decoder, container *object.Object) []ImageReference {
	var out []ImageReference
	children, _ := dec.sequence(container, tagContentSequence)
	for _, child := range children {
		switch ValueType(dec.string(child, tagValueType)) {
		case ValueImage:
			out = append(out, readImageReference(dec, child))
		case ValueContainer:
			out = append(out, readImageLibrary(dec, child)...)
		}
	}
	return out
}

// templateSequence identifies a DCMR template (Content Template Sequence).
func templateSequence(identifier string) core.Element {
	return derivedio.Seq(tagContentTemplateSequence, derivedio.DataSet(
		derivedio.CS(tagMappingResource, "DCMR"),
		derivedio.CS(tagTemplateIdentifier, identifier),
	))
}

func containerItem(relationship string, concept CodedEntry, template string, children ...core.DataSet) core.DataSet {
	elements := []core.Element{
		derivedio.CS(tagRelationshipType, relationship),
		derivedio.CS(tagValueType, string(ValueContainer)),
		derivedio.CS(tagContinuityOfContent, "SEPARATE"),
		codeSequence(tagConceptNameCodeSeq, concept),
	}
	if template != "" {
		elements = append(elements, templateSequence(template))
	}
	if len(children) > 0 {
		elements = append(elements, derivedio.Seq(tagContentSequence, children...))
	}
	return derivedio.DataSet(elements...)
}

func codeItem(relationship string, concept, value CodedEntry) core.DataSet {
	return derivedio.DataSet(
		derivedio.CS(tagRelationshipType, relationship),
		derivedio.CS(tagValueType, string(ValueCode)),
		codeSequence(tagConceptNameCodeSeq, concept),
		codeSequence(tagConceptCodeSeq, value),
	)
}

// textItem builds a TEXT item, such as the Tracking Identifier of a
// measurement group (TID 1501 row 2).
func textItem(relationship string, concept CodedEntry, value string) core.DataSet {
	return derivedio.DataSet(
		derivedio.CS(tagRelationshipType, relationship),
		derivedio.CS(tagValueType, string(ValueText)),
		codeSequence(tagConceptNameCodeSeq, concept),
		derivedio.Str(tagTextValue, core.VRUT, value),
	)
}

// uidItem builds a UIDREF item, such as the Tracking Unique Identifier of a
// measurement group (TID 1501 row 3). A CONTAINER cannot have HAS
// PROPERTIES children in the Comprehensive SR IODs (PS3.3 Table A.35.3-2).
func uidItem(relationship string, concept CodedEntry, value string) core.DataSet {
	return derivedio.DataSet(
		derivedio.CS(tagRelationshipType, relationship),
		derivedio.CS(tagValueType, string(ValueUIDRef)),
		codeSequence(tagConceptNameCodeSeq, concept),
		derivedio.UI(tagUID, value),
	)
}

// measurementGroupDataSet builds a Measurement Group container (TID 1501, or
// TID 1411 when it references a segment).
func measurementGroupDataSet(group MeasurementGroup) core.DataSet {
	var children []core.DataSet
	if group.Tracking.Identifier != "" {
		children = append(children, textItem(RelationshipHasObsContext, trackingIdentifierConcept, group.Tracking.Identifier))
	}
	if group.Tracking.UID != "" {
		children = append(children, uidItem(RelationshipHasObsContext, trackingUniqueIdentifierConcept, group.Tracking.UID))
	}
	if !group.Finding.Empty() {
		children = append(children, codeItem(RelationshipContains, findingConcept, group.Finding))
	}
	if hasSegmentReference(group.ReferencedSegment) {
		children = append(children, segmentItem(group.ReferencedSegment))
	}
	if group.SourceSeriesUID != "" {
		children = append(children, uidItem(RelationshipContains, sourceSeriesConcept, group.SourceSeriesUID))
	}
	for _, site := range group.FindingSites {
		children = append(children, codeItem(RelationshipHasConceptMod, findingSiteConcept, site))
	}
	for _, measurement := range group.Measurements {
		children = append(children, measurementItem(measurement))
	}
	template := group.Template
	if template == "" {
		template = TemplateMeasurementGroup
		if hasSegmentReference(group.ReferencedSegment) {
			template = TemplateVolumetricMeasurements
		}
	}
	return containerItem(RelationshipContains, measurementGroupConcept, template, children...)
}

func hasSegmentReference(ref SegmentReference) bool {
	return ref.SOPClassUID != "" || ref.SOPInstanceUID != "" || ref.SegmentNumber != 0
}

// segmentItem builds the Referenced Segment IMAGE item of a measurement group
// (TID 1411 row 7): CONTAINS, since a CONTAINER cannot be INFERRED FROM an
// IMAGE in the Comprehensive SR IODs (PS3.3 Table A.35.3-2).
func segmentItem(ref SegmentReference) core.DataSet {
	return derivedio.DataSet(
		derivedio.CS(tagRelationshipType, RelationshipContains),
		derivedio.CS(tagValueType, string(ValueImage)),
		codeSequence(tagConceptNameCodeSeq, referencedSegmentConcept),
		derivedio.Seq(tagRefSOPSequence, derivedio.DataSet(
			derivedio.UI(tagRefSOPClassUID, ref.SOPClassUID),
			derivedio.UI(tagRefSOPInstanceUID, ref.SOPInstanceUID),
			derivedio.US(tagReferencedSegmentNumber, uint16(ref.SegmentNumber)),
		)),
	)
}

// measurementItem constructs a DICOM dataset representing a measurement, including its concept name, numeric value and units, and optional derivation, image and spatial references.
func measurementItem(measurement ReportMeasurement) core.DataSet {
	children := []core.DataSet{}
	if !measurement.Derivation.Empty() {
		children = append(children, codeItem(RelationshipHasConceptMod, derivationConcept, measurement.Derivation))
	}
	if hasImageReference(measurement.Image) {
		children = append(children, imageItem(measurement.Image))
	}
	if hasSpatialReference(measurement.Spatial) {
		children = append(children, spatialItem(measurement.Spatial))
	}
	elements := []core.Element{
		derivedio.CS(tagRelationshipType, RelationshipContains),
		derivedio.CS(tagValueType, string(ValueNum)),
		codeSequence(tagConceptNameCodeSeq, measurement.ConceptName),
		measuredValueSequence(Measurement{Value: measurement.Value, Units: measurement.Units}),
	}
	if len(children) > 0 {
		elements = append(elements, derivedio.Seq(tagContentSequence, children...))
	}
	return derivedio.DataSet(elements...)
}

func hasImageReference(ref ImageReference) bool {
	return ref.SOPClassUID != "" || ref.SOPInstanceUID != "" || len(ref.Frames) > 0
}

func hasSpatialReference(ref SpatialReference) bool {
	return ref.FrameOfReferenceUID != "" || ref.GraphicType != "" || len(ref.Coordinates) > 0
}

// imageItem returns the source image of a measurement (TID 320 row 1).
func imageItem(ref ImageReference) core.DataSet {
	return derivedio.DataSet(
		derivedio.CS(tagRelationshipType, RelationshipInferredFrom),
		derivedio.CS(tagValueType, string(ValueImage)),
		codeSequence(tagConceptNameCodeSeq, sourceOfMeasurementConcept),
		referencedSOPSequence(ref),
	)
}

// spatialItem returns the 3D coordinates a measurement was made on (TID 320
// row 4).
func spatialItem(ref SpatialReference) core.DataSet {
	elements := append([]core.Element{
		derivedio.CS(tagRelationshipType, RelationshipInferredFrom),
		derivedio.CS(tagValueType, "SCOORD3D"),
		codeSequence(tagConceptNameCodeSeq, sourceOfMeasurementConcept),
	}, spatialCoordinate3DElements(ref)...)
	return derivedio.DataSet(elements...)
}

// readMeasurementGroup parses a measurement group container.
func readMeasurementGroup(dec *decoder, obj *object.Object) (MeasurementGroup, error) {
	var group MeasurementGroup
	if templates, ok := dec.sequence(obj, tagContentTemplateSequence); ok && len(templates) > 0 {
		group.Template = dec.string(templates[0], tagTemplateIdentifier)
	}
	children, _ := dec.sequence(obj, tagContentSequence)
	for index, child := range children {
		concept, _ := decodeCode(dec, child, tagConceptNameCodeSeq)
		switch {
		case matchesConcept(concept, trackingIdentifierConcept):
			group.Tracking.Identifier = dec.string(child, tagTextValue)
		case matchesConcept(concept, trackingUniqueIdentifierConcept):
			group.Tracking.UID = dec.string(child, tagUID)
		case matchesConcept(concept, findingConcept):
			group.Finding, _ = decodeCode(dec, child, tagConceptCodeSeq)
		case matchesConcept(concept, findingSiteConcept):
			if site, ok := decodeCode(dec, child, tagConceptCodeSeq); ok {
				group.FindingSites = append(group.FindingSites, site)
			}
		case matchesConcept(concept, referencedSegmentConcept):
			group.ReferencedSegment = readSegmentReference(dec, child)
		case matchesConcept(concept, sourceSeriesConcept):
			group.SourceSeriesUID = dec.string(child, tagUID)
		case ValueType(dec.string(child, tagValueType)) == ValueNum:
			measurement, err := readReportMeasurement(dec, child)
			if err != nil {
				return MeasurementGroup{}, fmt.Errorf("measurement item %d: %w", index, err)
			}
			group.Measurements = append(group.Measurements, measurement)
		}
	}
	return group, nil
}

func matchesConcept(got, want CodedEntry) bool {
	if got.CodingSchemeDesignator == want.CodingSchemeDesignator {
		return got.CodeValue == want.CodeValue
	}
	return got.CodeMeaning == want.CodeMeaning
}

// readReportMeasurement extracts measurement data from a DICOM dataset object, including concept name, numeric value and units, and any associated derivation, image or spatial references.
func readReportMeasurement(dec *decoder, obj *object.Object) (ReportMeasurement, error) {
	reportMeasurement := ReportMeasurement{}
	reportMeasurement.ConceptName, _ = decodeCode(dec, obj, tagConceptNameCodeSeq)
	if measurement, _, _, ok := readMeasurement(dec, obj); ok {
		reportMeasurement.Value = measurement.Value
		reportMeasurement.Units = measurement.Units
	}
	children, _ := dec.sequence(obj, tagContentSequence)
	for index, child := range children {
		switch ValueType(dec.string(child, tagValueType)) {
		case ValueCode:
			if concept, _ := decodeCode(dec, child, tagConceptNameCodeSeq); matchesConcept(concept, derivationConcept) {
				reportMeasurement.Derivation, _ = decodeCode(dec, child, tagConceptCodeSeq)
			}
		case ValueImage:
			reportMeasurement.Image = readImageReference(dec, child)
		case ValueSCoord3D:
			spatial, err := readSpatialReference(dec, child)
			if err != nil {
				return ReportMeasurement{}, fmt.Errorf("child content item %d: %w", index, err)
			}
			reportMeasurement.Spatial = spatial
		}
	}
	return reportMeasurement, nil
}

// readSegmentReference extracts a segment reference from a DICOM object's Referenced SOP Sequence.
// If the sequence contains no items, it returns a zero-value SegmentReference.
func readSegmentReference(dec *decoder, obj *object.Object) SegmentReference {
	items, _ := dec.sequence(obj, tagRefSOPSequence)
	if len(items) == 0 {
		return SegmentReference{}
	}
	segmentNumbers := dec.ints(items[0], tagReferencedSegmentNumber)
	segmentNumber := 0
	if len(segmentNumbers) > 0 {
		segmentNumber = int(segmentNumbers[0])
	}
	return SegmentReference{
		SOPClassUID:    dec.string(items[0], tagRefSOPClassUID),
		SOPInstanceUID: dec.string(items[0], tagRefSOPInstanceUID),
		SegmentNumber:  segmentNumber,
	}
}

// readSpatialReference decodes DICOM spatial coordinate data into a SpatialReference structure, extracting the graphic type and parsing flattened float values as Point3D coordinates.
func readSpatialReference(dec *decoder, obj *object.Object) (SpatialReference, error) {
	ref := SpatialReference{
		FrameOfReferenceUID: dec.string(obj, tagReferencedFrameOfRefUID),
		GraphicType:         dec.string(obj, tagGraphicType),
		FiducialUID:         dec.string(obj, tagFiducialUID),
	}
	values := dec.floats(obj, tagGraphicData)
	if len(values)%3 != 0 {
		return SpatialReference{}, fmt.Errorf("%w: SCOORD3D has %d values, want complete x/y/z triples", ErrInvalidGraphicData, len(values))
	}
	for i := 0; i+2 < len(values); i += 3 {
		ref.Coordinates = append(ref.Coordinates, Point3D{X: values[i], Y: values[i+1], Z: values[i+2]})
	}
	return ref, nil
}

// conceptMeaning returns the code meaning from the concept name code sequence.
// It returns an empty string if the code cannot be read.
func conceptMeaning(obj *object.Object) string {
	code, ok := readCode(obj, tagConceptNameCodeSeq)
	if !ok {
		return ""
	}
	return code.CodeMeaning
}

// flattenPoint3D converts a slice of Point3D values into a flat float64 slice
// ordered as X1, Y1, Z1, X2, Y2, Z2, and so on.
func flattenPoint3D(points []Point3D) []float64 {
	out := make([]float64, 0, len(points)*3)
	for _, point := range points {
		out = append(out, point.X, point.Y, point.Z)
	}
	return out
}

// measurementSOPClass reports whether uid is a supported measurement SR storage SOP Class UID.
func measurementSOPClass(uid string) bool {
	switch uid {
	case EnhancedSRStorage, ComprehensiveSRStorage, Comprehensive3DSRStorage:
		return true
	default:
		return false
	}
}
