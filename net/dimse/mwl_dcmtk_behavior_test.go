package dimse

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
)

func mwlEmpty(tag core.Tag, vr core.VR) core.Element {
	if vr == core.VRSQ {
		return dicomtest.NewSequenceElement(tag)
	}
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue{}}
}

func mwlCode(value, scheme, meaning string) core.DataSet {
	return core.DataSet{Elements: []core.Element{
		dicomtest.NewStringElement(core.NewTag(0x0008, 0x0100), core.VRSH, value),
		dicomtest.NewStringElement(core.NewTag(0x0008, 0x0102), core.VRSH, scheme),
		dicomtest.NewStringElement(core.NewTag(0x0008, 0x0104), core.VRLO, meaning),
	}}
}

var (
	tagPatientSpeciesDescription  = core.NewTag(0x0010, 0x2201)
	tagPatientSpeciesCodeSequence = core.NewTag(0x0010, 0x2202)
	tagPatientBreedCodeSequence   = core.NewTag(0x0010, 0x2293)
	tagBreedRegistrationSequence  = core.NewTag(0x0010, 0x2294)
	tagBreedRegistryCodeSequence  = core.NewTag(0x0010, 0x2296)
	tagResponsibleOrganization    = core.NewTag(0x0010, 0x2299)
	tagPatientTelephoneNumbers    = core.NewTag(0x0010, 0x2154)
	tagOtherPatientNames          = core.NewTag(0x0010, 0x1001)
	tagPatientMotherBirthName     = core.NewTag(0x0010, 0x1060)
	tagPregnancyStatus            = core.NewTag(0x0010, 0x21C0)
	tagRows                       = core.NewTag(0x0028, 0x0010)
)

// TestWorklistVeterinaryKeysAfterDCMTKTwlmatch follows DCMTK's twlmatch
// scenario with a fictitious veterinary record. Empty veterinary return keys
// (outside PS3.4 Table K.6-1) are returned whole with AcceptUnknownReturnKeys,
// including nested code sequences. DCMTK wlmscpfs also matches on them; this
// profile refuses such matching keys (A900), a documented divergence.
func TestWorklistVeterinaryKeysAfterDCMTKTwlmatch(t *testing.T) {
	record := object.FromElements([]core.Element{
		dicomtest.NewPNElement(tagMWLPatientName, "Rex^Dog"),
		dicomtest.NewStringElement(tagMWLPatientID, core.VRLO, "VET991"),
		dicomtest.NewStringElement(tagPatientSpeciesDescription, core.VRLO, "Canine"),
		dicomtest.NewSequenceElement(tagPatientSpeciesCodeSequence, mwlCode("448771007", "SCT", "Canis lupus familiaris")),
		dicomtest.NewSequenceElement(tagPatientBreedCodeSequence, mwlCode("132380002", "SCT", "Beagle, Smooth dog breed")),
		dicomtest.NewSequenceElement(tagBreedRegistrationSequence, core.DataSet{Elements: []core.Element{
			dicomtest.NewStringElement(core.NewTag(0x0010, 0x2295), core.VRLO, "996"),
			dicomtest.NewSequenceElement(tagBreedRegistryCodeSequence, mwlCode("109200", "DCM", "America Kennel Club")),
		}}),
		dicomtest.NewStringElement(tagResponsibleOrganization, core.VRLO, "Fictitious Clinic"),
	}, std.Dictionary)
	query := object.FromElements([]core.Element{
		mwlEmpty(tagMWLPatientName, core.VRPN),
		mwlEmpty(tagResponsibleOrganization, core.VRLO),
		mwlEmpty(tagPatientSpeciesCodeSequence, core.VRSQ),
		mwlEmpty(tagPatientBreedCodeSequence, core.VRSQ),
		mwlEmpty(tagBreedRegistrationSequence, core.VRSQ),
	}, std.Dictionary)
	parsed, err := ParseModalityWorklistIdentifierWithOptions(query, ModalityWorklistParseOptions{AcceptUnknownReturnKeys: true})
	if err != nil {
		t.Fatal(err)
	}
	response, err := ProjectModalityWorklistResult(parsed, record)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := worklistFingerprint(response)
	for _, want := range []string{
		"00102299=Fictitious Clinic",
		"00102202[0].00080100=448771007",
		"00102293[0].00080104=Beagle, Smooth dog breed",
		"00102294[0].00102295=996",
		"00102294[0].00102296[0].00080100=109200",
	} {
		if !strings.Contains(fingerprint, want) {
			t.Errorf("response lacks %s:\n%s", want, fingerprint)
		}
	}
	if status := modalityWorklistPendingStatus(parsed, record); status != StatusPending {
		t.Fatalf("pending status 0x%04X, want FF00", status)
	}
	// DCMTK matches on veterinary keys; this profile refuses them.
	matching := object.FromElements([]core.Element{dicomtest.NewStringElement(tagPatientSpeciesDescription, core.VRLO, "Canine")}, std.Dictionary)
	if _, err := ParseModalityWorklistIdentifierWithOptions(matching, ModalityWorklistParseOptions{AcceptUnknownReturnKeys: true}); !errors.Is(err, ErrModalityWorklistIdentifier) {
		t.Fatalf("matching on Patient Species Description: %v, want A900", err)
	}
}

