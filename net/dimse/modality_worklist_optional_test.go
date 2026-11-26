package dimse

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestMWLOptionalScalarMatchingAndProjection(t *testing.T) {
	for _, tc := range []struct {
		name, query, candidate, miss string
		set                          func(*ModalityWorklistQuery, MWLKey)
		tag                          core.Tag
		vr                           core.VR
	}{
		{"issuer", "Hospital*", "HospitalA", "hospitalA", func(q *ModalityWorklistQuery, k MWLKey) { q.IssuerOfPatientID = k }, core.NewTag(0x0010, 0x0021), core.VRLO},
		{"date", "20261008-20261009", "20261009", "20261010", func(q *ModalityWorklistQuery, k MWLKey) { q.StudyDate = k }, core.NewTag(0x0008, 0x0020), core.VRDA},
		{"time", "090000-100000", "093000", "103000", func(q *ModalityWorklistQuery, k MWLKey) { q.StudyTime = k }, core.NewTag(0x0008, 0x0030), core.VRTM},
		{"UID", "1.2.3.4", "1.2.3.4", "1.2.3.5", func(q *ModalityWorklistQuery, k MWLKey) { q.StudyInstanceUID = k }, core.NewTag(0x0020, 0x000D), core.VRUI},
		{"requesting physician", "doc*", "Doctor^Synthetic", "Nurse^Synthetic", func(q *ModalityWorklistQuery, k MWLKey) { q.RequestingPhysician = k }, core.NewTag(0x0032, 0x1032), core.VRPN},
		{"referring physician", "doc?", "DOCX", "NURSE", func(q *ModalityWorklistQuery, k MWLKey) { q.ReferringPhysicianName = k }, core.NewTag(0x0008, 0x0090), core.VRPN},
		{"admission", "ADM?", "ADM1", "adm1", func(q *ModalityWorklistQuery, k MWLKey) { q.AdmissionID = k }, core.NewTag(0x0038, 0x0010), core.VRLO},
		{"priority", "H*", "HIGH", "LOW", func(q *ModalityWorklistQuery, k MWLKey) { q.RequestedProcedurePriority = k }, core.NewTag(0x0040, 0x1003), core.VRSH},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query := ModalityWorklistQuery{}
			tc.set(&query, MWLMatch(tc.query))
			identifier, err := BuildModalityWorklistIdentifier(query)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := ParseModalityWorklistIdentifier(identifier)
			if err != nil {
				t.Fatal(err)
			}
			if !modalityWorklistHasMatchingKey(parsed.Query) {
				t.Fatal("optional matching key did not admit the query")
			}
			candidate := object.FromElements([]core.Element{dicomtest.NewStringElement(tc.tag, tc.vr, tc.candidate), dicomtest.NewPNElement(tagMWLPatientName, "Unrequested^Synthetic")}, std.Dictionary)
			matched, err := MatchModalityWorklist(parsed.Query, candidate)
			if err != nil || !matched {
				t.Fatalf("positive match: %t, %v", matched, err)
			}
			projected, err := ProjectModalityWorklistResult(parsed, candidate)
			if err != nil {
				t.Fatal(err)
			}
			if !projected.Has(tc.tag) || projected.Has(tagMWLPatientName) {
				t.Fatal("projection included unrequested attribute or lost optional key")
			}
			candidate.Put(dicomtest.NewStringElement(tc.tag, tc.vr, tc.miss))
			matched, err = MatchModalityWorklist(parsed.Query, candidate)
			if err != nil || matched {
				t.Fatalf("negative match: %t, %v", matched, err)
			}
			tc.set(&query, MWLReturnKey())
			identifier, err = BuildModalityWorklistIdentifier(query)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err = ParseModalityWorklistIdentifier(identifier)
			if err != nil || modalityWorklistHasMatchingKey(parsed.Query) {
				t.Fatalf("universal key: %+v, %v", parsed.Query, err)
			}
			absent := object.New(std.Dictionary)
			matched, err = MatchModalityWorklist(parsed.Query, absent)
			if err != nil || !matched {
				t.Fatalf("universal with absent attribute: %t, %v", matched, err)
			}
			projected, err = ProjectModalityWorklistResult(parsed, absent)
			if err != nil || projected.Has(tc.tag) || modalityWorklistPendingStatus(parsed, absent) != StatusPendingWarning {
				t.Fatalf("missing optional return: %v", err)
			}
		})
	}
}

