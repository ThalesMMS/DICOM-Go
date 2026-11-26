package dimse

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestStudyRootCFindMatchesEachPatientNameInItsOwnCharacterSet(t *testing.T) {
	charset := func(value string) core.Element {
		return core.Element{
			Header: core.ElementHeader{Tag: tagMWLSpecificCharacterSet, VR: core.VRCS},
			Value:  core.StringValue{value},
		}
	}
	var idx MemoryFindIndex
	for _, tc := range []struct{ charset, uid, name string }{
		{"ISO_IR 100", "1.2.3.41", "Érable^Célia"},
		{"ISO_IR 192", "1.2.3.42", "Érable^Célia"},
		{"ISO_IR 192", "1.2.3.43", "Erable^Célia"},
	} {
		dataset := object.FromElements([]core.Element{
			charset(tc.charset),
			{Header: core.ElementHeader{Tag: tags.PatientName, VR: core.VRPN}, Value: core.StringValue{tc.name}},
			{Header: core.ElementHeader{Tag: tags.StudyInstanceUID, VR: core.VRUI}, Value: core.StringValue{tc.uid}},
		}, std.Dictionary)
		var encoded bytes.Buffer
		if err := object.WriteDataSet(&encoded, dataset, transfer.ExplicitVRLittleEndian); err != nil {
			t.Fatal(err)
		}
		candidate, err := object.ReadDataSet(&encoded, transfer.ExplicitVRLittleEndian)
		if err != nil {
			t.Fatal(err)
		}
		idx.Candidates = append(idx.Candidates, candidate)
	}
	for _, encoding := range []string{"ISO_IR 100", "ISO_IR 192"} {
		for _, matching := range []struct{ pattern, name string }{
			{"érable^célia", "literal"},
			{"Érable^*", "star wildcard"},
			{"Érable^C?lia", "single character wildcard"},
		} {
			t.Run(encoding+"/"+matching.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				identifier := mustBuild(t, func() ([]core.Element, error) {
					return BuildStudyRootStudyFindKeys(map[string]string{"PatientName": matching.pattern}, "StudyInstanceUID")
				})
				identifier = append(identifier, charset(encoding))
				var got []string
				for _, item := range runStudyRootFindAgainstIndex(t, ctx, idx, identifier) {
					uid, _ := item.GetString(tags.StudyInstanceUID)
					name, _ := item.GetString(tags.PatientName)
					if name != "Érable^Célia" {
						t.Fatalf("candidate text = %q, want original decoded Patient Name", name)
					}
					got = append(got, uid)
				}
				if want := []string{"1.2.3.41", "1.2.3.42"}; !slices.Equal(got, want) {
					t.Fatalf("matching UIDs = %v, want %v", got, want)
				}
			})
		}
	}
}
