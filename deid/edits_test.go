package deid

import (
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/attrpath"
	"github.com/ThalesMMS/dicom-go/core"
)

func TestAnonymizeObjectRunsEditScriptAfterProfile(t *testing.T) {
	script, err := attrpath.ParseScript("insert PatientName=SUBJECT-007\ninsert ClinicalTrialSubjectID=007\ninsert ReferencedStudySequence[0].ReferencedSOPClassUID=1.2.840.10008.3.1.2.3.1\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	obj := deidTestObject()
	report, err := AnonymizeObjectWithReport(obj, Options{Edits: script}, NewUIDRemapper())
	if err != nil {
		t.Fatal(err)
	}
	if name, _ := obj.GetString(testTagPatientName); name != "SUBJECT-007" {
		t.Fatalf("PatientName = %q, want the scripted pseudonym", name)
	}
	if id, _ := obj.GetString(core.NewTag(0x0012, 0x0040)); id != "007" || report.EditedAttributes != 3 {
		t.Fatalf("subject %q, edited %d", id, report.EditedAttributes)
	}
	failing, _ := attrpath.ParseScript("modify PatientComments=x\n", nil)
	if _, err := AnonymizeObjectWithReport(deidTestObject(), Options{Edits: failing}, NewUIDRemapper()); !errors.Is(err, attrpath.ErrNotFound) {
		t.Fatalf("failing edit: %v", err)
	}
}
