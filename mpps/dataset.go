package mpps

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

// Reference names one composite SOP Instance.
type Reference struct {
	SOPClassUID    string
	SOPInstanceUID string
}

// Code is a coded entry (Code Value, Coding Scheme Designator, Code Meaning).
type Code struct {
	Value, Scheme, Meaning string
}

// ScheduledStep is one item of the Scheduled Step Attributes Sequence. For an
// unscheduled procedure the caller still supplies the Study Instance UID and
// leaves the Type 2 identifiers empty; nothing is invented.
type ScheduledStep struct {
	StudyInstanceUID                  string
	ReferencedStudies                 []Reference
	AccessionNumber                   string
	RequestedProcedureID              string
	RequestedProcedureDescription     string
	ScheduledProcedureStepID          string
	ScheduledProcedureStepDescription string
	ScheduledProtocolCodes            []Code
}

// CreateAttributes are the N-CREATE attributes of Table F.7.2-1. Empty strings
// encode Type 2 attributes as present and empty.
type CreateAttributes struct {
	SpecificCharacterSet string
	ScheduledSteps       []ScheduledStep
	PatientName          string
	PatientID            string
	PatientBirthDate     string
	PatientSex           string

	PerformedProcedureStepID string
	PerformedStationAETitle  string
	PerformedStationName     string
	PerformedLocation        string
	StartDate                string
	StartTime                string
	Description              string
	TypeDescription          string
	ProcedureCodes           []Code
	Modality                 string
	StudyID                  string
	PerformedProtocolCodes   []Code
	// PerformedSeries is usually empty at creation.
	PerformedSeries []PerformedSeries
}

// PerformedSeries is one item of the Performed Series Sequence.
type PerformedSeries struct {
	PerformingPhysicianName string
	ProtocolName            string
	OperatorsName           string
	SeriesInstanceUID       string
	SeriesDescription       string
	RetrieveAETitle         string
	Images                  []Reference
	NonImages               []Reference
}

// Update is an N-SET modification list. Zero fields are not sent; a non-nil
// PerformedSeries replaces the whole sequence (PS3.4 F.7.2.2.2).
type Update struct {
	Status                string
	EndDate               string
	EndTime               string
	Description           *string
	Comments              *string
	PerformedSeries       []PerformedSeries
	DiscontinuationReason *Code
	PerformedProtocols    []Code
	ProcedureCodes        []Code
}

func text(tag core.Tag, vr core.VR, value string) core.Element {
	element := core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}}
	if value == "" {
		element.Value = core.StringValue(nil)
	} else {
		element.Value = core.StringValue{value}
	}
	return element
}

func sequence(tag core.Tag, items ...core.DataSet) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRSQ, Length: core.UndefinedLength, LengthSet: true}, Value: core.SequenceValue{Items: items}}
}

func item(elements ...core.Element) core.DataSet {
	sort.Slice(elements, func(i, j int) bool { return lessTag(elements[i].Tag(), elements[j].Tag()) })
	return core.DataSet{Elements: elements}
}

func lessTag(a, b core.Tag) bool {
	if a.Group != b.Group {
		return a.Group < b.Group
	}
	return a.Element < b.Element
}

func referenceItems(refs []Reference) []core.DataSet {
	items := make([]core.DataSet, len(refs))
	for i, ref := range refs {
		items[i] = item(text(TagReferencedSOPClassUID, core.VRUI, ref.SOPClassUID), text(TagReferencedSOPInstanceUID, core.VRUI, ref.SOPInstanceUID))
	}
	return items
}

func codeItems(codes []Code) []core.DataSet {
	items := make([]core.DataSet, len(codes))
	for i, code := range codes {
		items[i] = item(text(TagCodeValue, core.VRSH, code.Value), text(TagCodingSchemeDesignator, core.VRSH, code.Scheme), text(TagCodeMeaning, core.VRLO, code.Meaning))
	}
	return items
}

func seriesItems(series []PerformedSeries) []core.DataSet {
	items := make([]core.DataSet, len(series))
	for i, s := range series {
		items[i] = item(
			text(TagPerformingPhysicianName, core.VRPN, s.PerformingPhysicianName),
			text(TagProtocolName, core.VRLO, s.ProtocolName),
			text(TagOperatorsName, core.VRPN, s.OperatorsName),
			text(TagSeriesInstanceUID, core.VRUI, s.SeriesInstanceUID),
			text(TagSeriesDescription, core.VRLO, s.SeriesDescription),
			text(TagRetrieveAETitle, core.VRAE, s.RetrieveAETitle),
			sequence(TagReferencedImageSequence, referenceItems(s.Images)...),
			sequence(TagReferencedNonImageSequence, referenceItems(s.NonImages)...),
		)
	}
	return items
}

func newDataSet(elements []core.Element) *object.Object {
	return object.FromElements(elements, std.Dictionary)
}

