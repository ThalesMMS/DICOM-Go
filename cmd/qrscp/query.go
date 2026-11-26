package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/index"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/qrmatch"
)

var (
	tagSpecificCharacterSet = core.NewTag(0x0008, 0x0005)
	tagQueryRetrieveLevel   = core.NewTag(0x0008, 0x0052)
	tagRetrieveAETitle      = core.NewTag(0x0008, 0x0054)
	tagInstanceAvailability = core.NewTag(0x0008, 0x0056)
)

// errIdentifier marks an Identifier this SCP refuses (status A900).
var errIdentifier = errors.New("qrscp: identifier does not match the SOP Class")

// errTooManyMatches marks a result above -max-results (status A700).
var errTooManyMatches = errors.New("qrscp: too many matches")

func element(keyword string, values ...string) core.Element {
	entry, ok := std.Dictionary.ByKeyword(keyword)
	if !ok {
		panic("qrscp: unknown keyword " + keyword)
	}
	value := core.StringValue(values)
	if len(values) == 1 && values[0] == "" {
		value = core.StringValue(nil)
	}
	return core.Element{Header: core.ElementHeader{Tag: entry.Tag, VR: entry.VR}, Value: value}
}

func usElement(keyword string, value int64) core.Element {
	entry, _ := std.Dictionary.ByKeyword(keyword)
	if value <= 0 || value > 0xFFFF {
		return core.Element{Header: core.ElementHeader{Tag: entry.Tag, VR: core.VRUS}, Value: core.Uint16Value(nil)}
	}
	return core.Element{Header: core.ElementHeader{Tag: entry.Tag, VR: core.VRUS}, Value: core.Uint16Value{uint16(value)}}
}

func count(keyword string, n int) core.Element {
	return element(keyword, strconv.Itoa(n))
}

func sortedKeys(set map[string]bool) []string {
	values := make([]string, 0, len(set))
	for value := range set {
		if value != "" {
			values = append(values, value)
		}
	}
	sort.Strings(values)
	return values
}

// tree holds the attributes of each patient, study and series, computed
// once from the catalog. Patients are keyed by Patient ID.
type tree struct {
	patients map[string][]core.Element
	studies  map[string][]core.Element
	series   map[string][]core.Element
}

func buildTree(c *catalog) *tree {
	t := &tree{patients: map[string][]core.Element{}, studies: map[string][]core.Element{}, series: map[string][]core.Element{}}
	patientStudies := map[string]map[string]bool{}
	patientSeries := map[string]map[string]bool{}
	patientInstances := map[string]int{}
	studySeries := map[string]map[string]bool{}
	studyInstances := map[string]int{}
	studyModalities := map[string]map[string]bool{}
	studyClasses := map[string]map[string]bool{}
	seriesInstances := map[string]int{}
	first := map[string]*instance{}
	for _, item := range c.instances {
		pid, study, series := item.patientID(), item.record.Study.InstanceUID, item.record.Series.InstanceUID
		for _, key := range []string{"P" + pid, "S" + study, "R" + series} {
			if first[key] == nil {
				first[key] = item
			}
		}
		if patientStudies[pid] == nil {
			patientStudies[pid], patientSeries[pid] = map[string]bool{}, map[string]bool{}
		}
		patientStudies[pid][study] = true
		patientSeries[pid][series] = true
		patientInstances[pid]++
		if studySeries[study] == nil {
			studySeries[study], studyModalities[study], studyClasses[study] = map[string]bool{}, map[string]bool{}, map[string]bool{}
		}
		studySeries[study][series] = true
		studyInstances[study]++
		studyModalities[study][item.record.Series.Modality] = true
		studyClasses[study][item.record.Instance.SOPClassUID] = true
		seriesInstances[series]++
	}
	for pid := range patientStudies {
		item := first["P"+pid]
		var patient index.Patient
		if item.record.Patient != nil {
			patient = *item.record.Patient
		}
		t.patients[pid] = []core.Element{
			element("PatientName", patient.Name),
			element("PatientID", pid),
			element("IssuerOfPatientID", item.extra[core.NewTag(0x0010, 0x0021)]),
			element("PatientBirthDate", patient.BirthDate),
			element("PatientSex", patient.Sex),
			count("NumberOfPatientRelatedStudies", len(patientStudies[pid])),
			count("NumberOfPatientRelatedSeries", len(patientSeries[pid])),
			count("NumberOfPatientRelatedInstances", patientInstances[pid]),
		}
	}
	for study := range studySeries {
		item := first["S"+study]
		t.studies[study] = []core.Element{
			element("StudyDate", item.record.Study.Date),
			element("StudyTime", item.record.Study.Time),
			element("AccessionNumber", item.record.Study.AccessionNumber),
			element("ModalitiesInStudy", sortedKeys(studyModalities[study])...),
			element("ReferringPhysicianName", item.extra[core.NewTag(0x0008, 0x0090)]),
			element("StudyDescription", item.record.Study.Description),
			element("StudyInstanceUID", study),
			element("StudyID", item.extra[core.NewTag(0x0020, 0x0010)]),
			element("SOPClassesInStudy", sortedKeys(studyClasses[study])...),
			count("NumberOfStudyRelatedSeries", len(studySeries[study])),
			count("NumberOfStudyRelatedInstances", studyInstances[study]),
		}
	}
	for series := range seriesInstances {
		item := first["R"+series]
		t.series[series] = []core.Element{
			element("Modality", item.record.Series.Modality),
			element("InstitutionName", item.extra[core.NewTag(0x0008, 0x0080)]),
			element("SeriesDate", item.record.Series.Date),
			element("SeriesTime", item.record.Series.Time),
			element("SeriesDescription", item.record.Series.Description),
			element("SeriesInstanceUID", series),
			element("SeriesNumber", item.record.Series.Number),
			count("NumberOfSeriesRelatedInstances", seriesInstances[series]),
		}
	}
	return t
}

