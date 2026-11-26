package dimse

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/net/ul"
)

func TestBuildCGetPresentationContextsPreservesPreferences(t *testing.T) {
	for _, test := range []struct {
		model    QueryRetrieveModel
		getClass string
	}{
		{QueryRetrieveModelStudyRoot, StudyRootGetSOPClassUID},
		{QueryRetrieveModelPatientRoot, PatientRootGetSOPClassUID},
	} {
		t.Run(string(test.model), func(t *testing.T) {
			const privateStorageClass = "1.2.826.0.1.9001"
			classes := []string{privateStorageClass, cGetTestStorageSOPClassUID}
			syntaxes := []string{ul.ExplicitVRLittleEndian, ul.ImplicitVRLittleEndian}
			contexts, roles, err := BuildCGetPresentationContexts(test.model, classes, syntaxes)
			if err != nil {
				t.Fatal(err)
			}
			if len(contexts) != 3 || len(roles) != 2 {
				t.Fatalf("contexts=%d roles=%d", len(contexts), len(roles))
			}
			if contexts[0].AbstractSyntaxUID != test.getClass ||
				!slices.Equal(contexts[0].TransferSyntaxUIDs, []string{ul.ImplicitVRLittleEndian, ul.ExplicitVRLittleEndian}) {
				t.Fatalf("GET context = %+v", contexts[0])
			}
			for i, classUID := range classes {
				context := contexts[i+1]
				if context.AbstractSyntaxUID != classUID || !slices.Equal(context.TransferSyntaxUIDs, syntaxes) {
					t.Errorf("storage context %d = %+v", i, context)
				}
				if want := (ul.RoleSelectionItem{SopClassUID: classUID, SCPRole: true}); roles[i] != want {
					t.Errorf("storage role %d = %+v, want %+v", i, roles[i], want)
				}
			}

			classes[0], syntaxes[0] = "", ""
			contexts[2].TransferSyntaxUIDs[0] = ""
			if contexts[1].AbstractSyntaxUID != privateStorageClass ||
				contexts[1].TransferSyntaxUIDs[0] != ul.ExplicitVRLittleEndian {
				t.Fatal("proposals share input or context state")
			}
		})
	}
}

func TestBuildCGetPresentationContextsRejectsInvalidOffers(t *testing.T) {
	validClass := []string{cGetTestStorageSOPClassUID}
	validSyntax := []string{ul.ExplicitVRLittleEndian}
	oversizedSyntaxes := make([]string, 1024)
	for i := range oversizedSyntaxes {
		oversizedSyntaxes[i] = fmt.Sprintf("1.2.826.%s.%d", strings.Repeat("9", 52), i)
	}
	for _, test := range []struct {
		name     string
		model    QueryRetrieveModel
		classes  []string
		syntaxes []string
	}{
		{"model", "", validClass, validSyntax},
		{"no storage", QueryRetrieveModelStudyRoot, nil, validSyntax},
		{"invalid storage UID", QueryRetrieveModelStudyRoot, []string{"1.2.03"}, validSyntax},
		{"non-storage SOP", QueryRetrieveModelStudyRoot, []string{PatientRootGetSOPClassUID}, validSyntax},
		{"duplicate storage", QueryRetrieveModelStudyRoot, []string{cGetTestStorageSOPClassUID, cGetTestStorageSOPClassUID}, validSyntax},
		{"no syntax", QueryRetrieveModelStudyRoot, validClass, nil},
		{"invalid syntax UID", QueryRetrieveModelStudyRoot, validClass, []string{" 1.2.3"}},
		{"non-syntax UID", QueryRetrieveModelStudyRoot, validClass, validClass},
		{"duplicate syntax", QueryRetrieveModelStudyRoot, validClass, []string{ul.ExplicitVRLittleEndian, ul.ExplicitVRLittleEndian}},
		{"oversized syntax item", QueryRetrieveModelStudyRoot, validClass, oversizedSyntaxes},
	} {
		t.Run(test.name, func(t *testing.T) {
			contexts, roles, err := BuildCGetPresentationContexts(test.model, test.classes, test.syntaxes)
			if err == nil || contexts != nil || roles != nil {
				t.Fatalf("contexts=%v roles=%v error=%v, want no proposals and an error", contexts, roles, err)
			}
		})
	}
}

func TestBuildCGetPresentationContextsCapacity(t *testing.T) {
	classes := make([]string, ul.MaxPresentationContexts)
	for i := range classes {
		classes[i] = fmt.Sprintf("1.2.826.0.1.9001.%d", i)
	}
	syntaxes := []string{ul.ImplicitVRLittleEndian}
	contexts, roles, err := BuildCGetPresentationContexts(QueryRetrieveModelStudyRoot, classes[:len(classes)-1], syntaxes)
	if err != nil {
		t.Fatal(err)
	}
	if len(contexts) != ul.MaxPresentationContexts || len(roles) != ul.MaxPresentationContexts-1 {
		t.Fatalf("contexts=%d roles=%d", len(contexts), len(roles))
	}
	if _, _, err := BuildCGetPresentationContexts(QueryRetrieveModelStudyRoot, classes, syntaxes); err == nil {
		t.Fatal("more than 128 contexts accepted")
	}
}
