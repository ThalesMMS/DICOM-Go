package iod_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/validation/iod"
)

// dcmpschkPasses runs dcmpschk, which exits 0 either way and reports the
// outcome as "Test passed." or "Test failed".
func dcmpschkPasses(t *testing.T, tool, path string) (bool, string) {
	t.Helper()
	out := dcmtkoracle.Run(t, dcmtkoracle.Accepted, tool, path)
	return bytes.Contains(out, []byte("Test passed.")), string(out)
}

// TestValidateAgreesWithDCMPSCHK checks the presentation state dicom-go
// writes and copies with one top-level Attribute removed: dcmpschk accepts
// the state as written, and Validate reports the problem whenever dcmpschk
// rejects a copy: an error, or a warning of a Module inferred from its other
// Attributes (Spatial Transformation without Image Rotation). Validate also
// reports absent Type 2 Attributes that dcmpschk does not check.
func TestValidateAgreesWithDCMPSCHK(t *testing.T) {
	dcmtkoracle.Require(t)
	tool := dcmtkoracle.Tool(t, "dcmpschk")
	dir := t.TempDir()
	gsps := syntheticObjects(t)["gsps"]
	if ok, out := dcmpschkPasses(t, tool, writeObject(t, dir, "gsps", gsps)); !ok {
		t.Fatalf("dcmpschk rejects the presentation state:\n%s", out)
	}
	rejected := 0
	for _, element := range gsps.Elements() {
		tag := element.Header.Tag
		if tag.Group == 0x0008 && (tag.Element == 0x0005 || tag.Element == 0x0016 || tag.Element == 0x0018) {
			continue
		}
		mutated := object.FromElements(gsps.Elements(), nil)
		mutated.Remove(tag)
		label := "gsps-without-" + strings.Trim(tag.String(), "()")
		ok, out := dcmpschkPasses(t, tool, writeObject(t, dir, label, mutated))
		if ok {
			continue
		}
		rejected++
		report, err := iod.Validate(mutated, iod.Options{})
		if err != nil {
			t.Fatal(err)
		}
		reported := false
		for _, f := range report.Findings {
			reported = reported || f.Severity != iod.SeverityInfo
		}
		if !reported {
			t.Errorf("%s: dcmpschk rejects it, Validate reports nothing:\n%s", label, out)
		}
	}
	if rejected == 0 {
		t.Error("dcmpschk rejected no copy")
	}
}