func instanceElements(item *instance) []core.Element {
	elements := []core.Element{
		element("SOPClassUID", item.record.Instance.SOPClassUID),
		element("SOPInstanceUID", item.record.Instance.SOPInstanceUID),
		element("InstanceNumber", item.record.Instance.Number),
	}
	frames := ""
	if item.record.Instance.NumberOfFrames > 0 {
		frames = strconv.FormatInt(item.record.Instance.NumberOfFrames, 10)
	}
	elements = append(elements, element("NumberOfFrames", frames))
	var rows, columns int64
	if item.record.Geometry != nil {
		rows, columns = item.record.Geometry.Rows, item.record.Geometry.Columns
	}
	return append(elements, usElement("Rows", rows), usElement("Columns", columns))
}

// entity is one matched patient, study, series or instance.
type entity struct {
	attributes *object.Object
	members    []*instance
}

// levelKey is the identity of item at level.
func levelKey(item *instance, level string) string {
	switch level {
	case dimse.QueryRetrieveLevelPatient:
		return item.patientID()
	case dimse.QueryRetrieveLevelStudy:
		return item.record.Study.InstanceUID
	case dimse.QueryRetrieveLevelSeries:
		return item.record.Series.InstanceUID
	default:
		return item.record.Instance.SOPInstanceUID
	}
}

// candidate holds the attributes of the entity at level and of its
// ancestors, so keys of higher levels match as well.
func (t *tree) candidate(item *instance, level string) *object.Object {
	elements := []core.Element{element("SpecificCharacterSet", "ISO_IR 192")}
	elements = append(elements, t.patients[item.patientID()]...)
	if level != dimse.QueryRetrieveLevelPatient {
		elements = append(elements, t.studies[item.record.Study.InstanceUID]...)
	}
	if level == dimse.QueryRetrieveLevelSeries || level == dimse.QueryRetrieveLevelImage {
		elements = append(elements, t.series[item.record.Series.InstanceUID]...)
	}
	if level == dimse.QueryRetrieveLevelImage {
		elements = append(elements, instanceElements(item)...)
	}
	return object.FromElements(elements, std.Dictionary)
}

// uniqueKeys are the Unique Keys of the level and its ancestors in model.
func uniqueKeys(model dimse.QueryRetrieveModel, level string) ([]core.Tag, error) {
	keywords, err := dimse.QueryRetrieveRequiredKeys(model, level)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errIdentifier, err)
	}
	tags := make([]core.Tag, 0, len(keywords))
	for _, keyword := range keywords {
		entry, _ := std.Dictionary.ByKeyword(keyword)
		tags = append(tags, entry.Tag)
	}
	return tags, nil
}

func modelOf(sopClassUID string) dimse.QueryRetrieveModel {
	switch sopClassUID {
	case dimse.PatientRootFindSOPClassUID, dimse.PatientRootMoveSOPClassUID, dimse.PatientRootGetSOPClassUID:
		return dimse.QueryRetrieveModelPatientRoot
	default:
		return dimse.QueryRetrieveModelStudyRoot
	}
}