func TestMWLOptionalStudyTemporalRangesRemainIndependent(t *testing.T) {
	query := ModalityWorklistQuery{TimezoneOffsetFromUTC: "+0300", StudyDate: MWLMatch("20261008-20261010"), StudyTime: MWLMatch("090000-100000")}
	identifier, err := BuildModalityWorklistIdentifier(query)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseModalityWorklistIdentifier(identifier)
	if err != nil {
		t.Fatal(err)
	}
	candidate := object.FromElements([]core.Element{dicomtest.NewStringElement(core.NewTag(0x0008, 0x0020), core.VRDA, "20261009"), dicomtest.NewStringElement(core.NewTag(0x0008, 0x0030), core.VRTM, "110000")}, std.Dictionary)
	if match, err := MatchModalityWorklist(parsed.Query, candidate); err != nil || match {
		t.Fatalf("non-negotiated combined date/time: %t, %v", match, err)
	}
	candidate.Put(dicomtest.NewStringElement(core.NewTag(0x0008, 0x0030), core.VRTM, "093000"))
	projected, err := ProjectModalityWorklistResult(parsed, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := projected.GetString(tagMWLTimezoneOffsetFromUTC); got != "+0300" {
		t.Fatalf("projected timezone: %q", got)
	}
	for _, key := range []MWLKey{MWLMatch("-093000"), MWLMatch("093000-"), MWLMatch("093000")} {
		match, err := MatchModalityWorklist(ModalityWorklistQuery{StudyTime: key}, candidate)
		if err != nil || !match {
			t.Fatalf("open/single time: %t, %v", match, err)
		}
	}
}

func TestMWLOptionalPatientIssuerConflict(t *testing.T) {
	query := ModalityWorklistQuery{PatientID: MWLMatch("SYNTHETIC-ID"), IssuerOfPatientID: MWLMatch("ISSUER-A")}
	candidate := object.FromElements([]core.Element{dicomtest.NewStringElement(tagMWLPatientID, core.VRLO, "SYNTHETIC-ID"), dicomtest.NewStringElement(core.NewTag(0x0010, 0x0021), core.VRLO, "ISSUER-B")}, std.Dictionary)
	if match, err := MatchModalityWorklist(query, candidate); err != nil || match {
		t.Fatalf("issuer collision: %t, %v", match, err)
	}
}

func TestMWLOptionalCodeSameItemAndProjection(t *testing.T) {
	tag := core.NewTag(0x0032, 0x1064)
	queryItem := core.DataSet{Elements: []core.Element{dicomtest.NewStringElement(core.NewTag(0x0008, 0x0100), core.VRSH, "VALUE-A"), dicomtest.NewStringElement(core.NewTag(0x0008, 0x0102), core.VRSH, "SCHEME-B"), mwlEmpty(core.NewTag(0x0008, 0x0104), core.VRLO)}}
	identifier, err := BuildModalityWorklistIdentifier(ModalityWorklistQuery{RequestedProcedureCodeSequence: MWLSequenceMatch(queryItem)})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseModalityWorklistIdentifier(identifier)
	if err != nil {
		t.Fatal(err)
	}
	candidate := object.FromElements([]core.Element{dicomtest.NewSequenceElement(tag, mwlCode("VALUE-A", "SCHEME-A", "First"), mwlCode("VALUE-B", "SCHEME-B", "Second"))}, std.Dictionary)
	if match, err := MatchModalityWorklist(parsed.Query, candidate); err != nil || match {
		t.Fatalf("cross-item false match: %t, %v", match, err)
	}
	third := mwlCode("VALUE-A", "SCHEME-B", "Selected")
	third.Elements = append(third.Elements, dicomtest.NewStringElement(core.NewTag(0x0008, 0x0103), core.VRSH, "UNREQUESTED"))
	candidate.Put(dicomtest.NewSequenceElement(tag, mwlCode("VALUE-A", "SCHEME-A", "First"), third, mwlCode("VALUE-B", "SCHEME-B", "Second")))
	if match, err := MatchModalityWorklist(parsed.Query, candidate); err != nil || !match {
		t.Fatalf("same-item match: %t, %v", match, err)
	}
	projected, err := ProjectModalityWorklistResult(parsed, candidate)
	if err != nil {
		t.Fatal(err)
	}
	items, _ := projected.GetSequence(tag)
	if len(items) != 1 || items[0].Has(core.NewTag(0x0008, 0x0103)) {
		t.Fatal("projection returned unmatched items or unrequested keys")
	}
	if got, _ := items[0].GetString(core.NewTag(0x0008, 0x0104)); got != "Selected" {
		t.Fatalf("meaning %q", got)
	}
	third.Elements[2].Value.(core.StringValue)[0] = "MUTATED"
	if got, _ := items[0].GetString(core.NewTag(0x0008, 0x0104)); got != "Selected" {
		t.Fatal("response aliases provider values")
	}
}

func TestMWLOptionalSequencesUniversalAndMissingNestedKeys(t *testing.T) {
	tag := core.NewTag(0x0032, 0x1064)
	candidate := object.FromElements([]core.Element{dicomtest.NewSequenceElement(tag, mwlCode("A", "99SYN", "One"), mwlCode("B", "99SYN", "Two"))}, std.Dictionary)
	for _, key := range []MWLSequenceKey{MWLSequenceReturnKey(), MWLSequenceMatch(core.DataSet{})} {
		identifier, err := BuildModalityWorklistIdentifier(ModalityWorklistQuery{RequestedProcedureCodeSequence: key})
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := ParseModalityWorklistIdentifier(identifier)
		if err != nil || modalityWorklistHasMatchingKey(parsed.Query) {
			t.Fatalf("universal sequence: %v", err)
		}
		projected, err := ProjectModalityWorklistResult(parsed, candidate)
		if err != nil {
			t.Fatal(err)
		}
		items, _ := projected.GetSequence(tag)
		if len(items) != 2 || !items[0].Has(core.NewTag(0x0008, 0x0104)) {
			t.Fatal("universal sequence was not returned whole")
		}
	}
	key := MWLSequenceMatch(core.DataSet{Elements: []core.Element{mwlEmpty(core.NewTag(0x0008, 0x0103), core.VRSH)}})
	identifier, _ := BuildModalityWorklistIdentifier(ModalityWorklistQuery{RequestedProcedureCodeSequence: key})
	parsed, err := ParseModalityWorklistIdentifier(identifier)
	if err != nil {
		t.Fatal(err)
	}
	if modalityWorklistPendingStatus(parsed, candidate) != StatusPendingWarning {
		t.Fatal("missing nested optional key did not produce FF01")
	}
	projected, err := ProjectModalityWorklistResult(parsed, candidate)
	if err != nil {
		t.Fatal(err)
	}
	items, _ := projected.GetSequence(tag)
	if len(items) != 2 || items[0].Len() != 0 {
		t.Fatal("unrequested nested values leaked")
	}
}

func TestMWLVeterinaryExtensionExplicitAndRecursive(t *testing.T) {
	registry := core.NewTag(0x0010, 0x2296)
	registration := core.NewTag(0x0010, 0x2294)
	code := core.NewTag(0x0008, 0x0100)
	query := ModalityWorklistQuery{Veterinary: &ModalityWorklistVeterinaryQuery{PatientSpeciesDescription: MWLMatch("Can*"), BreedRegistrationSequence: MWLSequenceMatch(core.DataSet{Elements: []core.Element{dicomtest.NewStringElement(core.NewTag(0x0010, 0x2295), core.VRLO, "REG-A"), dicomtest.NewSequenceElement(registry, core.DataSet{Elements: []core.Element{dicomtest.NewStringElement(code, core.VRSH, "B")}})}})}}
	identifier, err := BuildModalityWorklistIdentifier(query)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseModalityWorklistIdentifier(identifier); !errors.Is(err, ErrModalityWorklistIdentifier) {
		t.Fatalf("default veterinary admission: %v", err)
	}
	parsed, err := ParseModalityWorklistIdentifierWithOptions(identifier, ModalityWorklistParseOptions{EnableVeterinaryMatching: true})
	if err != nil {
		t.Fatal(err)
	}
	reg := func(number, value string) core.DataSet {
		return core.DataSet{Elements: []core.Element{dicomtest.NewStringElement(core.NewTag(0x0010, 0x2295), core.VRLO, number), dicomtest.NewSequenceElement(registry, mwlCode(value, "99SYN", "Synthetic registry"))}}
	}
	candidate := object.FromElements([]core.Element{dicomtest.NewStringElement(core.NewTag(0x0010, 0x2201), core.VRLO, "Canine"), dicomtest.NewSequenceElement(registration, reg("REG-A", "A"), reg("REG-B", "B"))}, std.Dictionary)
	if match, err := MatchModalityWorklist(parsed.Query, candidate); err != nil || match {
		t.Fatalf("cross registration/registry match: %t, %v", match, err)
	}
	candidate.Put(dicomtest.NewSequenceElement(registration, reg("REG-A", "A"), reg("REG-A", "B")))
	if match, err := MatchModalityWorklist(parsed.Query, candidate); err != nil || !match {
		t.Fatalf("recursive same-item: %t, %v", match, err)
	}
	projected, err := ProjectModalityWorklistResult(parsed, candidate)
	if err != nil {
		t.Fatal(err)
	}
	items, _ := projected.GetSequence(registration)
	if len(items) != 1 {
		t.Fatalf("registration count %d", len(items))
	}
	nested, _ := items[0].GetSequence(registry)
	if len(nested) != 1 || nested[0].Len() != 1 {
		t.Fatal("recursive projection included unrequested coding attributes")
	}
}

func TestMWLOptionalIdentifierBoundsAndRejectedForms(t *testing.T) {
	tag := core.NewTag(0x0032, 0x1064)
	code := core.NewTag(0x0008, 0x0100)
	for _, element := range []core.Element{
		dicomtest.NewSequenceElement(tag, mwlCode("A", "B", "C"), mwlCode("D", "E", "F")),
		dicomtest.NewSequenceElement(tag, core.DataSet{Elements: []core.Element{dicomtest.NewStringElement(code, core.VRSH, "A"), dicomtest.NewStringElement(code, core.VRSH, "B")}}),
		dicomtest.NewSequenceElement(tag, core.DataSet{Elements: []core.Element{dicomtest.NewStringElement(core.NewTag(0x0008, 0x010F), core.VRCS, "DCMR")}}),
		dicomtest.NewStringElement(core.NewTag(0x0040, 0x1003), core.VRLO, "HIGH"),
		dicomtest.NewStringElement(core.NewTag(0x0008, 0x0020), core.VRDA, "20260230-"),
		dicomtest.NewStringElement(core.NewTag(0x0008, 0x0030), core.VRTM, "230000-010000"),
	} {
		if _, err := ParseModalityWorklistIdentifierWithOptions(object.FromElements([]core.Element{element}, std.Dictionary), ModalityWorklistParseOptions{AcceptUnknownReturnKeys: true}); !errors.Is(err, ErrModalityWorklistIdentifier) {
			t.Fatalf("invalid key %s: %v", element.Tag(), err)
		}
	}
	wide := core.DataSet{}
	for i := 0; i < defaultModalityWorklistIdentifierElements; i++ {
		wide.Elements = append(wide.Elements, mwlEmpty(code, core.VRSH))
	}
	if _, err := BuildModalityWorklistIdentifier(ModalityWorklistQuery{RequestedProcedureCodeSequence: MWLSequenceMatch(wide)}); !errors.Is(err, ErrModalityWorklistResourceLimit) {
		t.Fatalf("wide query: %v", err)
	}
	huge := core.DataSet{Elements: []core.Element{dicomtest.NewStringElement(core.NewTag(0x0008, 0x0119), core.VRUC, strings.Repeat("X", int(MaxIdentifierBytes)+1))}}
	if _, err := BuildModalityWorklistIdentifier(ModalityWorklistQuery{RequestedProcedureCodeSequence: MWLSequenceMatch(huge)}); !errors.Is(err, ErrModalityWorklistResourceLimit) {
		t.Fatalf("value byte limit: %v", err)
	}
	// A cyclic query must fail bounded admission before recursion/facades.
	cycle := make([]core.Element, 1)
	cycle[0] = dicomtest.NewSequenceElement(core.NewTag(0x0010, 0x0024), core.DataSet{Elements: cycle})
	if _, err := BuildModalityWorklistIdentifier(ModalityWorklistQuery{OtherPatientIDsSequence: MWLSequenceMatch(core.DataSet{Elements: cycle})}); !errors.Is(err, ErrModalityWorklistResourceLimit) {
		t.Fatalf("cyclic query: %v", err)
	}
}

func TestMWLOptionalRawCharsetAndTransferSyntaxRoundTrip(t *testing.T) {
	query := ModalityWorklistQuery{SpecificCharacterSet: []string{"ISO_IR 100"}, IssuerOfPatientID: MWLMatch("Clínica*"), RequestingPhysician: MWLMatch("García*")}
	identifier, err := BuildModalityWorklistIdentifier(query)
	if err != nil {
		t.Fatal(err)
	}
	for _, syntax := range []transfer.Syntax{transfer.ImplicitVRLittleEndian, transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
		var encoded bytes.Buffer
		if err := object.WriteDataSet(&encoded, identifier, syntax); err != nil {
			t.Fatal(err)
		}
		decoded, err := object.ReadDataSet(bytes.NewReader(encoded.Bytes()), syntax)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := ParseModalityWorklistIdentifier(decoded)
		if err != nil {
			t.Fatal(err)
		}
		if got := parsed.Query.IssuerOfPatientID.Values[0]; got != "Clínica*" {
			t.Fatalf("raw query decoded %q", got)
		}
		candidate := object.FromElements([]core.Element{dicomtest.NewStringElement(tagMWLSpecificCharacterSet, core.VRCS, "ISO_IR 100"), core.NewRawElement(core.NewTag(0x0010, 0x0021), core.VRLO, []byte("Cl\xednicaA")), core.NewRawElement(core.NewTag(0x0032, 0x1032), core.VRPN, []byte("Garc\xeda^Synthetic"))}, std.Dictionary)
		if match, err := MatchModalityWorklist(parsed.Query, candidate); err != nil || !match {
			t.Fatalf("raw candidate %s: %t, %v", syntax.UID, match, err)
		}
		projected, err := ProjectModalityWorklistResult(parsed, candidate)
		if err != nil || !projected.Has(tagMWLSpecificCharacterSet) {
			t.Fatalf("raw projection %s: %v", syntax.UID, err)
		}
	}
}

func TestMWLOptionalQuerySCUSCPRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	peer, local := testPipeAssociations(t, []ul.AcceptedContext{{ID: 1, AbstractSyntaxUID: ModalityWorklistFindSOPClassUID, TransferSyntaxUID: transfer.ImplicitVRLittleEndian.UID}})
	defer peer.Close()
	defer local.Close()
	handler, err := NewInMemoryModalityWorklistHandler([]*object.Object{object.FromElements([]core.Element{dicomtest.NewStringElement(core.NewTag(0x0038, 0x0010), core.VRLO, "ADM-A"), dicomtest.NewStringElement(core.NewTag(0x0040, 0x1003), core.VRSH, "HIGH")}, std.Dictionary), object.FromElements([]core.Element{dicomtest.NewStringElement(core.NewTag(0x0038, 0x0010), core.VRLO, "ADM-B")}, std.Dictionary)})
	if err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- ServeModalityWorklistCFind(ctx, local, 1, handler) }()
	client, err := NewModalityWorklistClient(peer)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	result, err := client.Find(ctx, ModalityWorklistQuery{AdmissionID: MWLMatch("ADM-A"), RequestedProcedurePriority: MWLReturnKey()}, func(match *object.Object) error {
		count++
		if got, _ := match.GetString(core.NewTag(0x0040, 0x1003)); got != "HIGH" {
			return errors.New("unexpected priority")
		}
		return nil
	})
	if err != nil || count != 1 || result.FinalResponse == nil || result.FinalResponse.Status != StatusSuccess {
		t.Fatalf("wire result %+v matches %d: %v", result, count, err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestMWLOptionalIssuerAndOtherPatientIDSequences(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tag, child core.Tag
		vr         core.VR
		set        func(*ModalityWorklistQuery, MWLSequenceKey)
	}{
		{"accession issuer", core.NewTag(0x0008, 0x0051), core.NewTag(0x0040, 0x0031), core.VRUT, func(q *ModalityWorklistQuery, k MWLSequenceKey) { q.IssuerOfAccessionNumberSequence = k }},
		{"admission issuer", core.NewTag(0x0038, 0x0014), core.NewTag(0x0040, 0x0032), core.VRUT, func(q *ModalityWorklistQuery, k MWLSequenceKey) { q.IssuerOfAdmissionIDSequence = k }},
		{"patient issuer qualifiers", core.NewTag(0x0010, 0x0024), core.NewTag(0x0040, 0x0035), core.VRCS, func(q *ModalityWorklistQuery, k MWLSequenceKey) { q.IssuerOfPatientIDQualifiersSequence = k }},
		{"other IDs", core.NewTag(0x0010, 0x1002), tagMWLPatientID, core.VRLO, func(q *ModalityWorklistQuery, k MWLSequenceKey) { q.OtherPatientIDsSequence = k }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query := ModalityWorklistQuery{}
			tc.set(&query, MWLSequenceMatch(core.DataSet{Elements: []core.Element{dicomtest.NewStringElement(tc.child, tc.vr, "SYNTHETIC")}}))
			identifier, err := BuildModalityWorklistIdentifier(query)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := ParseModalityWorklistIdentifier(identifier)
			if err != nil {
				t.Fatal(err)
			}
			candidate := object.FromElements([]core.Element{dicomtest.NewSequenceElement(tc.tag, core.DataSet{Elements: []core.Element{dicomtest.NewStringElement(tc.child, tc.vr, "OTHER")}}, core.DataSet{Elements: []core.Element{dicomtest.NewStringElement(tc.child, tc.vr, "SYNTHETIC")}})}, std.Dictionary)
			if matched, err := MatchModalityWorklist(parsed.Query, candidate); err != nil || !matched {
				t.Fatalf("sequence match %t: %v", matched, err)
			}
			projected, err := ProjectModalityWorklistResult(parsed, candidate)
			if err != nil {
				t.Fatal(err)
			}
			items, _ := projected.GetSequence(tc.tag)
			if len(items) != 1 {
				t.Fatal("unmatched issuer/ID was projected")
			}
			candidate.Put(dicomtest.NewSequenceElement(tc.tag, core.DataSet{Elements: []core.Element{dicomtest.NewStringElement(tc.child, tc.vr, "OTHER")}}))
			if matched, err := MatchModalityWorklist(parsed.Query, candidate); err != nil || matched {
				t.Fatalf("issuer/ID miss %t: %v", matched, err)
			}
		})
	}
}

