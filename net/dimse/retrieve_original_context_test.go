package dimse

import (
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestCGetAlternativeStorageContexts(t *testing.T) {
	const ct, mr = "1.2.840.10008.5.1.4.1.1.2", "1.2.840.10008.5.1.4.1.1.4"
	classes := []string{ct, mr}
	native := []string{ul.ImplicitVRLittleEndian, ul.ExplicitVRLittleEndian}
	alternatives := []string{transfer.JPEGBaseline.UID, transfer.JPEGLSLossless.UID, transfer.JPEG2000LosslessOnly.UID}
	contexts, roles, err := BuildCGetPresentationContextsWithAlternatives(QueryRetrieveModelStudyRoot, classes, native, alternatives)
	if err != nil {
		t.Fatal(err)
	}
	if len(contexts) != 9 || contexts[0].AbstractSyntaxUID != StudyRootGetSOPClassUID || len(roles) != 2 {
		t.Fatalf("contexts=%v roles=%v", contexts, roles)
	}
	for i, class := range classes {
		if role := roles[i]; role.SopClassUID != class || role.SCURole || !role.SCPRole {
			t.Fatalf("role %d = %+v", i, role)
		}
		for j, pc := range contexts[1+i*4 : 1+(i+1)*4] {
			want := native
			if j < len(alternatives) {
				want = []string{alternatives[j]}
			}
			if pc.AbstractSyntaxUID != class || !slices.Equal(pc.TransferSyntaxUIDs, want) {
				t.Fatalf("class %s context %d = %+v, want %v", class, j, pc, want)
			}
		}
	}
	if _, _, err := BuildCGetPresentationContextsWithAlternatives(QueryRetrieveModelStudyRoot, classes, native, native); err == nil {
		t.Fatal("accepted duplicate native alternatives")
	}
	if _, _, err := BuildCGetPresentationContextsWithAlternatives(QueryRetrieveModelStudyRoot, DefaultCGetStorageSOPClassUIDs(), native, alternatives); err == nil {
		t.Fatal("accepted storage contexts over capacity")
	}
}

func TestCGetInventoryRequiresExactFindHierarchy(t *testing.T) {
	const study, series, instance, class = "1.2.3", "1.2.4", "1.2.5", "1.2.840.10008.5.1.4.1.1.2"
	keys := []core.Element{
		uiElement(tagStudyInstanceUID, study), uiElement(tagSeriesInstanceUID, series),
		uiElement(tagSOPClassUID), uiElement(tagSOPInstanceUID),
	}
	for _, tc := range []struct {
		name string
		tag  core.Tag
		want bool
		edit core.Element
	}{
		{name: "matching", want: true},
		{name: "absent class", tag: tagSOPClassUID},
		{name: "absent level", tag: tagQueryRetrieveLevel},
		{name: "other study", tag: tagStudyInstanceUID, edit: uiElement(tagStudyInstanceUID, "1.2.9")},
		{name: "multiple series", tag: tagSeriesInstanceUID, edit: uiElement(tagSeriesInstanceUID, series, "1.2.9")},
		{name: "wrong instance VR", tag: tagSOPInstanceUID, edit: loElement(tagSOPInstanceUID, instance)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := object.FromElements([]core.Element{
				csElement(tagQueryRetrieveLevel, QueryRetrieveLevelImage),
				uiElement(tagStudyInstanceUID, study), uiElement(tagSeriesInstanceUID, series),
				uiElement(tagSOPClassUID, class), uiElement(tagSOPInstanceUID, instance),
			}, std.Dictionary)
			if !tc.want {
				response.Remove(tc.tag)
				if tc.edit.Value != nil {
					response.Put(tc.edit)
				}
			}
			if got := validRetrieveInventoryMatch(response, QueryRetrieveLevelImage, keys); got != tc.want {
				t.Fatalf("valid = %t, want %t", got, tc.want)
			}
		})
	}
}
