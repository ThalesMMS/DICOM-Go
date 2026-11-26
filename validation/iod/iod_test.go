package iod_test

import (
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/validation/iod"
)

func findings(t *testing.T, obj *object.Object, opts iod.Options) iod.Report {
	t.Helper()
	report, err := iod.Validate(obj, opts)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func hasFinding(report iod.Report, kind iod.Kind, path string) bool {
	for _, f := range report.Findings {
		if f.Kind == kind && f.Path == path {
			return true
		}
	}
	return false
}

// TestValidateWrittenObjects checks that the objects dicom-go writes meet
// their IODs.
func TestValidateWrittenObjects(t *testing.T) {
	want := map[string]string{"ct": "CT Image", "sc": "Secondary Capture Image", "gsps": "Grayscale Softcopy Presentation State", "kos": "Key Object Selection Document", "pdf": "Encapsulated PDF",
		"cda": "Encapsulated CDA", "stl": "Encapsulated STL", "obj": "Encapsulated OBJ", "mtl": "Encapsulated MTL"}
	for name, obj := range syntheticObjects(t) {
		report := findings(t, obj, iod.Options{})
		if report.IOD != want[name] {
			t.Errorf("%s: IOD %q, want %q", name, report.IOD, want[name])
		}
		if report.StandardVersion != iod.StandardVersion || report.StandardVersion == "" {
			t.Errorf("%s: standard version %q", name, report.StandardVersion)
		}
		for _, f := range report.Errors() {
			t.Errorf("%s: %s", name, f)
		}
	}
}

func TestValidateReportsMissingAndEmptyAttributes(t *testing.T) {
	ct := syntheticObjects(t)["ct"]

	withoutPatientID := object.FromElements(ct.Elements(), nil)
	withoutPatientID.Remove(core.NewTag(0x0010, 0x0020))
	if report := findings(t, withoutPatientID, iod.Options{}); !hasFinding(report, iod.MissingType2, "(0010,0020)") {
		t.Errorf("absent Patient ID not reported: %v", report.Findings)
	}

	withoutStudyUID := object.FromElements(ct.Elements(), nil)
	withoutStudyUID.Remove(core.NewTag(0x0020, 0x000D))
	if report := findings(t, withoutStudyUID, iod.Options{}); !hasFinding(report, iod.MissingType1, "(0020,000D)") {
		t.Errorf("absent Study Instance UID not reported: %v", report.Findings)
	}

	emptyStudyUID := object.FromElements(ct.Elements(), nil)
	emptyStudyUID.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0020, 0x000D), VR: core.VRUI}, Value: core.StringValue{}})
	report := findings(t, emptyStudyUID, iod.Options{})
	if !hasFinding(report, iod.EmptyType1, "(0020,000D)") {
		t.Errorf("empty Study Instance UID not reported: %v", report.Findings)
	}
	for _, f := range report.Findings {
		if f.Path == "(0020,000D)" && (f.Severity != iod.SeverityError || f.Module != "General Study") {
			t.Errorf("finding %+v", f)
		}
	}
}

// TestValidateChecksSequenceItems removes a Type 1 Attribute from an Item
// of the presentation state's Referenced Series Sequence.
func TestValidateChecksSequenceItems(t *testing.T) {
	gsps := syntheticObjects(t)["gsps"]
	referencedSeries := core.NewTag(0x0008, 0x1115)
	items, ok := gsps.GetSequence(referencedSeries)
	if !ok || len(items) == 0 {
		t.Fatal("no Referenced Series Sequence")
	}
	items[0].Remove(core.NewTag(0x0020, 0x000E))
	mutated := object.FromElements(gsps.Elements(), nil)
	mutated.Put(core.Element{Header: core.ElementHeader{Tag: referencedSeries, VR: core.VRSQ},
		Value: core.SequenceValue{Items: []core.DataSet{{Elements: items[0].Elements()}}}})
	if report := findings(t, mutated, iod.Options{}); !hasFinding(report, iod.MissingType1, "(0008,1115)[1]/(0020,000E)") {
		t.Errorf("absent Series Instance UID in the Item not reported: %v", report.Errors())
	}
}

func TestValidateModuleSelection(t *testing.T) {
	report := findings(t, syntheticObjects(t)["ct"], iod.Options{})
	checks := map[string]iod.ModuleCheck{}
	for _, m := range report.Modules {
		checks[m.Module] = m
	}
	for module, want := range map[string]bool{"Patient": true, "CT Image": true, "VOI LUT": true, "Contrast/Bolus": false} {
		check, ok := checks[module]
		if !ok {
			t.Errorf("module %s not listed", module)
			continue
		}
		if check.Checked != want {
			t.Errorf("module %s checked %v (%s), want %v", module, check.Checked, check.Reason, want)
		}
	}
}

func TestValidateOptionsAndErrors(t *testing.T) {
	ct := syntheticObjects(t)["ct"]
	if _, err := iod.Validate(nil, iod.Options{}); !errors.Is(err, iod.ErrUnknownIOD) {
		t.Errorf("nil data set: %v", err)
	}
	unknown := object.FromElements(ct.Elements(), nil)
	unknown.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{"1.2.3.4"}})
	if _, err := iod.Validate(unknown, iod.Options{}); !errors.Is(err, iod.ErrUnknownIOD) {
		t.Errorf("unknown SOP Class: %v", err)
	}
	if _, err := iod.Validate(ct, iod.Options{IOD: "No Such"}); !errors.Is(err, iod.ErrUnknownIOD) {
		t.Errorf("unknown IOD name: %v", err)
	}
	// The CT validated as an MR Image lacks the MR Image Module.
	mr := findings(t, ct, iod.Options{IOD: "MR Image"})
	if len(mr.Errors()) == 0 {
		t.Error("CT meets the MR Image IOD")
	}
	limited := findings(t, ct, iod.Options{IOD: "MR Image", MaxFindings: 1})
	if len(limited.Findings) != 1 || limited.Truncated != len(mr.Findings)-1 {
		t.Errorf("MaxFindings 1: %d findings, %d truncated of %d", len(limited.Findings), limited.Truncated, len(mr.Findings))
	}
}

func TestIODForSOPClass(t *testing.T) {
	for uid, want := range map[string]string{
		"1.2.840.10008.5.1.4.1.1.2":     "CT Image",
		"1.2.840.10008.5.1.4.1.1.7":     "Secondary Capture Image",
		"1.2.840.10008.5.1.4.1.1.11.1":  "Grayscale Softcopy Presentation State",
		"1.2.840.10008.5.1.4.1.1.88.59": "Key Object Selection Document",
		"1.2.840.10008.5.1.4.1.1.66.4":  "Segmentation",
	} {
		if got, ok := iod.IODForSOPClass(uid); !ok || got != want {
			t.Errorf("%s: %q %v, want %q", uid, got, ok, want)
		}
	}
	if _, ok := iod.IODForSOPClass("1.2.3"); ok {
		t.Error("unknown SOP Class mapped")
	}
}
