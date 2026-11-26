package dimse

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/object"
)

func TestMemoryCFindExtendedValueMatchingNegotiation(t *testing.T) {
	candidate := func(uid string, modalities []string, accession *string) *object.Object {
		obj := object.FromElements([]core.Element{
			{Header: core.ElementHeader{Tag: tags.StudyInstanceUID, VR: core.VRUI}, Value: core.StringValue{uid}},
			{Header: core.ElementHeader{Tag: tags.ModalitiesInStudy, VR: core.VRCS}, Value: core.StringValue(modalities)},
		}, std.Dictionary)
		if accession != nil {
			obj.Put(core.Element{Header: core.ElementHeader{Tag: tags.AccessionNumber, VR: core.VRSH}, Value: core.StringValue{*accession}})
		}
		return obj
	}
	quoted, empty := `""`, ""
	index := MemoryFindIndex{Candidates: []*object.Object{
		candidate("1.2.3.1", []string{"CT"}, &quoted),
		candidate("1.2.3.2", []string{"MR", "PT", "CT"}, nil),
		candidate("1.2.3.3", []string{"MR"}, &empty),
	}}
	modalities := core.Element{Header: core.ElementHeader{Tag: tags.ModalitiesInStudy, VR: core.VRCS}, Value: core.StringValue{"CT", "MR"}}
	accession := core.Element{Header: core.ElementHeader{Tag: tags.AccessionNumber, VR: core.VRSH}, Value: core.StringValue{quoted}}
	support := CFindExtendedNegotiation{EmptyValueMatching: true, MultipleValueMatching: true}
	emptyOnly := CFindExtendedNegotiation{EmptyValueMatching: true}
	valuesOnly := CFindExtendedNegotiation{MultipleValueMatching: true}
	for _, tc := range []struct {
		name     string
		support  *CFindExtendedNegotiation
		request  *CFindExtendedNegotiation
		query    core.Element
		accepted CFindExtendedNegotiation
		wantUIDs []string
	}{
		{"legacy list", &support, nil, modalities, CFindExtendedNegotiation{}, []string{"1.2.3.1", "1.2.3.2", "1.2.3.3"}},
		{"refused list", &emptyOnly, &support, modalities, emptyOnly, []string{"1.2.3.1", "1.2.3.2", "1.2.3.3"}},
		{"accepted list", &support, &support, modalities, support, []string{"1.2.3.2"}},
		{"legacy quoted literal", &support, nil, accession, CFindExtendedNegotiation{}, []string{"1.2.3.1"}},
		{"refused empty", &valuesOnly, &support, accession, valuesOnly, []string{"1.2.3.1"}},
		{"accepted empty", &support, &support, accession, support, []string{"1.2.3.2", "1.2.3.3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			keys, err := BuildStudyRootStudyFindKeys(nil, "StudyInstanceUID", "ModalitiesInStudy", "AccessionNumber")
			if err != nil {
				t.Fatal(err)
			}
			identifier := object.FromElements(keys, std.Dictionary)
			identifier.Put(tc.query)
			matches, accepted := runNegotiatedStudyRootFind(t, ctx, index, identifier.Elements(), tc.support, tc.request)
			if accepted != tc.accepted {
				t.Fatalf("accepted negotiation = %+v, want %+v", accepted, tc.accepted)
			}
			var gotUIDs []string
			for _, match := range matches {
				uid, _ := match.GetUID(tags.StudyInstanceUID)
				gotUIDs = append(gotUIDs, uid)
				if uid == "1.2.3.2" {
					values, _ := match.GetStrings(tags.ModalitiesInStudy)
					if !slices.Equal(values, []string{"MR", "PT", "CT"}) {
						t.Fatalf("response modalities = %v", values)
					}
				}
			}
			if !slices.Equal(gotUIDs, tc.wantUIDs) {
				t.Fatalf("matched Study Instance UIDs = %v, want %v", gotUIDs, tc.wantUIDs)
			}
		})
	}
}

func TestMemoryCFindRejectsWildcardInNegotiatedMultipleValuesInEmptyIndex(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	negotiation := CFindExtendedNegotiation{MultipleValueMatching: true}
	identifier, err := BuildStudyRootStudyFindKeys(nil, "StudyInstanceUID")
	if err != nil {
		t.Fatal(err)
	}
	identifier = append(identifier, core.Element{
		Header: core.ElementHeader{Tag: tags.ModalitiesInStudy, VR: core.VRCS},
		Value:  core.StringValue{"CT", "M*"},
	})
	status, hits, accepted := runCheckedStudyRootFind(t, ctx, MemoryFindIndex{}, negotiation, &negotiation, identifier)
	if accepted != negotiation || hits != 0 || status != CFindStatusUnableToProcess {
		t.Fatalf("accepted = %+v, hits = %d, status = 0x%04X", accepted, hits, status)
	}
}
