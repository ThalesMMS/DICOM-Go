package attrpath

import (
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

// dcmdata/tests/tcond.cc checks ordering and equality for several textual VRs.
// Run the same operator table on decoded and encoded values, with operands
// of different widths for the documented lexical ordering of IS and DS.
func TestDCMTKReferenceLexicalConditionOperators(t *testing.T) {
	for _, key := range []struct {
		path                string
		vr                  core.VR
		lower, value, upper string
	}{
		{"Modality", core.VRCS, "CT", "MR", "US"},
		{"Manufacturer", core.VRLO, "Adams", "Middle Clinic", "Zimmerman"},
		{"PatientName", core.VRPN, "Adams^Lee", "Brown^Lee", "Zimmerman^Lee"},
		{"StudyDate", core.VRDA, "20231231", "20240101", "20240102"},
		{"WindowCenter", core.VRDS, "100.0", "100.5", "50.0"},
		{"NumberOfFrames", core.VRIS, "09", "10", "5"},
	} {
		t.Run(key.path, func(t *testing.T) {
			wire := []byte(key.value)
			if len(wire)%2 != 0 {
				wire = append(wire, ' ')
			}
			for _, form := range []struct {
				name  string
				value core.Value
			}{
				{"decoded", core.StringValue{key.value}},
				{"encoded", core.RawValue(wire)},
			} {
				t.Run(form.name, func(t *testing.T) {
					tag := MustParse(key.path)[0].Tag
					dataset := object.FromElements([]core.Element{dccondElement(tag, key.vr, form.value)}, std.Dictionary)
					session := dccondSession(t, dataset, EditLimits{})
					for _, tc := range []struct {
						comparison string
						want       bool
					}{
						{"=" + key.value, true},
						{"=" + key.lower, false},
						{"!=" + key.value, false},
						{"!=" + key.upper, true},
						{"<" + key.upper, true},
						{"<" + key.value, false},
						{"<=" + key.value, true},
						{"<=" + key.lower, false},
						{">" + key.lower, true},
						{">" + key.value, false},
						{">=" + key.value, true},
						{">=" + key.upper, false},
					} {
						dccondEvaluate(t, session, key.path+tc.comparison, tc.want)
					}
				})
			}
		})
	}
}
