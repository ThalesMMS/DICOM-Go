package ian

import (
	"sort"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

// Notification is the Attribute List of one N-CREATE: the SOP Instances of
// one study whose availability is announced.
type Notification struct {
	// SpecificCharacterSet is sent only when set (Type 1C).
	SpecificCharacterSet string
	StudyInstanceUID     string
	// PerformedProcedureSteps fills the Type 2 Referenced Performed
	// Procedure Step Sequence; it is sent empty when nil.
	PerformedProcedureSteps []ProcedureStepReference
	Series                  []Series
}

// ProcedureStepReference names the Performed Procedure Step that produced
// the instances, usually a Modality Performed Procedure Step.
type ProcedureStepReference struct {
	SOPClassUID    string
	SOPInstanceUID string
	// WorkitemCodes fills the Performed Workitem Code Sequence, which is
	// sent empty when nil.
	WorkitemCodes []Code
}

// Code is a coded entry (Code Value, Coding Scheme Designator, Code Meaning).
type Code struct {
	Value, Scheme, Meaning string
}

// Series is one Referenced Series Sequence item.
type Series struct {
	SeriesInstanceUID string
	Instances         []Instance
}

// Instance is one Referenced SOP Sequence item. Empty optional fields are
// not sent.
type Instance struct {
	SOPClassUID    string
	SOPInstanceUID string
	// Availability is one of the Availability constants.
	Availability string
	// RetrieveAETitle is the AE Title to retrieve the instance from.
	RetrieveAETitle        string
	RetrieveLocationUID    string
	RetrieveURL            string
	RetrieveURI            string
	StorageMediaFileSetID  string
	StorageMediaFileSetUID string
}

// Count returns the number of SOP Instances the notification references.
func (n Notification) Count() int {
	count := 0
	for _, series := range n.Series {
		count += len(series.Instances)
	}
	return count
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

func optional(elements []core.Element, tag core.Tag, vr core.VR, value string) []core.Element {
	if value == "" {
		return elements
	}
	return append(elements, text(tag, vr, value))
}

func instanceItem(instance Instance) core.DataSet {
	elements := []core.Element{
		text(TagReferencedSOPClassUID, core.VRUI, instance.SOPClassUID),
		text(TagReferencedSOPInstanceUID, core.VRUI, instance.SOPInstanceUID),
		text(TagInstanceAvailability, core.VRCS, instance.Availability),
	}
	if instance.RetrieveAETitle != "" || (instance.RetrieveLocationUID == "" && instance.RetrieveURL == "" && instance.RetrieveURI == "") {
		elements = append(elements, text(TagRetrieveAETitle, core.VRAE, instance.RetrieveAETitle))
	}
	elements = optional(elements, TagRetrieveLocationUID, core.VRUI, instance.RetrieveLocationUID)
	elements = optional(elements, TagRetrieveURL, core.VRUR, instance.RetrieveURL)
	elements = optional(elements, TagRetrieveURI, core.VRUR, instance.RetrieveURI)
	elements = optional(elements, TagStorageMediaFileSetID, core.VRSH, instance.StorageMediaFileSetID)
	elements = optional(elements, TagStorageMediaFileSetUID, core.VRUI, instance.StorageMediaFileSetUID)
	return item(elements...)
}

func codeItems(codes []Code) []core.DataSet {
	items := make([]core.DataSet, len(codes))
	for i, code := range codes {
		items[i] = item(text(TagCodeValue, core.VRSH, code.Value), text(TagCodingSchemeDesignator, core.VRSH, code.Scheme), text(TagCodeMeaning, core.VRLO, code.Meaning))
	}
	return items
}

// Build returns the N-CREATE Attribute List of n, validated with Validate.
func Build(n Notification) (*object.Object, error) {
	steps := make([]core.DataSet, len(n.PerformedProcedureSteps))
	for i, step := range n.PerformedProcedureSteps {
		steps[i] = item(
			text(TagReferencedSOPClassUID, core.VRUI, step.SOPClassUID),
			text(TagReferencedSOPInstanceUID, core.VRUI, step.SOPInstanceUID),
			sequence(TagPerformedWorkitemCodeSequence, codeItems(step.WorkitemCodes)...),
		)
	}
	series := make([]core.DataSet, len(n.Series))
	for i, s := range n.Series {
		instances := make([]core.DataSet, len(s.Instances))
		for j, instance := range s.Instances {
			instances[j] = instanceItem(instance)
		}
		series[i] = item(
			sequence(TagReferencedSOPSequence, instances...),
			text(TagSeriesInstanceUID, core.VRUI, s.SeriesInstanceUID),
		)
	}
	elements := []core.Element{
		sequence(TagReferencedPerformedProcedureStepSequence, steps...),
		sequence(TagReferencedSeriesSequence, series...),
		text(TagStudyInstanceUID, core.VRUI, n.StudyInstanceUID),
	}
	if n.SpecificCharacterSet != "" {
		elements = append(elements, text(TagSpecificCharacterSet, core.VRCS, n.SpecificCharacterSet))
	}
	ds := object.FromElements(item(elements...).Elements, std.Dictionary)
	if err := Validate(ds); err != nil {
		return nil, err
	}
	return ds, nil
}

// Parse validates ds and returns its notification. Attributes outside the
// module are ignored.
func Parse(ds *object.Object) (Notification, error) {
	if err := Validate(ds); err != nil {
		return Notification{}, err
	}
	n := Notification{
		SpecificCharacterSet: joined(ds, TagSpecificCharacterSet),
		StudyInstanceUID:     joined(ds, TagStudyInstanceUID),
	}
	steps, _ := ds.GetSequence(TagReferencedPerformedProcedureStepSequence)
	for _, step := range steps {
		reference := ProcedureStepReference{SOPClassUID: joined(step, TagReferencedSOPClassUID), SOPInstanceUID: joined(step, TagReferencedSOPInstanceUID)}
		codes, _ := step.GetSequence(TagPerformedWorkitemCodeSequence)
		for _, code := range codes {
			reference.WorkitemCodes = append(reference.WorkitemCodes, Code{Value: joined(code, TagCodeValue), Scheme: joined(code, TagCodingSchemeDesignator), Meaning: joined(code, TagCodeMeaning)})
		}
		n.PerformedProcedureSteps = append(n.PerformedProcedureSteps, reference)
	}
	seriesItems, _ := ds.GetSequence(TagReferencedSeriesSequence)
	for _, seriesItem := range seriesItems {
		series := Series{SeriesInstanceUID: joined(seriesItem, TagSeriesInstanceUID)}
		instances, _ := seriesItem.GetSequence(TagReferencedSOPSequence)
		for _, instance := range instances {
			series.Instances = append(series.Instances, Instance{
				SOPClassUID:            joined(instance, TagReferencedSOPClassUID),
				SOPInstanceUID:         joined(instance, TagReferencedSOPInstanceUID),
				Availability:           joined(instance, TagInstanceAvailability),
				RetrieveAETitle:        joined(instance, TagRetrieveAETitle),
				RetrieveLocationUID:    joined(instance, TagRetrieveLocationUID),
				RetrieveURL:            joined(instance, TagRetrieveURL),
				RetrieveURI:            joined(instance, TagRetrieveURI),
				StorageMediaFileSetID:  joined(instance, TagStorageMediaFileSetID),
				StorageMediaFileSetUID: joined(instance, TagStorageMediaFileSetUID),
			})
		}
		n.Series = append(n.Series, series)
	}
	return n, nil
}

func joined(ds *object.Object, tag core.Tag) string {
	values, err := ds.LookupStrings(tag)
	if err != nil {
		return ""
	}
	for i := range values {
		values[i] = strings.TrimSpace(values[i])
	}
	return strings.TrimSpace(strings.Join(values, `\`))
}

// StudyInstance is one SOP Instance located in its study and series, the
// input of Group.
type StudyInstance struct {
	StudyInstanceUID  string
	SeriesInstanceUID string
	Instance          Instance
}

// Group returns one Notification per study, in the order the studies, series
// and instances first appear. Each notification references the given
// Performed Procedure Steps. Instances repeated within a series are kept
// once.
func Group(instances []StudyInstance, steps []ProcedureStepReference) []Notification {
	var notifications []Notification
	studyIndex := map[string]int{}
	seriesIndex := map[[2]string]int{}
	seen := map[[3]string]bool{}
	for _, located := range instances {
		study, ok := studyIndex[located.StudyInstanceUID]
		if !ok {
			study = len(notifications)
			studyIndex[located.StudyInstanceUID] = study
			notifications = append(notifications, Notification{StudyInstanceUID: located.StudyInstanceUID, PerformedProcedureSteps: append([]ProcedureStepReference(nil), steps...)})
		}
		seriesKey := [2]string{located.StudyInstanceUID, located.SeriesInstanceUID}
		series, ok := seriesIndex[seriesKey]
		if !ok {
			series = len(notifications[study].Series)
			seriesIndex[seriesKey] = series
			notifications[study].Series = append(notifications[study].Series, Series{SeriesInstanceUID: located.SeriesInstanceUID})
		}
		instanceKey := [3]string{located.StudyInstanceUID, located.SeriesInstanceUID, located.Instance.SOPInstanceUID}
		if seen[instanceKey] {
			continue
		}
		seen[instanceKey] = true
		notifications[study].Series[series].Instances = append(notifications[study].Series[series].Instances, located.Instance)
	}
	return notifications
}
