package object

import (
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
)

func identityObject(elements ...core.Element) *Object {
	base := []core.Element{
		newStringElement(tagSOPClassUID, core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		newStringElement(tagSOPInstanceUID, core.VRUI, "1.2.826.0.1.3680043.10.1206.1"),
	}
	return FromElements(append(base, elements...), std.Dictionary)
}

func qualifiers(universalID string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tagIssuerOfPatientIDQualifiersSeq, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{
		newStringElement(tagIssuerQualifierUniversalID, core.VRUT, universalID),
		newStringElement(tagIssuerQualifierUniversalIDType, core.VRCS, "ISO"),
	}}}}}
}

func TestCopyPatientIdentity(t *testing.T) {
	for _, tt := range []struct {
		name         string
		source       *Object
		target       *Object
		wantModified []core.Tag
		wantValues   map[core.Tag]string
		wantAbsent   []core.Tag
		wantPresent  []core.Tag
	}{
		{
			name: "copies every identity attribute",
			source: identityObject(
				newStringElement(tagPatientsName, core.VRPN, "RIGHT^NAME"),
				newStringElement(tagPatientIDValue, core.VRLO, "ID-R"),
				newStringElement(tagIssuerOfPatientID, core.VRLO, "SITE-A"),
				newStringElement(tagPatientsBirthDate, core.VRDA, "19800101"),
				newStringElement(tagPatientsSex, core.VRCS, "F"),
			),
			target: identityObject(
				newStringElement(tagPatientsName, core.VRPN, "WRONG^NAME"),
				newStringElement(tagPatientIDValue, core.VRLO, "ID-W"),
				newStringElement(tagPatientsBirthDate, core.VRDA, "19800101"),
			),
			wantModified: []core.Tag{tagPatientsName, tagPatientIDValue, tagIssuerOfPatientID, tagPatientsSex},
			wantValues: map[core.Tag]string{
				tagPatientsName: "RIGHT^NAME", tagPatientIDValue: "ID-R", tagIssuerOfPatientID: "SITE-A",
				tagPatientsBirthDate: "19800101", tagPatientsSex: "F",
			},
		},
		{
			name:   "type 2 attributes missing from the source become empty, issuer is removed",
			source: identityObject(newStringElement(tagPatientsName, core.VRPN, "RIGHT^NAME"), newStringElement(tagPatientIDValue, core.VRLO, "ID-R")),
			target: identityObject(
				newStringElement(tagPatientsName, core.VRPN, "WRONG^NAME"),
				newStringElement(tagPatientIDValue, core.VRLO, "ID-W"),
				newStringElement(tagIssuerOfPatientID, core.VRLO, "SITE-W"),
				qualifiers("1.2.3"),
				newStringElement(tagPatientsBirthDate, core.VRDA, "19700101"),
				newStringElement(tagPatientsSex, core.VRCS, "M"),
			),
			wantModified: []core.Tag{tagPatientsName, tagPatientIDValue, tagIssuerOfPatientID, tagIssuerOfPatientIDQualifiersSeq, tagPatientsBirthDate, tagPatientsSex},
			wantValues:   map[core.Tag]string{tagPatientsName: "RIGHT^NAME", tagPatientsBirthDate: "", tagPatientsSex: ""},
			wantAbsent:   []core.Tag{tagIssuerOfPatientID, tagIssuerOfPatientIDQualifiersSeq},
		},
		{
			name:   "qualifiers survive when the issuer does not change",
			source: identityObject(newStringElement(tagPatientIDValue, core.VRLO, "ID-R"), newStringElement(tagIssuerOfPatientID, core.VRLO, "SITE-A")),
			target: identityObject(
				newStringElement(tagPatientIDValue, core.VRLO, "ID-W"),
				newStringElement(tagIssuerOfPatientID, core.VRLO, "SITE-A"),
				qualifiers("1.2.3"),
			),
			wantModified: []core.Tag{tagPatientIDValue},
			wantPresent:  []core.Tag{tagIssuerOfPatientIDQualifiersSeq},
		},
		{
			name:   "identical identity changes nothing",
			source: identityObject(newStringElement(tagPatientsName, core.VRPN, "SAME^NAME"), newStringElement(tagPatientIDValue, core.VRLO, "ID-S")),
			target: identityObject(newStringElement(tagPatientsName, core.VRPN, "SAME^NAME"), newStringElement(tagPatientIDValue, core.VRLO, "ID-S")),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			modified, err := CoerceAttributes(tt.target, OriginalAttributesRecord{Reason: ModificationCorrect, System: "Twin Viewer"}, CopyPatientIdentity{Source: tt.source})
			if err != nil {
				t.Fatal(err)
			}
			if !equalTags(elementTags(modified), tt.wantModified) {
				t.Fatalf("modified = %v, want %v", elementTags(modified), tt.wantModified)
			}
			for tag, want := range tt.wantValues {
				element, ok := tt.target.Get(tag)
				if !ok {
					t.Fatalf("%s missing", tag)
				}
				got := ""
				if values := element.StringValues(); len(values) > 0 {
					got = values[0]
				}
				if got != want {
					t.Fatalf("%s = %q, want %q", tag, got, want)
				}
			}
			for _, tag := range tt.wantAbsent {
				if tt.target.Has(tag) {
					t.Fatalf("%s still present", tag)
				}
			}
			for _, tag := range tt.wantPresent {
				if !tt.target.Has(tag) {
					t.Fatalf("%s removed", tag)
				}
			}
			if recorded := tt.target.Has(tagOriginalAttributesSequence); recorded != (len(tt.wantModified) > 0) {
				t.Fatalf("Original Attributes recorded = %v, want %v", recorded, len(tt.wantModified) > 0)
			}
		})
	}
}

func TestCopyPatientIdentityRefusesTextOutsideTheTargetCharacterSet(t *testing.T) {
	source := identityObject(
		newStringElement(tagSpecificCharacterSet, core.VRCS, "ISO_IR 192"),
		newStringElement(tagPatientsName, core.VRPN, "JOSÉ^SILVA"),
	)
	target := identityObject(newStringElement(tagPatientsName, core.VRPN, "JOSE^SILVA"))
	if _, err := (CopyPatientIdentity{Source: source}).Coerce(target); !errors.Is(err, ErrPatientIdentityCharacterSet) {
		t.Fatalf("err = %v, want ErrPatientIdentityCharacterSet", err)
	}
	if got, _ := target.GetString(tagPatientsName); got != "JOSE^SILVA" {
		t.Fatalf("refused copy changed the name to %q", got)
	}

	ascii := identityObject(
		newStringElement(tagSpecificCharacterSet, core.VRCS, "ISO_IR 192"),
		newStringElement(tagPatientsName, core.VRPN, "JOSE^SILVA"),
	)
	if _, err := (CopyPatientIdentity{Source: ascii}).Coerce(identityObject()); err != nil {
		t.Fatalf("plain ASCII identity refused: %v", err)
	}
}