// TestWorklistReturnKeysAfterDCMTKTwlallret follows DCMTK's twlallret
// scenario. By default an attribute outside Table K.6-1 refuses the
// Identifier (A900) where wlmscpfs drops it with FF01. With
// AcceptUnknownReturnKeys it is returned when the record has it, including a
// binary VR, as wlmscpfs --enable-all-return does; a requested attribute the
// record lacks is omitted with FF01 where wlmscpfs returns it empty, and an
// absent Pregnancy Status is omitted where wlmscpfs returns 4 (unknown).
func TestWorklistReturnKeysAfterDCMTKTwlallret(t *testing.T) {
	record := object.FromElements([]core.Element{
		dicomtest.NewPNElement(tagMWLPatientName, "Doe^Fictitious"),
		dicomtest.NewStringElement(tagMWLPatientID, core.VRLO, "PID996"),
		{Header: core.ElementHeader{Tag: tagOtherPatientNames, VR: core.VRPN}, Value: core.StringValue{"Alias^One", "Alias^Two"}},
		dicomtest.NewStringElement(tagPatientTelephoneNumbers, core.VRSH, "0000-000000"),
		dicomtest.NewUShortElement(tagRows, 512),
	}, std.Dictionary)
	withTelephone := object.FromElements([]core.Element{mwlEmpty(tagMWLPatientName, core.VRPN), mwlEmpty(tagPatientTelephoneNumbers, core.VRSH)}, std.Dictionary)
	if _, err := ParseModalityWorklistIdentifier(withTelephone); !errors.Is(err, ErrModalityWorklistIdentifier) {
		t.Fatalf("default profile: %v, want A900", err)
	}
	all := ModalityWorklistParseOptions{AcceptUnknownReturnKeys: true}
	query := object.FromElements([]core.Element{
		mwlEmpty(tagMWLPatientName, core.VRPN),
		mwlEmpty(tagOtherPatientNames, core.VRPN),
		mwlEmpty(tagPatientTelephoneNumbers, core.VRSH),
		core.NewRawElement(tagRows, core.VRUS, nil),
	}, std.Dictionary)
	parsed, err := ParseModalityWorklistIdentifierWithOptions(query, all)
	if err != nil {
		t.Fatal(err)
	}
	response, err := ProjectModalityWorklistResult(parsed, record)
	if err != nil {
		t.Fatal(err)
	}
	if values, _ := response.GetStrings(tagOtherPatientNames); strings.Join(values, `\`) != `Alias^One\Alias^Two` {
		t.Fatalf("Other Patient Names %q", values)
	}
	if value, _ := response.GetString(tagPatientTelephoneNumbers); value != "0000-000000" {
		t.Fatalf("Patient's Telephone Numbers %q", value)
	}
	if raw, ok := response.GetRaw(tagRows); !ok || len(raw) != 2 || binary.LittleEndian.Uint16(raw) != 512 {
		t.Fatalf("Rows %v", raw)
	}
	if status := modalityWorklistPendingStatus(parsed, record); status != StatusPending {
		t.Fatalf("pending status 0x%04X, want FF00", status)
	}
	absent := object.FromElements([]core.Element{
		mwlEmpty(tagMWLPatientName, core.VRPN),
		mwlEmpty(tagPatientMotherBirthName, core.VRPN),
		core.NewRawElement(tagPregnancyStatus, core.VRUS, nil),
	}, std.Dictionary)
	parsed, err = ParseModalityWorklistIdentifierWithOptions(absent, all)
	if err != nil {
		t.Fatal(err)
	}
	response, err = ProjectModalityWorklistResult(parsed, record)
	if err != nil {
		t.Fatal(err)
	}
	if response.Has(tagPatientMotherBirthName) || response.Has(tagPregnancyStatus) {
		t.Fatal("absent return keys were returned")
	}
	if status := modalityWorklistPendingStatus(parsed, record); status != StatusPendingWarning {
		t.Fatalf("pending status 0x%04X, want FF01", status)
	}
}

// TestStudyRootCFindLevelAfterDCMTKTqrlevel follows DCMTK's tqrlevel: a
// lower-case QueryRetrieveLevel is accepted, as DCMTK upper-cases it (PS3.5
// defines CS values in upper case, so this is leniency), and a 60-character
// level ends the request. DCMTK answers it with Unable to Process (C000); this
// SCP answers Identifier does not match SOP Class (A900), the PS3.4 C.4.1.1.4
// status for an Identifier that does not fit the Information Model.
func TestStudyRootCFindLevelAfterDCMTKTqrlevel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	status, serverErr := runStudyRootCFindStatusTest(t, ctx, CFindHandlerFunc(func(_ context.Context, req CFindRequestContext) ([]*object.Object, error) {
		if req.QueryRetrieveLevel != QueryRetrieveLevelStudy {
			return nil, errors.New("level not normalized")
		}
		return nil, nil
	}), withQueryRetrieveLevel("study"))
	if status != StatusSuccess || serverErr != nil {
		t.Fatalf("lower-case level: status 0x%04X, %v", status, serverErr)
	}
	status, serverErr = runStudyRootCFindStatusTest(t, ctx, CFindHandlerFunc(func(context.Context, CFindRequestContext) ([]*object.Object, error) {
		return nil, errors.New("handler must not run")
	}), withQueryRetrieveLevel(strings.Repeat("a", 60)))
	if status != CFindStatusIdentifierDoesNotMatchSOPClass || serverErr == nil {
		t.Fatalf("oversized level: status 0x%04X, %v; want 0xA900", status, serverErr)
	}
}

func TestModalityWorklistReturnKeyRules(t *testing.T) {
	studyUID := core.NewTag(0x0020, 0x000D)
	priority := core.NewTag(0x0040, 0x1003)
	for _, tc := range []struct {
		name    string
		element core.Element
		ok      bool
	}{
		{"K.6-1 key empty", mwlEmpty(studyUID, core.VRUI), true},
		{"K.6-1 key asterisk", dicomtest.NewStringElement(priority, core.VRSH, "*"), true},
		{"K.6-1 priority matching", dicomtest.NewStringElement(priority, core.VRSH, "HIGH"), true},
		{"K.6-1 key with the wrong VR", mwlEmpty(priority, core.VRLO), false},
		{"K.6-1 sequence with empty item keys", dicomtest.NewSequenceElement(core.NewTag(0x0032, 0x1064), core.DataSet{Elements: []core.Element{mwlEmpty(core.NewTag(0x0008, 0x0100), core.VRSH)}}), true},
		{"K.6-1 coded sequence matching", dicomtest.NewSequenceElement(core.NewTag(0x0032, 0x1064), mwlCode("X", "Y", "Z")), true},
		{"attribute outside K.6-1", mwlEmpty(tagPatientTelephoneNumbers, core.VRSH), false},
	} {
		query := object.FromElements([]core.Element{mwlEmpty(tagMWLPatientName, core.VRPN), tc.element}, std.Dictionary)
		_, err := ParseModalityWorklistIdentifier(query)
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v, want accepted %v", tc.name, err, tc.ok)
		}
	}
}

func TestInMemoryWorklistUniversalQueryOption(t *testing.T) {
	records := dcmtkWorklistRecords(t)
	query := object.FromElements([]core.Element{mwlEmpty(tagMWLPatientName, core.VRPN)}, std.Dictionary)
	parsed, err := ParseModalityWorklistIdentifier(query)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		options InMemoryModalityWorklistOptions
		want    int
	}{{InMemoryModalityWorklistOptions{}, 0}, {InMemoryModalityWorklistOptions{UniversalQueryMatchesAll: true}, len(records)}} {
		handler, err := NewInMemoryModalityWorklistHandlerWithOptions(records, tc.options)
		if err != nil {
			t.Fatal(err)
		}
		matches := 0
		if err := handler.Find(context.Background(), ModalityWorklistRequest{Identifier: parsed}, func(*object.Object) error { matches++; return nil }); err != nil {
			t.Fatal(err)
		}
		if matches != tc.want {
			t.Errorf("%+v: %d matches, want %d", tc.options, matches, tc.want)
		}
	}
}
