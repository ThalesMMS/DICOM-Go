package qrmatch

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
)

func TestExtendedEmptyMatchUsesWholeEligibleKey(t *testing.T) {
	tests := []struct {
		name             string
		query            core.Element
		candidate        []core.Element
		negotiated, want bool
	}{
		{"absent attribute", str(tags.AccessionNumber, core.VRSH, `""`), nil, true, true},
		{"explicit empty", str(tags.AccessionNumber, core.VRSH, `""`), []core.Element{str(tags.AccessionNumber, core.VRSH, "")}, true, true},
		{"quoted literal without negotiation", str(tags.AccessionNumber, core.VRSH, `""`), []core.Element{str(tags.AccessionNumber, core.VRSH, `""`)}, false, true},
		{"quoted literal with negotiation", str(tags.AccessionNumber, core.VRSH, `""`), []core.Element{str(tags.AccessionNumber, core.VRSH, `""`)}, true, false},
		{"quotes and wildcard", str(tags.AccessionNumber, core.VRSH, `""*`), []core.Element{str(tags.AccessionNumber, core.VRSH, `""`)}, true, true},
		{"universal return key", str(tags.AccessionNumber, core.VRSH, ""), []core.Element{str(tags.AccessionNumber, core.VRSH, "A1")}, true, true},
		{"quotes among multiple values", str(tags.ModalitiesInStudy, core.VRCS, "CT", `""`), []core.Element{str(tags.ModalitiesInStudy, core.VRCS, "CT", "")}, true, false},
		{"other quote characters", str(tags.PatientName, core.VRPN, "“”"), nil, true, false},
		{"ineligible UID", str(tags.StudyInstanceUID, core.VRUI, `""`), nil, true, false},
		{"nonempty PN delimiters", str(tags.PatientName, core.VRPN, `""`), []core.Element{str(tags.PatientName, core.VRPN, "^^")}, true, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := Match(dataset(test.query), dataset(test.candidate...), Options{
				EmptyValueMatching:    test.negotiated,
				MultipleValueMatching: test.negotiated,
			})
			if err != nil || result.Match != test.want {
				t.Fatalf("Match = %+v, %v; want match %v", result, err, test.want)
			}
		})
	}
}

func TestExtendedEmptyMatchKeepsDateTimeIndependent(t *testing.T) {
	query := dataset(str(tags.StudyDate, core.VRDA, "20240201"), str(tags.StudyTime, core.VRTM, `""`))
	options := Options{EmptyValueMatching: true, CombinedDateTime: true}
	result, err := Match(query, dataset(str(tags.StudyDate, core.VRDA, "20240201")), options)
	if err != nil || !result.Match {
		t.Fatalf("matching day with absent StudyTime = %+v, %v", result, err)
	}
	result, err = Match(query, dataset(str(tags.StudyDate, core.VRDA, "20240202")), options)
	if err != nil || result.Match {
		t.Fatalf("different day with absent StudyTime = %+v, %v", result, err)
	}
}

func TestExtendedMultipleMatchRequiresAllValuesOfEligibleKey(t *testing.T) {
	tests := []struct {
		name             string
		query, candidate core.Element
		negotiated, want bool
		wantErr          error
	}{
		{"overlap preserves baseline OR", str(tags.ModalitiesInStudy, core.VRCS, "CT", "MR"), str(tags.ModalitiesInStudy, core.VRCS, "CT"), false, true, nil},
		{"partial overlap fails AND", str(tags.ModalitiesInStudy, core.VRCS, "CT", "MR"), str(tags.ModalitiesInStudy, core.VRCS, "CT"), true, false, nil},
		{"matches in another order with extra value", str(tags.ModalitiesInStudy, core.VRCS, "CT", "MR"), str(tags.ModalitiesInStudy, core.VRCS, "MR", "PT", "CT"), true, true, nil},
		{"UID list retains OR", str(tags.StudyInstanceUID, core.VRUI, "1.2.3", "1.2.4"), str(tags.StudyInstanceUID, core.VRUI, "1.2.4"), true, true, nil},
		{"VM one key keeps baseline semantics", str(tags.Modality, core.VRCS, "CT", "MR"), str(tags.Modality, core.VRCS, "CT"), true, true, nil},
		{"single key wildcard still matches", str(tags.ModalitiesInStudy, core.VRCS, "M*"), str(tags.ModalitiesInStudy, core.VRCS, "MR"), true, true, nil},
		{"wildcard in negotiated list rejected even with no value", str(tags.ModalitiesInStudy, core.VRCS, "CT", "M*"), core.Element{}, true, false, ErrInvalidQuery},
		{"empty component in negotiated list rejected", str(tags.ModalitiesInStudy, core.VRCS, "CT", ""), core.Element{}, true, false, ErrInvalidQuery},
		{"nonempty wire encoding of an all-empty list rejected", str(tags.ModalitiesInStudy, core.VRCS, "", ""), core.Element{}, true, false, ErrInvalidQuery},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := dataset()
			if test.candidate.Value != nil {
				candidate.Put(test.candidate)
			}
			result, err := Match(dataset(test.query), candidate, Options{MultipleValueMatching: test.negotiated})
			if !errors.Is(err, test.wantErr) || result.Match != test.want {
				t.Fatalf("Match = %+v, %v; want %v, %v", result, err, test.want, test.wantErr)
			}
		})
	}
}

func TestExtendedMultipleMatchLimitsRawKeysBeforeSplitting(t *testing.T) {
	rawList := core.NewRawElement(tags.ModalitiesInStudy, core.VRCS, []byte(strings.Repeat(`CT\`, 64)+"MR"))
	options := Options{MultipleValueMatching: true, Limits: Limits{MaxValueBytes: 64}}
	if _, err := Match(dataset(rawList), dataset(), options); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("raw multiple matching key limit = %v", err)
	}
}

func TestExtendedMultipleMatchAttributeTagsUsesValueByteOrder(t *testing.T) {
	key := core.NewTag(0x0028, 0x0009) // Frame Increment Pointer, AT (1-n).
	first, second, extra := tags.PatientID, tags.PatientName, tags.StudyInstanceUID
	query := dataset(core.NewRawElement(key, core.VRAT, []byte{0x00, 0x10, 0x00, 0x20, 0x00, 0x10, 0x00, 0x10}))
	query.SetValueByteOrder(binary.BigEndian)
	candidate := dataset(core.Element{Header: core.ElementHeader{Tag: key, VR: core.VRAT}, Value: core.TagValue{second, extra, first}})
	result, err := Match(query, candidate, Options{MultipleValueMatching: true})
	if err != nil || !result.Match {
		t.Fatalf("all AT values = %+v, %v", result, err)
	}
	candidate.Put(core.Element{Header: core.ElementHeader{Tag: key, VR: core.VRAT}, Value: core.TagValue{second, extra}})
	result, err = Match(query, candidate, Options{MultipleValueMatching: true})
	if err != nil || result.Match {
		t.Fatalf("partial AT values = %+v, %v", result, err)
	}
	if _, err := Match(query, candidate, Options{MultipleValueMatching: true, Limits: Limits{MaxValues: 1}}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("AT values limit = %v", err)
	}
	query.Put(core.NewRawElement(key, core.VRAT, []byte{0x00, 0x10}))
	if _, err := Match(query, candidate, Options{MultipleValueMatching: true}); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("malformed raw AT = %v", err)
	}
}