// BuildCreate returns the N-CREATE Attribute List with Performed Procedure
// Step Status IN PROGRESS and every Type 2 attribute of the subset present.
// It is validated with ValidateCreate before it is returned.
func BuildCreate(a CreateAttributes) (*object.Object, error) {
	steps := make([]core.DataSet, len(a.ScheduledSteps))
	for i, s := range a.ScheduledSteps {
		steps[i] = item(
			text(TagStudyInstanceUID, core.VRUI, s.StudyInstanceUID),
			sequence(TagReferencedStudySequence, referenceItems(s.ReferencedStudies)...),
			text(TagAccessionNumber, core.VRSH, s.AccessionNumber),
			text(TagRequestedProcedureID, core.VRSH, s.RequestedProcedureID),
			text(TagRequestedProcedureDescription, core.VRLO, s.RequestedProcedureDescription),
			text(TagScheduledProcedureStepID, core.VRSH, s.ScheduledProcedureStepID),
			text(TagScheduledProcedureStepDescription, core.VRLO, s.ScheduledProcedureStepDescription),
			sequence(TagScheduledProtocolCodeSequence, codeItems(s.ScheduledProtocolCodes)...),
		)
	}
	elements := []core.Element{
		sequence(TagScheduledStepAttributesSequence, steps...),
		text(TagPatientName, core.VRPN, a.PatientName),
		text(TagPatientID, core.VRLO, a.PatientID),
		text(TagPatientBirthDate, core.VRDA, a.PatientBirthDate),
		text(TagPatientSex, core.VRCS, a.PatientSex),
		sequence(TagReferencedPatientSequence),
		text(TagPerformedProcedureStepID, core.VRSH, a.PerformedProcedureStepID),
		text(TagPerformedStationAETitle, core.VRAE, a.PerformedStationAETitle),
		text(TagPerformedStationName, core.VRSH, a.PerformedStationName),
		text(TagPerformedLocation, core.VRSH, a.PerformedLocation),
		text(TagPerformedProcedureStepStartDate, core.VRDA, a.StartDate),
		text(TagPerformedProcedureStepStartTime, core.VRTM, a.StartTime),
		text(TagPerformedProcedureStepStatus, core.VRCS, StatusInProgress),
		text(TagPerformedProcedureStepDescription, core.VRLO, a.Description),
		text(TagPerformedProcedureTypeDescription, core.VRLO, a.TypeDescription),
		sequence(TagProcedureCodeSequence, codeItems(a.ProcedureCodes)...),
		text(TagPerformedProcedureStepEndDate, core.VRDA, ""),
		text(TagPerformedProcedureStepEndTime, core.VRTM, ""),
		text(TagModality, core.VRCS, a.Modality),
		text(TagStudyID, core.VRSH, a.StudyID),
		sequence(TagPerformedProtocolCodeSequence, codeItems(a.PerformedProtocolCodes)...),
		sequence(TagPerformedSeriesSequence, seriesItems(a.PerformedSeries)...),
	}
	if a.SpecificCharacterSet != "" {
		elements = append(elements, text(TagSpecificCharacterSet, core.VRCS, a.SpecificCharacterSet))
	}
	ds := newDataSet(elements)
	if err := ValidateCreate(ds); err != nil {
		return nil, err
	}
	return ds, nil
}

// BuildSet returns an N-SET Modification List. It checks only what can be
// checked without the stored step; the SCP validates the merged result.
func BuildSet(u Update) (*object.Object, error) {
	var elements []core.Element
	if u.Status != "" {
		elements = append(elements, text(TagPerformedProcedureStepStatus, core.VRCS, u.Status))
	}
	if u.EndDate != "" || u.EndTime != "" {
		elements = append(elements, text(TagPerformedProcedureStepEndDate, core.VRDA, u.EndDate), text(TagPerformedProcedureStepEndTime, core.VRTM, u.EndTime))
	}
	if u.Description != nil {
		elements = append(elements, text(TagPerformedProcedureStepDescription, core.VRLO, *u.Description))
	}
	if u.Comments != nil {
		elements = append(elements, text(TagCommentsOnPerformedProcedureStep, core.VRST, *u.Comments))
	}
	if u.PerformedSeries != nil {
		elements = append(elements, sequence(TagPerformedSeriesSequence, seriesItems(u.PerformedSeries)...))
	}
	if u.DiscontinuationReason != nil {
		elements = append(elements, sequence(TagDiscontinuationReasonCodeSequence, codeItems([]Code{*u.DiscontinuationReason})...))
	}
	if u.PerformedProtocols != nil {
		elements = append(elements, sequence(TagPerformedProtocolCodeSequence, codeItems(u.PerformedProtocols)...))
	}
	if u.ProcedureCodes != nil {
		elements = append(elements, sequence(TagProcedureCodeSequence, codeItems(u.ProcedureCodes)...))
	}
	if len(elements) == 0 {
		return nil, fmt.Errorf("%w: empty modification list", ErrInvalidDataSet)
	}
	ds := newDataSet(elements)
	if err := validateModificationShape(ds); err != nil {
		return nil, err
	}
	return ds, nil
}

// Complete is BuildSet for the COMPLETED transition.
func Complete(endDate, endTime string, series []PerformedSeries) (*object.Object, error) {
	return BuildSet(Update{Status: StatusCompleted, EndDate: endDate, EndTime: endTime, PerformedSeries: nonNilSeries(series)})
}

// Discontinue is BuildSet for the DISCONTINUED transition. A reason is
// optional (Type 3).
func Discontinue(endDate, endTime string, series []PerformedSeries, reason *Code) (*object.Object, error) {
	return BuildSet(Update{Status: StatusDiscontinued, EndDate: endDate, EndTime: endTime, PerformedSeries: nonNilSeries(series), DiscontinuationReason: reason})
}

func nonNilSeries(series []PerformedSeries) []PerformedSeries {
	if series == nil {
		return []PerformedSeries{}
	}
	return series
}

func trimmed(value string) string { return strings.TrimSpace(value) }
