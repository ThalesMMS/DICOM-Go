package object

import (
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
)

// Synthetic identifiers only; none of these values come from a patient.

func issuerQualifiers(universalID, universalIDType string) core.Element {
	item := FromElements([]core.Element{
		newStringElement(tagIssuerQualifierUniversalID, core.VRUT, universalID),
		newStringElement(tagIssuerQualifierUniversalIDType, core.VRCS, universalIDType),
	}, std.Dictionary)
	return SequenceElement(tagIssuerOfPatientIDQualifiersSeq, item)
}

func TestIssuerMatches(t *testing.T) {
	tests := []struct {
		name        string
		left, right Issuer
		want        bool
	}{
		{"both unknown", Issuer{}, Issuer{}, true},
		{"one side unknown", Issuer{LocalNamespaceEntityID: "SITE_A"}, Issuer{}, true},
		{"other side unknown", Issuer{}, Issuer{UniversalEntityID: "2.25.1", UniversalEntityIDType: "ISO"}, true},
		{"same namespace", Issuer{LocalNamespaceEntityID: "SITE_A"}, Issuer{LocalNamespaceEntityID: " SITE_A "}, true},
		{"different namespace", Issuer{LocalNamespaceEntityID: "SITE_A"}, Issuer{LocalNamespaceEntityID: "SITE_B"}, false},
		{"namespace is case sensitive", Issuer{LocalNamespaceEntityID: "SITE_A"}, Issuer{LocalNamespaceEntityID: "site_a"}, false},
		{"same universal id decides over namespace",
			Issuer{LocalNamespaceEntityID: "A", UniversalEntityID: "2.25.1", UniversalEntityIDType: "ISO"},
			Issuer{LocalNamespaceEntityID: "Site A", UniversalEntityID: "2.25.1", UniversalEntityIDType: "ISO"}, true},
		{"different universal id",
			Issuer{LocalNamespaceEntityID: "A", UniversalEntityID: "2.25.1", UniversalEntityIDType: "ISO"},
			Issuer{LocalNamespaceEntityID: "A", UniversalEntityID: "2.25.2", UniversalEntityIDType: "ISO"}, false},
		{"different universal id type",
			Issuer{UniversalEntityID: "example.org", UniversalEntityIDType: "DNS"},
			Issuer{UniversalEntityID: "example.org", UniversalEntityIDType: "URI"}, false},
		{"universal id type missing on one side",
			Issuer{UniversalEntityID: "2.25.1", UniversalEntityIDType: "ISO"},
			Issuer{UniversalEntityID: "2.25.1"}, true},
		{"universal on one side, namespaces agree",
			Issuer{LocalNamespaceEntityID: "A", UniversalEntityID: "2.25.1", UniversalEntityIDType: "ISO"},
			Issuer{LocalNamespaceEntityID: "A"}, true},
		{"universal on one side, namespaces differ",
			Issuer{LocalNamespaceEntityID: "A", UniversalEntityID: "2.25.1", UniversalEntityIDType: "ISO"},
			Issuer{LocalNamespaceEntityID: "B"}, false},
		{"no comparable component",
			Issuer{UniversalEntityID: "2.25.1", UniversalEntityIDType: "ISO"},
			Issuer{LocalNamespaceEntityID: "A"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.left.Matches(tt.right); got != tt.want {
				t.Fatalf("Matches(%+v, %+v) = %v, want %v", tt.left, tt.right, got, tt.want)
			}
			if got := tt.right.Matches(tt.left); got != tt.want {
				t.Fatalf("Matches is not symmetric for %+v and %+v", tt.left, tt.right)
			}
		})
	}
}

func TestIssuerStringRoundTrip(t *testing.T) {
	tests := []struct {
		issuer Issuer
		want   string
	}{
		{Issuer{}, ""},
		{Issuer{LocalNamespaceEntityID: " SITE_A "}, "SITE_A"},
		{Issuer{LocalNamespaceEntityID: "SITE_A", UniversalEntityID: "2.25.1", UniversalEntityIDType: "ISO"}, "SITE_A&2.25.1&ISO"},
		{Issuer{UniversalEntityID: "2.25.1", UniversalEntityIDType: "ISO"}, "&2.25.1&ISO"},
		{Issuer{LocalNamespaceEntityID: `R&D`, UniversalEntityID: `urn:x\T\y`, UniversalEntityIDType: "URI"}, `R\T\D&urn:x\E\T\E\y&URI`},
	}
	for _, tt := range tests {
		got := tt.issuer.String()
		if got != tt.want {
			t.Fatalf("String(%+v) = %q, want %q", tt.issuer, got, tt.want)
		}
		if parsed := ParseIssuer(got); parsed != tt.issuer.normalized() {
			t.Fatalf("ParseIssuer(%q) = %+v, want %+v", got, parsed, tt.issuer.normalized())
		}
	}
}

