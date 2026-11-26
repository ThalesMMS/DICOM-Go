package dimse

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	dicomencoding "github.com/ThalesMMS/dicom-go/encoding"
	"github.com/ThalesMMS/dicom-go/object"
)

func TestCFindCombinesWildcardUIDListAndDateRangeOnWire(t *testing.T) {
	var index MemoryFindIndex
	for _, row := range []struct{ uid, name, date string }{
		{"1.2.3.11", "ARCHIVE.A^Iles", "20240104"},
		{"1.2.3.12", "ARCHIVE.A^Ines", "20240108"},
		{"1.2.3.13", "ARCHIVE.A^Idnes", "20240106"},
		{"1.2.3.14", "ARCHIVEXA^Iles", "20240106"},
		{"1.2.3.15", "ARCHIVE.A^Iles", "20240109"},
		{"1.2.3.16", "ARCHIVE.A^Ines", "20240106"},
	} {
		index.Candidates = append(index.Candidates, object.FromElements([]core.Element{
			{Header: core.ElementHeader{Tag: tags.StudyInstanceUID, VR: core.VRUI}, Value: core.StringValue{row.uid}},
			{Header: core.ElementHeader{Tag: tags.PatientName, VR: core.VRPN}, Value: core.StringValue{row.name}},
			{Header: core.ElementHeader{Tag: tags.StudyDate, VR: core.VRDA}, Value: core.StringValue{row.date}},
		}, std.Dictionary))
	}
	for _, tc := range []struct {
		name      string
		dateRange string
		uids      string
		want      []string
	}{
		{"closed range includes both endpoints", "20240104-20240108", `1.2.3.15\1.2.3.13\1.2.3.11\1.2.3.12\1.2.3.14`, []string{"1.2.3.11", "1.2.3.12"}},
		{"open start includes before end", "-20240106", `1.2.3.12\1.2.3.16\1.2.3.11`, []string{"1.2.3.11", "1.2.3.16"}},
		{"open end includes after start", "20240108-", `1.2.3.11\1.2.3.15\1.2.3.12`, []string{"1.2.3.12", "1.2.3.15"}},
		{"no candidate satisfies every key", "20240104-20240108", `1.2.3.13\1.2.3.14\1.2.3.15`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			identifier := mustBuild(t, func() ([]core.Element, error) {
				return BuildStudyRootStudyFindKeys(map[string]string{
					"PatientName": "ARCHIVE.*^I?es", "StudyDate": tc.dateRange, "StudyInstanceUID": tc.uids,
				})
			})
			var got []string
			for _, match := range runStudyRootFindAgainstIndex(t, ctx, index, identifier) {
				uid, ok := match.GetString(tags.StudyInstanceUID)
				if !ok {
					t.Fatal("pending result has no Study Instance UID")
				}
				got = append(got, uid)
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("matching UIDs = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCFindDecodesEachPendingIdentifierUsingItsOwnCharacterSet(t *testing.T) {
	// One association can carry successive response identifiers encoded in
	// different repertoires. The SCU must decode each from its own declaration.
	var index MemoryFindIndex
	want := []struct {
		declaration []string
		name        string
	}{
		{[]string{"ISO_IR 100"}, "Érable^Célia"},
		{[]string{"", "ISO 2022 IR 87"}, "=三浦^葵"},
		{nil, "CONTROL^ASCII"},
		{[]string{"ISO_IR 192"}, "=Ελένη^Δέλτα"},
	}
	for _, row := range want {
		candidate := object.New(std.Dictionary)
		var value core.Value = core.StringValue{row.name}
		if row.declaration != nil {
			codec, err := dicomencoding.ParseCharacterSet(row.declaration...)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := codec.EncodePersonName(row.name)
			if err != nil {
				t.Fatal(err)
			}
			candidate.Put(core.Element{Header: core.ElementHeader{Tag: tagMWLSpecificCharacterSet, VR: core.VRCS}, Value: core.StringValue(row.declaration)})
			value = core.RawValue(encoded)
		}
		candidate.Put(core.Element{Header: core.ElementHeader{Tag: tags.PatientName, VR: core.VRPN}, Value: value})
		index.Candidates = append(index.Candidates, candidate)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	identifier := mustBuild(t, func() ([]core.Element, error) {
		return BuildStudyRootStudyFindKeys(nil, "PatientName")
	})
	got := runStudyRootFindAgainstIndex(t, ctx, index, identifier)
	if len(got) != len(want) {
		t.Fatalf("pending identifiers = %d, want %d", len(got), len(want))
	}
	for i, candidate := range got {
		if name, ok := candidate.GetString(tags.PatientName); !ok || name != want[i].name {
			t.Errorf("identifier %d Patient Name = %q, want %q", i, name, want[i].name)
		}
		if declaration, _ := candidate.GetStrings(tagMWLSpecificCharacterSet); !slices.Equal(declaration, want[i].declaration) {
			t.Errorf("identifier %d Specific Character Set = %q, want %q", i, declaration, want[i].declaration)
		}
	}
}