func TestMWLVeterinaryScalarAndCodeKeys(t *testing.T) {
	for _, tc := range []struct {
		tag                         core.Tag
		vr                          core.VR
		pattern, positive, negative string
	}{
		{core.NewTag(0x0010, 0x2201), core.VRLO, "Can*", "Canine", "canine"},
		{core.NewTag(0x0010, 0x2292), core.VRLO, "Bea?le", "Beagle", "Spaniel"},
		{core.NewTag(0x0010, 0x2203), core.VRCS, "Y*", "YES", "NO"},
		{core.NewTag(0x0010, 0x2297), core.VRPN, "owner*", "OWNER^Synthetic", "VET^Synthetic"},
		{core.NewTag(0x0010, 0x2298), core.VRCS, "OWN*", "OWNER", "VET"},
		{core.NewTag(0x0010, 0x2299), core.VRLO, "Clinic*", "ClinicSynthetic", "clinicSynthetic"},
	} {
		identifier := object.FromElements([]core.Element{dicomtest.NewStringElement(tc.tag, tc.vr, tc.pattern)}, std.Dictionary)
		parsed, err := ParseModalityWorklistIdentifierWithOptions(identifier, ModalityWorklistParseOptions{EnableVeterinaryMatching: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range []string{tc.positive, tc.negative} {
			candidate := object.FromElements([]core.Element{dicomtest.NewStringElement(tc.tag, tc.vr, value)}, std.Dictionary)
			matched, err := MatchModalityWorklist(parsed.Query, candidate)
			if err != nil || matched != (value == tc.positive) {
				t.Fatalf("veterinary %s match %t: %v", tc.tag, matched, err)
			}
		}
	}
	for _, tag := range []core.Tag{core.NewTag(0x0010, 0x2202), core.NewTag(0x0010, 0x2293)} {
		identifier := object.FromElements([]core.Element{dicomtest.NewSequenceElement(tag, core.DataSet{Elements: []core.Element{dicomtest.NewStringElement(core.NewTag(0x0008, 0x0100), core.VRSH, "A*"), dicomtest.NewStringElement(core.NewTag(0x0008, 0x0102), core.VRSH, "99SYN")}})}, std.Dictionary)
		parsed, err := ParseModalityWorklistIdentifierWithOptions(identifier, ModalityWorklistParseOptions{EnableVeterinaryMatching: true})
		if err != nil {
			t.Fatal(err)
		}
		candidate := object.FromElements([]core.Element{dicomtest.NewSequenceElement(tag, mwlCode("A1", "99SYN", "Synthetic"))}, std.Dictionary)
		if matched, err := MatchModalityWorklist(parsed.Query, candidate); err != nil || !matched {
			t.Fatalf("veterinary code %s: %t, %v", tag, matched, err)
		}
	}
}

func TestMWLScheduledProtocolRemainsReturnOnly(t *testing.T) {
	identifier := object.FromElements([]core.Element{dicomtest.NewSequenceElement(tagMWLScheduledProcedureStepSequence, core.DataSet{Elements: []core.Element{dicomtest.NewSequenceElement(tagMWLScheduledProtocolCodeSequence, mwlCode("MATCH", "99SYN", "Synthetic"))}})}, std.Dictionary)
	if _, err := ParseModalityWorklistIdentifier(identifier); !errors.Is(err, ErrModalityWorklistIdentifier) {
		t.Fatalf("protocol matching must be refused: %v", err)
	}
}

func TestMWLOptionalSequenceDetachmentAndAggregateLimits(t *testing.T) {
	item := mwlCode("SYNTHETIC", "99SYN", "Original")
	identifier, err := BuildModalityWorklistIdentifier(ModalityWorklistQuery{RequestedProcedureCodeSequence: MWLSequenceMatch(item)})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseModalityWorklistIdentifier(identifier)
	if err != nil {
		t.Fatal(err)
	}
	item.Elements[0].Value.(core.StringValue)[0] = "MUTATED"
	identifier.Elements()[0].Value.(core.SequenceValue).Items[0].Elements[0].Value.(core.StringValue)[0] = "CHANGED"
	got := parsed.Query.RequestedProcedureCodeSequence.Items[0].Elements[0].StringValues()[0]
	if got != "SYNTHETIC" {
		t.Fatalf("parsed query aliases caller: %q", got)
	}
	large := core.DataSet{Elements: []core.Element{dicomtest.NewStringElement(core.NewTag(0x0040, 0x0032), core.VRUT, strings.Repeat("X", int(MaxIdentifierBytes/2)+1))}}
	if _, err := BuildModalityWorklistIdentifier(ModalityWorklistQuery{IssuerOfAccessionNumberSequence: MWLSequenceMatch(large), IssuerOfAdmissionIDSequence: MWLSequenceMatch(large)}); !errors.Is(err, ErrModalityWorklistResourceLimit) {
		t.Fatalf("aggregate primitive bytes: %v", err)
	}
	identifier, _ = BuildModalityWorklistIdentifier(ModalityWorklistQuery{RequestedProcedureCodeSequence: MWLSequenceMatch(mwlCode("A", "99SYN", "Meaning"))})
	parsed, _ = ParseModalityWorklistIdentifier(identifier)
	candidate := object.FromElements([]core.Element{dicomtest.NewSequenceElement(core.NewTag(0x0032, 0x1064), mwlCode("A", "99SYN", "Meaning"))}, std.Dictionary)
	if _, err := projectModalityWorklistResultWithLimits(parsed, candidate, 1024, 3, 8); !errors.Is(err, ErrModalityWorklistResourceLimit) {
		t.Fatalf("projection bytes: %v", err)
	}
	if _, err := projectModalityWorklistResultWithLimits(parsed, candidate, 1024, MaxIdentifierBytes, 1); !errors.Is(err, ErrModalityWorklistResourceLimit) {
		t.Fatalf("projection depth: %v", err)
	}
}

func TestMWLOptionalLongAndURNCodeMatching(t *testing.T) {
	for _, tc := range []struct {
		tag          core.Tag
		vr           core.VR
		query, value string
	}{
		{core.NewTag(0x0008, 0x0119), core.VRUC, "SyntheticLongCode*", "SyntheticLongCodeValue"},
		{core.NewTag(0x0008, 0x0120), core.VRUR, "urn:dicom:*", "urn:dicom:synthetic"},
	} {
		key := MWLSequenceMatch(core.DataSet{Elements: []core.Element{dicomtest.NewStringElement(tc.tag, tc.vr, tc.query)}})
		identifier, err := BuildModalityWorklistIdentifier(ModalityWorklistQuery{RequestedProcedureCodeSequence: key})
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := ParseModalityWorklistIdentifier(identifier)
		if err != nil {
			t.Fatal(err)
		}
		candidate := object.FromElements([]core.Element{dicomtest.NewSequenceElement(core.NewTag(0x0032, 0x1064), core.DataSet{Elements: []core.Element{dicomtest.NewStringElement(tc.tag, tc.vr, tc.value)}})}, std.Dictionary)
		if matched, err := MatchModalityWorklist(parsed.Query, candidate); err != nil || !matched {
			t.Fatalf("code key %s: %t, %v", tc.tag, matched, err)
		}
	}
}

func TestMWLOptionalParserResourceRefusalIsA700(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	peer, local := testPipeAssociations(t, []ul.AcceptedContext{{ID: 1, AbstractSyntaxUID: ModalityWorklistFindSOPClassUID, TransferSyntaxUID: transfer.ImplicitVRLittleEndian.UID}})
	defer peer.Close()
	defer local.Close()
	item := core.DataSet{}
	for i := 0; i < defaultModalityWorklistIdentifierElements; i++ {
		item.Elements = append(item.Elements, mwlEmpty(core.NewTag(0x0008, 0x0100), core.VRSH))
	}
	identifier := object.FromElements([]core.Element{dicomtest.NewSequenceElement(core.NewTag(0x0032, 0x1064), item)}, std.Dictionary)
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- ServeModalityWorklistCFindWithOptions(ctx, local, 1, ModalityWorklistHandlerFunc(func(context.Context, ModalityWorklistRequest, ModalityWorklistYield) error {
			return errors.New("handler must not run")
		}), ModalityWorklistSCPOptions{MaxIdentifierElements: 512})
	}()
	if err := SendCommandSetWithContext(ctx, peer, 1, (CFindRequest{AffectedSOPClassUID: ModalityWorklistFindSOPClassUID, MessageID: 71}).CommandSet()); err != nil {
		t.Fatal(err)
	}
	if err := SendDataSetWithContext(ctx, peer, 1, identifier, transfer.ImplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	response, value, err := receiveCFindResponseWithContext(ctx, peer, 1, transfer.ImplicitVRLittleEndian)
	if err != nil || response.Status != CFindStatusOutOfResources || value != nil {
		t.Fatalf("bounded parse status=%04X value=%v: %v", response.Status, value != nil, err)
	}
	if err := <-serverDone; !errors.Is(err, ErrModalityWorklistResourceLimit) {
		t.Fatalf("server classification: %v", err)
	}
}
