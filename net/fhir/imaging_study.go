// Package fhir projects DICOM imaging metadata into FHIR R4 and queries
// explicitly configured FHIR endpoints.
package fhir

import (
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dcmtime"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/object"
)

const (
	imagingStudyType = "ImagingStudy"
	dicomUIDSystem   = "urn:dicom:uid"
	dicomOntology    = "http://dicom.nema.org/resources/ontology/DCM"
	oidPrefix        = "urn:oid:"
	maxMappedObjects = 100_000
)

// ErrImagingIdentity reports an incomplete or conflicting imaging hierarchy.
var ErrImagingIdentity = errors.New("fhir: invalid DICOM imaging identity")

// Identifier is the system/value projection of a FHIR R4 Identifier.
type Identifier struct {
	System string `json:"system,omitempty"`
	Value  string `json:"value,omitempty"`
}

// Reference is the literal-reference projection of a FHIR R4 Reference.
type Reference struct {
	Reference  string      `json:"reference,omitempty"`
	Identifier *Identifier `json:"identifier,omitempty"`
	Display    string      `json:"display,omitempty"`
}

// Coding holds the code and system used by the imaging hierarchy.
type Coding struct {
	System  string `json:"system,omitempty"`
	Code    string `json:"code,omitempty"`
	Display string `json:"display,omitempty"`
}

// ImagingStudy is the FHIR R4 imaging hierarchy projection. Started remains
// a string so the sender's precision and explicit time zone are preserved.
type ImagingStudy struct {
	ResourceType      string          `json:"resourceType"`
	ID                string          `json:"id,omitempty"`
	Identifiers       []Identifier    `json:"identifier,omitempty"`
	Status            string          `json:"status"`
	Subject           Reference       `json:"subject"`
	Started           string          `json:"started,omitempty"`
	Description       string          `json:"description,omitempty"`
	Modality          []Coding        `json:"modality,omitempty"`
	Endpoints         []Reference     `json:"endpoint,omitempty"`
	NumberOfSeries    *int            `json:"numberOfSeries,omitempty"`
	NumberOfInstances *int            `json:"numberOfInstances,omitempty"`
	Series            []ImagingSeries `json:"series,omitempty"`
}

// ImagingSeries is one FHIR ImagingStudy.series entry.
type ImagingSeries struct {
	UID               string            `json:"uid"`
	Number            *int              `json:"number,omitempty"`
	Modality          Coding            `json:"modality"`
	Description       string            `json:"description,omitempty"`
	Started           string            `json:"started,omitempty"`
	NumberOfInstances *int              `json:"numberOfInstances,omitempty"`
	Instances         []ImagingInstance `json:"instance,omitempty"`
}

// ImagingInstance is one FHIR ImagingStudy.series.instance entry.
type ImagingInstance struct {
	UID      string `json:"uid"`
	SOPClass Coding `json:"sopClass"`
	Number   *int   `json:"number,omitempty"`
}

// DICOMStudyInstanceUID extracts one unique valid DICOM UID from the R4
// ImagingStudy identifiers. Conflicting or malformed UID identifiers fail.
func (s ImagingStudy) DICOMStudyInstanceUID() (string, bool) {
	var result string
	for _, identifier := range s.Identifiers {
		if identifier.System != dicomUIDSystem {
			continue
		}
		uid, prefixed := strings.CutPrefix(identifier.Value, oidPrefix)
		if !prefixed || !core.IsValidUID(uid) || (result != "" && uid != result) {
			return "", false
		}
		result = uid
	}
	return result, result != ""
}

// ImagingStudyOptions supplies application-owned resource references. Subject
// must name a known Patient/<id>. The optional Endpoint must name an
// Endpoint/<id>. These are identifiers, never destinations the library opens.
type ImagingStudyOptions struct {
	ID              string
	Subject         string
	Endpoint        string
	AccessionSystem string
}

