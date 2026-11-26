package fhir

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/object"
)

const studyUID = "1.2.826.0.1.1"

func TestMapImagingStudyPreservesIndependentOffsets(t *testing.T) {
	serialThree := imagingDataset("1.2.826.0.1.3", "1.2.826.0.1.339", "2", "1", "MR", "+0530")
	serialTwo := imagingDataset("1.2.826.0.1.2", "1.2.826.0.1.229", "1", "2", "CT", "-0300")
	serialOne := imagingDataset("1.2.826.0.1.2", "1.2.826.0.1.228", "1", "1", "CT", "-0300")
	putText(serialThree, tags.StudyDescription, core.VRLO, "Synthetic radiology")
	putText(serialThree, tags.AccessionNumber, core.VRSH, "ORDER-7")
	options := ImagingStudyOptions{
		ID: "study-a", Subject: "Patient/patient-a", Endpoint: "Endpoint/wado-a", AccessionSystem: "https://example.test/accession",
	}
	got, err := MapImagingStudy([]*object.Object{serialThree, serialTwo, serialOne}, options)
	if err != nil {
		t.Fatal(err)
	}
	uid, ok := got.DICOMStudyInstanceUID()
	if !ok || uid != studyUID || got.Status != "available" || got.Subject.Reference != options.Subject ||
		got.Started != "2026-09-12T10:15:00+05:30" || got.NumberOfSeries == nil || *got.NumberOfSeries != 2 ||
		got.NumberOfInstances == nil || *got.NumberOfInstances != 3 ||
		len(got.Identifiers) != 2 || got.Identifiers[1].Value != "ORDER-7" ||
		len(got.Modality) != 2 || got.Modality[0].Code != "CT" || got.Modality[1].Code != "MR" {
		t.Fatalf("study = %+v", got)
	}
	firstSeries := got.Series[0]
	if firstSeries.UID != "1.2.826.0.1.2" || firstSeries.Started != "2026-09-12T11:15:30.120-03:00" ||
		got.Series[1].Started != "2026-09-12T11:15:30.120+05:30" ||
		len(firstSeries.Instances) != 2 || firstSeries.Instances[0].UID != "1.2.826.0.1.228" ||
		firstSeries.Instances[1].UID != "1.2.826.0.1.229" ||
		firstSeries.Instances[0].SOPClass.Code != "urn:oid:1.2.840.10008.5.1.4.1.1.2" {
		t.Fatalf("series = %+v", got.Series)
	}
	wire, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		`"resourceType":"ImagingStudy"`, `"reference":"Endpoint/wado-a"`,
		`"value":"urn:oid:1.2.826.0.1.1"`, `"started":"2026-09-12T11:15:30.120-03:00"`,
	} {
		if !strings.Contains(string(wire), field) {
			t.Errorf("projection missing %s", field)
		}
	}
}

func TestMapImagingStudyOmitsUnknownTimeInsteadOfAssumingOffset(t *testing.T) {
	for _, options := range []struct {
		name, offset, clock, want string
	}{
		{"absent offset", "", "101500", "2026-09-12"},
		{"malformed timezone", "0300", "101500", "2026-09-12"},
		{"DICOM negative bound", "-1230", "101500", "2026-09-12"},
		{"illegal offset", "+1460", "101500", "2026-09-12"},
		{"negative zero offset", "-0000", "101500", "2026-09-12"},
		{"missing minutes", "+0000", "10", "2026-09-12"},
		{"missing seconds", "+0000", "1015", "2026-09-12T10:15:00+00:00"},
		{"leap second", "+0000", "235960", "2026-09-12T23:59:60+00:00"},
	} {
		t.Run(options.name, func(t *testing.T) {
			obj := imagingDataset("1.2.826.0.1.2", "1.2.826.0.1.9", "1", "1", "CT", options.offset)
			putText(obj, tags.StudyTime, core.VRTM, options.clock)
			got, err := MapImagingStudy([]*object.Object{obj}, ImagingStudyOptions{Subject: "Patient/synthetic"})
			if err != nil || got.Started != options.want || !validDateTime(got.Started) {
				t.Fatalf("started = %q, err=%v, want=%q", got.Started, err, options.want)
			}
		})
	}
}

func TestMapImagingStudyRejectsConflictingIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*object.Object)
	}{
		{"another study", func(p *object.Object) { putText(p, tags.StudyInstanceUID, core.VRUI, "1.2.826.0.1.999") }},
		{"duplicate SOP", func(p *object.Object) { putText(p, tags.SOPInstanceUID, core.VRUI, "1.2.826.0.1.9") }},
		{"issuer mismatch", func(p *object.Object) { putText(p, core.NewTag(0x0010, 0x0021), core.VRLO, "OTHER") }},
		{"patient VM mismatch", func(p *object.Object) { putText(p, tags.PatientID, core.VRLO, "PAT\\OTHER") }},
		{"wrong UID VR", func(p *object.Object) { putText(p, tags.SeriesInstanceUID, core.VRLO, "1.2.826.0.1.2") }},
		{"malformed UID", func(p *object.Object) { putText(p, tags.SeriesInstanceUID, core.VRUI, "1.2.826.\x000.1.2") }},
		{"modality disagreement", func(p *object.Object) { putText(p, tags.Modality, core.VRCS, "MR") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := imagingDataset("1.2.826.0.1.2", "1.2.826.0.1.9", "1", "1", "CT", "+0100")
			second := imagingDataset("1.2.826.0.1.2", "1.2.826.0.1.10", "1", "2", "CT", "+0100")
			tc.mutate(second)
			got, err := MapImagingStudy([]*object.Object{first, second}, ImagingStudyOptions{Subject: "Patient/synthetic"})
			if !errors.Is(err, ErrImagingIdentity) || len(got.Series) != 0 {
				t.Fatalf("study = %+v, error=%v", got, err)
			}
		})
	}
}

func TestDICOMStudyInstanceUIDRejectsAmbiguity(t *testing.T) {
	for _, other := range []string{"1.2.3", "urn:oid:1.02.3", "urn:oid:1.2.4"} {
		study := ImagingStudy{Identifiers: []Identifier{
			{System: dicomUIDSystem, Value: "urn:oid:1.2.3"},
			{System: "https://example.test", Value: "other"},
			{System: dicomUIDSystem, Value: other},
		}}
		if got, ok := study.DICOMStudyInstanceUID(); ok || got != "" {
			t.Fatalf("got ambiguous UID %q from %q", got, other)
		}
	}
}

func imagingDataset(series, instance, seriesNumber, instanceNumber, modality, timezone string) *object.Object {
	obj := object.New(std.Dictionary)
	putText(obj, tags.StudyInstanceUID, core.VRUI, studyUID)
	putText(obj, tags.SeriesInstanceUID, core.VRUI, series)
	putText(obj, tags.SOPInstanceUID, core.VRUI, instance)
	putText(obj, tags.SOPClassUID, core.VRUI, "1.2.840.10008.5.1.4.1.1.2")
	putText(obj, tags.PatientID, core.VRLO, "PAT")
	putText(obj, core.NewTag(0x0010, 0x0021), core.VRLO, "ISSUER")
	putText(obj, tags.Modality, core.VRCS, modality)
	putText(obj, tags.SeriesNumber, core.VRIS, seriesNumber)
	putText(obj, tags.InstanceNumber, core.VRIS, instanceNumber)
	putText(obj, tags.StudyDate, core.VRDA, "20260912")
	putText(obj, tags.StudyTime, core.VRTM, "101500")
	putText(obj, tags.SeriesDate, core.VRDA, "20260912")
	putText(obj, tags.SeriesTime, core.VRTM, "111530.120")
	putText(obj, tags.TimezoneOffsetFromUTC, core.VRSH, timezone)
	return obj
}

func putText(obj *object.Object, tag core.Tag, vr core.VR, value string) {
	obj.Put(core.NewRawElement(tag, vr, []byte(value)))
}
