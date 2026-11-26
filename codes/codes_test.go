package codes_test

import (
	"testing"

	"github.com/ThalesMMS/dicom-go/codes"
	"github.com/ThalesMMS/dicom-go/codes/dcm"
	"github.com/ThalesMMS/dicom-go/codes/sct"
	"github.com/ThalesMMS/dicom-go/codes/ucum"
	"github.com/ThalesMMS/dicom-go/codes/umls"
)

func TestTablesParseWithProvenance(t *testing.T) {
	if err := codes.Err(); err != nil {
		t.Fatal(err)
	}
	edition, sum := codes.Source()
	if edition != "PS3.16 2026d" || len(sum) != 64 {
		t.Fatalf("source %q %q", edition, sum)
	}
}

// TestNamedCodesMatchLookup: the named constants carry the terms Lookup
// returns, including a retired DCM code.
func TestNamedCodesMatchLookup(t *testing.T) {
	for _, code := range []codes.Code{dcm.ImagingMeasurementReport, dcm.Finding, dcm.OfInterest, sct.FindingSite, sct.Area, ucum.Mm, umls.QualitativeEvaluations} {
		term, ok := codes.Lookup(code.Scheme, code.Value)
		if !ok || term.Code != code {
			t.Fatalf("Lookup(%v) = %+v, %v", code, term, ok)
		}
	}
	if term, ok := codes.Lookup("DCM", "AS"); !ok || !term.Retired || term.Meaning != "Angioscopy" {
		t.Fatalf("retired DCM AS = %+v, %v", term, ok)
	}
	if _, ok := codes.Lookup("DCM", "no-such-code"); ok {
		t.Fatal("unknown code found")
	}
	if dcm.Finding.String() != `(121071, DCM, "Finding")` || !dcm.Finding.Equal(codes.Code{Value: "121071", Scheme: "DCM", Meaning: "other"}) {
		t.Fatal("String or Equal")
	}
}

// TestContextGroupsResolveIncludesAndExtensibility uses CID 7464, which
// includes CID 3488 (Min/Max/Mean), and CID 82, defined by reference to
// UCUM.
func TestContextGroupsResolveIncludesAndExtensibility(t *testing.T) {
	group, ok := codes.ContextGroupByID(7464)
	if !ok || group.Keyword != "GeneralRegionOfInterestMeasurementModifier" || !group.Extensible || group.UID != "1.2.840.10008.6.1.951" || group.Version != "20121101" {
		t.Fatalf("CID 7464 %+v", group)
	}
	mean, _ := codes.Lookup("SCT", "373098007")
	if member, decided := group.Contains(mean.Code); !member || !decided {
		t.Fatalf("Mean (from CID 3488) member %v decided %v", member, decided)
	}
	if member, decided := group.Contains(dcm.Finding); member || !decided {
		t.Fatalf("Finding member %v decided %v", member, decided)
	}
	members := group.Members()
	if len(members) != len(group.Codes)+3 {
		t.Fatalf("%d members with the include, %d own", len(members), len(group.Codes))
	}
	if byKeyword, ok := codes.ContextGroupByKeyword("MinMaxMean"); !ok || byKeyword.ID != 3488 || len(byKeyword.Codes) != 3 {
		t.Fatalf("MinMaxMean %+v", byKeyword)
	}
	units, ok := codes.ContextGroupByID(82)
	if !ok || !units.DefinedByReference {
		t.Fatalf("CID 82 %+v", units)
	}
	if _, decided := units.Contains(ucum.Mm); decided {
		t.Fatal("membership of a group defined by reference was decided")
	}
	if _, ok := codes.ContextGroupByID(999999); ok {
		t.Fatal("unknown CID found")
	}
}

// TestMemberTakesTheGroupMeaning: CID 7470 spells (103339001, SCT) Long
// Axis, although the named constant carries CID 6's Long axis.
func TestMemberTakesTheGroupMeaning(t *testing.T) {
	linear, _ := codes.ContextGroupByID(7470)
	term, ok := linear.Member(sct.LongAxis)
	if !ok || term.Meaning != "Long Axis" || sct.LongAxis.Meaning != "Long axis" {
		t.Fatalf("CID 7470 member %+v (%v), constant %+v", term, ok, sct.LongAxis)
	}
	if _, ok := linear.Member(dcm.Finding); ok {
		t.Fatal("Finding is not in CID 7470")
	}
}