// MapImagingStudy projects metadata for one DICOM study. It reads no Pixel
// Data. Study metadata comes from the first dataset and series metadata from
// the first dataset supplied for that series. Series and instances are then
// sorted by their optional numbers, followed by UID.
func MapImagingStudy(datasets []*object.Object, options ImagingStudyOptions) (ImagingStudy, error) {
	if len(datasets) == 0 || datasets[0] == nil {
		return ImagingStudy{}, ErrImagingIdentity
	}
	if len(datasets) > maxMappedObjects {
		return ImagingStudy{}, ErrLimit
	}
	if (options.ID != "" && !validResourceID(options.ID)) || !validReference(options.Subject, "Patient") ||
		(options.Endpoint != "" && !validReference(options.Endpoint, "Endpoint")) {
		return ImagingStudy{}, ErrImagingIdentity
	}
	if options.AccessionSystem != "" {
		system, err := url.Parse(options.AccessionSystem)
		if err != nil || !system.IsAbs() || options.AccessionSystem == dicomUIDSystem || len(options.AccessionSystem) > 4096 ||
			strings.ContainsAny(options.AccessionSystem, "\r\n\t ") {
			return ImagingStudy{}, ErrImagingIdentity
		}
	}

	first := datasets[0]
	studyUID := dicomUID(first, tags.StudyInstanceUID)
	if !core.IsValidUID(studyUID) {
		return ImagingStudy{}, ErrImagingIdentity
	}
	study := ImagingStudy{
		ResourceType: imagingStudyType,
		ID:           options.ID,
		Identifiers:  []Identifier{{System: dicomUIDSystem, Value: oidPrefix + studyUID}},
		Status:       "available",
		Subject:      Reference{Reference: options.Subject},
		Started:      dicomStarted(first, tags.StudyDate, tags.StudyTime),
		Description:  dicomValue(first, tags.StudyDescription),
	}
	if options.Endpoint != "" {
		study.Endpoints = []Reference{{Reference: options.Endpoint}}
	}
	if accession := dicomValue(first, tags.AccessionNumber); accession != "" && options.AccessionSystem != "" {
		study.Identifiers = append(study.Identifiers, Identifier{System: options.AccessionSystem, Value: accession})
	}

	seriesIndexes := make(map[string]int)
	instances := make(map[string]struct{}, len(datasets))
	modalities := make(map[string]struct{})
	var patient object.IDWithIssuer
	hasPatient := false
	for _, dataset := range datasets {
		if dataset == nil || dicomUID(dataset, tags.StudyInstanceUID) != studyUID || !unambiguousPatientID(dataset) {
			return ImagingStudy{}, ErrImagingIdentity
		}
		if latest, ok := object.PatientIDWithIssuer(dataset); ok {
			if hasPatient && patient != latest {
				return ImagingStudy{}, ErrImagingIdentity
			}
			patient, hasPatient = latest, true
		}
		seriesUID := dicomUID(dataset, tags.SeriesInstanceUID)
		instanceUID := dicomUID(dataset, tags.SOPInstanceUID)
		sopClass := dicomUID(dataset, tags.SOPClassUID)
		modality := dicomValue(dataset, tags.Modality)
		if !core.IsValidUID(seriesUID) || !core.IsValidUID(instanceUID) || !core.IsValidUID(sopClass) || !validModality(modality) {
			return ImagingStudy{}, ErrImagingIdentity
		}
		if _, duplicate := instances[instanceUID]; duplicate {
			return ImagingStudy{}, ErrImagingIdentity
		}
		instances[instanceUID] = struct{}{}

		index, found := seriesIndexes[seriesUID]
		if !found {
			index = len(study.Series)
			seriesIndexes[seriesUID] = index
			study.Series = append(study.Series, ImagingSeries{
				UID:         seriesUID,
				Number:      dicomNumber(dataset, tags.SeriesNumber),
				Modality:    Coding{System: dicomOntology, Code: modality},
				Started:     dicomStarted(dataset, tags.SeriesDate, tags.SeriesTime),
				Description: dicomValue(dataset, tags.SeriesDescription),
			})
		}
		series := &study.Series[index]
		if series.Modality.Code != modality {
			return ImagingStudy{}, ErrImagingIdentity
		}
		series.Instances = append(series.Instances, ImagingInstance{
			UID:      instanceUID,
			SOPClass: Coding{System: "urn:ietf:rfc:3986", Code: oidPrefix + sopClass},
			Number:   dicomNumber(dataset, tags.InstanceNumber),
		})
		modalities[modality] = struct{}{}
	}

	for code := range modalities {
		study.Modality = append(study.Modality, Coding{System: dicomOntology, Code: code})
	}
	slices.SortFunc(study.Modality, func(a, b Coding) int { return strings.Compare(a.Code, b.Code) })
	for i := range study.Series {
		series := &study.Series[i]
		series.NumberOfInstances = intPointer(len(series.Instances))
		slices.SortFunc(series.Instances, func(a, b ImagingInstance) int {
			return compareNumbered(a.Number, a.UID, b.Number, b.UID)
		})
	}
	slices.SortFunc(study.Series, func(a, b ImagingSeries) int {
		return compareNumbered(a.Number, a.UID, b.Number, b.UID)
	})
	study.NumberOfSeries = intPointer(len(study.Series))
	study.NumberOfInstances = intPointer(len(instances))
	return study, nil
}