func TestIDWithIssuerMatches(t *testing.T) {
	siteA := Issuer{LocalNamespaceEntityID: "SITE_A"}
	siteB := Issuer{LocalNamespaceEntityID: "SITE_B"}
	tests := []struct {
		name        string
		left, right IDWithIssuer
		want        bool
	}{
		{"same id same issuer", IDWithIssuer{ID: "PID-1", Issuer: siteA}, IDWithIssuer{ID: "PID-1 ", Issuer: siteA}, true},
		{"same id different issuer", IDWithIssuer{ID: "PID-1", Issuer: siteA}, IDWithIssuer{ID: "PID-1", Issuer: siteB}, false},
		{"same id one issuer unknown", IDWithIssuer{ID: "PID-1", Issuer: siteA}, IDWithIssuer{ID: "PID-1"}, true},
		{"different id", IDWithIssuer{ID: "PID-1", Issuer: siteA}, IDWithIssuer{ID: "PID-2", Issuer: siteA}, false},
		{"empty id never matches", IDWithIssuer{}, IDWithIssuer{}, false},
		{"type of patient id is not identity", IDWithIssuer{ID: "PID-1", TypeOfPatientID: "TEXT"}, IDWithIssuer{ID: "PID-1", TypeOfPatientID: "BARCODE"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.left.Matches(tt.right); got != tt.want {
				t.Fatalf("Matches = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPatientIDsWithIssuer(t *testing.T) {
	otherItem := func(id, issuer string, qualifiers *core.Element) *Object {
		elements := []core.Element{newStringElement(tagPatientIDValue, core.VRLO, id)}
		if issuer != "" {
			elements = append(elements, newStringElement(tagIssuerOfPatientID, core.VRLO, issuer))
		}
		if qualifiers != nil {
			elements = append(elements, *qualifiers)
		}
		return FromElements(elements, std.Dictionary)
	}
	qualifiersB := issuerQualifiers("2.25.200", "ISO")
	tests := []struct {
		name    string
		dataset *Object
		primary IDWithIssuer
		ok      bool
		all     []IDWithIssuer
	}{
		{
			name:    "no patient id",
			dataset: FromElements(nil, std.Dictionary),
		},
		{
			name: "id without issuer",
			dataset: FromElements([]core.Element{
				newStringElement(tagPatientIDValue, core.VRLO, "PID-1 "),
			}, std.Dictionary),
			primary: IDWithIssuer{ID: "PID-1"},
			ok:      true,
			all:     []IDWithIssuer{{ID: "PID-1"}},
		},
		{
			name: "full issuer, type and other ids",
			dataset: FromElements([]core.Element{
				newStringElement(tagPatientIDValue, core.VRLO, "PID-1"),
				newStringElement(tagIssuerOfPatientID, core.VRLO, "SITE_A"),
				newStringElement(tagTypeOfPatientID, core.VRCS, "TEXT"),
				issuerQualifiers("2.25.100", "ISO"),
				SequenceElement(tagOtherPatientIDsSequence,
					otherItem("PID-9", "SITE_B", &qualifiersB),
					otherItem("", "SITE_C", nil),
					otherItem("PID-1", "SITE_A", nil),
					otherItem("PID-9", "SITE_B", &qualifiersB),
				),
			}, std.Dictionary),
			primary: IDWithIssuer{ID: "PID-1", TypeOfPatientID: "TEXT", Issuer: Issuer{LocalNamespaceEntityID: "SITE_A", UniversalEntityID: "2.25.100", UniversalEntityIDType: "ISO"}},
			ok:      true,
			all: []IDWithIssuer{
				{ID: "PID-1", TypeOfPatientID: "TEXT", Issuer: Issuer{LocalNamespaceEntityID: "SITE_A", UniversalEntityID: "2.25.100", UniversalEntityIDType: "ISO"}},
				{ID: "PID-9", Issuer: Issuer{LocalNamespaceEntityID: "SITE_B", UniversalEntityID: "2.25.200", UniversalEntityIDType: "ISO"}},
				{ID: "PID-1", Issuer: Issuer{LocalNamespaceEntityID: "SITE_A"}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			primary, ok := PatientIDWithIssuer(tt.dataset)
			if ok != tt.ok || primary != tt.primary {
				t.Fatalf("PatientIDWithIssuer = %+v, %v; want %+v, %v", primary, ok, tt.primary, tt.ok)
			}
			if all := AllPatientIDsWithIssuer(tt.dataset); !reflect.DeepEqual(all, tt.all) {
				t.Fatalf("AllPatientIDsWithIssuer = %+v, want %+v", all, tt.all)
			}
		})
	}
	if _, ok := PatientIDWithIssuer(nil); ok {
		t.Fatal("PatientIDWithIssuer(nil) reported an identifier")
	}
}

func TestAnyPatientIDMatchesUsesOtherPatientIDs(t *testing.T) {
	siteA := Issuer{LocalNamespaceEntityID: "SITE_A"}
	siteB := Issuer{LocalNamespaceEntityID: "SITE_B"}
	left := []IDWithIssuer{{ID: "PID-1", Issuer: siteA}, {ID: "PID-9", Issuer: siteB}}
	tests := []struct {
		name  string
		right []IDWithIssuer
		want  bool
	}{
		{"matches through other patient id", []IDWithIssuer{{ID: "PID-9", Issuer: siteB}}, true},
		{"same id under another issuer", []IDWithIssuer{{ID: "PID-1", Issuer: siteB}}, false},
		{"no identifiers", nil, false},
	}
	for _, tt := range tests {
		if got := AnyPatientIDMatches(left, tt.right); got != tt.want {
			t.Fatalf("%s: AnyPatientIDMatches = %v, want %v", tt.name, got, tt.want)
		}
	}
}