// selectEntities matches identifier against every entity at level. Keys
// the catalog does not hold do not filter (qrmatch SupportedMatching); keys
// of higher levels are matched like any other key. retrieve requires a value
// for the Unique Key of the level, as C-MOVE and C-GET do.
func (s *server) selectEntities(ctx context.Context, model dimse.QueryRetrieveModel, level string, identifier *object.Object, retrieve bool) ([]entity, error) {
	if identifier == nil {
		return nil, errIdentifier
	}
	level = strings.ToUpper(strings.TrimSpace(level))
	keys, err := uniqueKeys(model, level)
	if err != nil {
		return nil, err
	}
	if retrieve {
		if values, ok := identifier.GetUIDs(keys[len(keys)-1]); !ok || len(values) == 0 {
			if level != dimse.QueryRetrieveLevelPatient {
				return nil, fmt.Errorf("%w: the Unique Key of the %s level has no value", errIdentifier, level)
			}
			if id, _ := identifier.GetString(keys[0]); strings.TrimSpace(id) == "" {
				return nil, fmt.Errorf("%w: Patient ID has no value", errIdentifier)
			}
		}
	}
	if _, err := identifier.CharacterSet(); err != nil {
		return nil, fmt.Errorf("%w: invalid Specific Character Set", errIdentifier)
	}
	var filterElements []core.Element
	for _, elem := range identifier.Elements() {
		switch elem.Tag() {
		case tagQueryRetrieveLevel, tagRetrieveAETitle, tagInstanceAvailability:
			continue
		}
		if elem.Tag().Group == 0x0002 || elem.Tag().Group == 0x0000 {
			continue
		}
		filterElements = append(filterElements, elem)
	}
	filter := object.FromElements(filterElements, std.Dictionary)
	position := make(map[string]int)
	var matched []entity
	for _, item := range s.catalog.instances {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := levelKey(item, level)
		if i, ok := position[key]; ok {
			if matched[i].attributes != nil {
				matched[i].members = append(matched[i].members, item)
			}
			continue
		}
		candidate := s.tree.candidate(item, level)
		supported := make([]core.Tag, 0, candidate.Len())
		for _, elem := range candidate.Elements() {
			supported = append(supported, elem.Tag())
		}
		result, err := qrmatch.Match(filter, candidate, qrmatch.Options{Context: ctx, Limits: qrmatch.DefaultLimits(), SupportedMatching: supported, LenientDateQueries: true})
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, fmt.Errorf("%w: %v", errIdentifier, err)
		}
		position[key] = len(matched)
		if !result.Match {
			// Remember the key so later members are skipped cheaply.
			matched = append(matched, entity{})
			continue
		}
		matched = append(matched, entity{attributes: candidate, members: []*instance{item}})
	}
	selected := matched[:0]
	for _, e := range matched {
		if e.attributes != nil {
			selected = append(selected, e)
		}
	}
	if len(selected) > s.maxResults {
		return nil, errTooManyMatches
	}
	return selected, nil
}

// response is the C-FIND response Identifier: every requested key with the
// entity's value, or empty when the catalog has none, plus the Unique Keys
// of the level and its ancestors.
func (s *server) response(e entity, model dimse.QueryRetrieveModel, level string, identifier *object.Object) *object.Object {
	out := object.FromElements([]core.Element{
		element("SpecificCharacterSet", "ISO_IR 192"),
		element("QueryRetrieveLevel", level),
	}, std.Dictionary)
	keys, _ := uniqueKeys(model, level)
	for _, tag := range keys {
		if elem, ok := e.attributes.Get(tag); ok {
			out.Put(elem)
		}
	}
	for _, requested := range identifier.Elements() {
		tag := requested.Tag()
		switch {
		case tag == tagSpecificCharacterSet || tag == tagQueryRetrieveLevel || tag.Group == 0x0000 || tag.Group == 0x0002:
			continue
		case tag == tagRetrieveAETitle:
			out.Put(element("RetrieveAETitle", s.aeTitle))
		case tag == tagInstanceAvailability:
			out.Put(element("InstanceAvailability", "ONLINE"))
		default:
			if elem, ok := e.attributes.Get(tag); ok {
				out.Put(elem)
				continue
			}
			if requested.VR() == core.VRSQ {
				out.Put(core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRSQ}, Value: core.SequenceValue{}})
				continue
			}
			out.Put(core.NewRawElement(tag, requested.VR(), nil))
		}
	}
	return out
}

func instancesOf(entities []entity) []*instance {
	var out []*instance
	for _, e := range entities {
		out = append(out, e.members...)
	}
	return out
}