func dicomValue(dataset *object.Object, tag core.Tag) string {
	value, ok := singleDICOMText(dataset, tag)
	if !ok || strings.ContainsRune(value, 0) {
		return ""
	}
	return strings.TrimSpace(value)
}

func dicomUID(dataset *object.Object, tag core.Tag) string {
	element, ok := dataset.Get(tag)
	if !ok || element.VR() != core.VRUI {
		return ""
	}
	value, ok := singleDICOMText(dataset, tag)
	if !ok {
		return ""
	}
	return core.NormalizeUID(value)
}

func singleDICOMText(dataset *object.Object, tag core.Tag) (string, bool) {
	if dataset.HasDuplicateParsedTag(tag) {
		return "", false
	}
	if !dataset.Has(tag) {
		return "", true
	}
	values, err := dataset.LookupStrings(tag)
	switch {
	case err != nil || len(values) > 1:
		return "", false
	case len(values) == 0:
		return "", true
	default:
		return values[0], true
	}
}

func unambiguousPatientID(dataset *object.Object) bool {
	for _, tag := range []core.Tag{tags.PatientID, core.NewTag(0x0010, 0x0021), core.NewTag(0x0010, 0x0022)} {
		value, ok := singleDICOMText(dataset, tag)
		if !ok || strings.ContainsRune(value, 0) {
			return false
		}
	}
	qualifiers := core.NewTag(0x0010, 0x0024)
	if !dataset.Has(qualifiers) {
		return true
	}
	items, ok := dataset.GetSequence(qualifiers)
	if !ok || len(items) > 1 || dataset.HasDuplicateParsedTag(qualifiers) {
		return false
	}
	if len(items) == 1 {
		if items[0] == nil {
			return false
		}
		for _, tag := range []core.Tag{core.NewTag(0x0040, 0x0032), core.NewTag(0x0040, 0x0033)} {
			value, ok := singleDICOMText(items[0], tag)
			if !ok || strings.ContainsRune(value, 0) {
				return false
			}
		}
	}
	return true
}

func validModality(value string) bool {
	if len(value) == 0 || len(value) > 16 {
		return false
	}
	for i := 0; i < len(value); i++ {
		b := value[i]
		if (b < 'A' || b > 'Z') && (b < '0' || b > '9') && b != '_' {
			return false
		}
	}
	return true
}

func dicomNumber(dataset *object.Object, tag core.Tag) *int {
	value, err := strconv.ParseUint(dicomValue(dataset, tag), 10, 31)
	if err != nil {
		return nil
	}
	return intPointer(int(value))
}

func intPointer(value int) *int { return &value }

func compareNumbered(left *int, leftUID string, right *int, rightUID string) int {
	switch {
	case left != nil && right == nil:
		return -1
	case left == nil && right != nil:
		return 1
	case left != nil && *left != *right:
		if *left < *right {
			return -1
		}
		return 1
	default:
		return strings.Compare(leftUID, rightUID)
	}
}

func dicomStarted(dataset *object.Object, dateTag, timeTag core.Tag) string {
	date, err := dcmtime.ParseDate(dicomValue(dataset, dateTag))
	if err != nil || len(date.DCM()) != 8 || date.Time.Year() < 1 {
		return ""
	}
	started := date.Time.Format("2006-01-02")
	clock := dicomValue(dataset, timeTag)
	offset := dicomValue(dataset, tags.TimezoneOffsetFromUTC)
	if len(clock) < 4 || len(offset) != 5 {
		return started
	}
	if _, err := dcmtime.ParseDatetime(date.DCM() + clock + offset); err != nil {
		return started
	}
	second := "00"
	if len(clock) >= 6 {
		second = clock[4:]
	}
	return started + "T" + clock[:2] + ":" + clock[2:4] + ":" + second + offset[:3] + ":" + offset[3:]
}

func validReference(value, resource string) bool {
	prefix, id, ok := strings.Cut(value, "/")
	return ok && prefix == resource && validResourceID(id)
}

func validResourceID(id string) bool {
	if len(id) == 0 || len(id) > 64 || id == "." || id == ".." {
		return false
	}
	for i := 0; i < len(id); i++ {
		b := id[i]
		if (b < 'a' || b > 'z') && (b < 'A' || b > 'Z') && (b < '0' || b > '9') && b != '-' && b != '.' {
			return false
		}
	}
	return true
}
