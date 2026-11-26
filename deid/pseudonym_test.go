package deid

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var pseudonymSecret = []byte("synthetic-test-secret-0123456789")

func TestPseudonymTemplateValidation(t *testing.T) {
	cases := []struct {
		name     string
		template PseudonymTemplate
		secret   []byte
		want     error
	}{
		{name: "empty", template: PseudonymTemplate{}, want: ErrInvalidPseudonymTemplate},
		{name: "no token", template: PseudonymTemplate{PatientID: "SAME"}, want: ErrInvalidPseudonymTemplate},
		{name: "unknown token", template: PseudonymTemplate{PatientID: "P-{date}"}, want: ErrInvalidPseudonymTemplate},
		{name: "unterminated", template: PseudonymTemplate{PatientID: "P-{counter"}, want: ErrInvalidPseudonymTemplate},
		{name: "backslash", template: PseudonymTemplate{PatientID: `P\{counter}`}, want: ErrInvalidPseudonymTemplate},
		{name: "caret in ID", template: PseudonymTemplate{PatientID: "P^{counter}"}, want: ErrInvalidPseudonymTemplate},
		{name: "non ASCII", template: PseudonymTemplate{PatientName: "PACÍENTE^{counter}"}, want: ErrInvalidPseudonymTemplate},
		{name: "too long", template: PseudonymTemplate{PatientID: strings.Repeat("X", 60) + "{hash:8}"}, secret: pseudonymSecret, want: ErrInvalidPseudonymTemplate},
		{name: "counter width", template: PseudonymTemplate{PatientID: "{counter:0}"}, want: ErrInvalidPseudonymTemplate},
		{name: "hash without secret", template: PseudonymTemplate{PatientID: "{hash}"}, want: ErrPseudonymSecretRequired},
		{name: "hash with short secret", template: PseudonymTemplate{PatientID: "{hash}"}, secret: []byte("short"), want: ErrPseudonymSecretRequired},
		{name: "valid counter", template: PseudonymTemplate{PatientName: "ANON^SUBJECT {counter:3}", PatientID: "S{counter:3}"}},
		{name: "valid hash", template: PseudonymTemplate{PatientName: "ANON^{hash:8}", PatientID: "PSN-{hash}"}, secret: pseudonymSecret},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewPseudonymizer(test.template, test.secret)
			if !errors.Is(err, test.want) || (test.want == nil && err != nil) {
				t.Fatalf("NewPseudonymizer error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestPseudonymsAreDeterministicPerIdentityAndSecret(t *testing.T) {
	template := PseudonymTemplate{PatientName: "ANON^{hash:10}", PatientID: "PSN-{hash:16}"}
	first, err := NewPseudonymizer(template, pseudonymSecret)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewPseudonymizer(template, append([]byte(nil), pseudonymSecret...))
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewPseudonymizer(template, []byte("another-synthetic-secret-abcdefgh"))
	if err != nil {
		t.Fatal(err)
	}
	a1, _ := first.Pseudonym("id\x00\x00SYN-A")
	b1, _ := first.Pseudonym("id\x00\x00SYN-B")
	b2, _ := second.Pseudonym("id\x00\x00SYN-B") // different order, same secret
	a2, _ := second.Pseudonym("id\x00\x00SYN-A")
	again, _ := first.Pseudonym("id\x00\x00SYN-A")
	otherA, _ := other.Pseudonym("id\x00\x00SYN-A")
	if a1 != a2 || b1 != b2 || a1 != again {
		t.Fatalf("keyed pseudonyms are not deterministic: %v %v %v %v", a1, a2, b1, b2)
	}
	if a1 == b1 || a1.PatientID == otherA.PatientID {
		t.Fatalf("pseudonyms do not separate identities or secrets: %v %v %v", a1, b1, otherA)
	}
	if !strings.HasPrefix(a1.PatientID, "PSN-") || len(a1.PatientID) != 20 || !strings.HasPrefix(a1.PatientName, "ANON^") {
		t.Fatalf("rendered pseudonym = %+v", a1)
	}
	if strings.Contains(a1.PatientID, "SYN") {
		t.Fatal("pseudonym contains the original identity")
	}
}

func TestPseudonymCountersFollowFirstSightAndStayStable(t *testing.T) {
	p, err := NewPseudonymizer(PseudonymTemplate{PatientName: "ANON^SUBJECT {counter:3}", PatientID: "SUBJ-{counter:3}"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for index, identity := range []string{"x", "y", "x", "z", "y"} {
		got, err := p.Pseudonym(identity)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"x": "SUBJ-001", "y": "SUBJ-002", "z": "SUBJ-003"}[identity]
		if got.PatientID != want {
			t.Fatalf("call %d identity %q = %+v, want %s", index, identity, got, want)
		}
	}
}

func TestPseudonymCollisionsFailInsteadOfMergingPatients(t *testing.T) {
	// One hexadecimal digit has 16 values, so 17 identities must collide.
	p, err := NewPseudonymizer(PseudonymTemplate{PatientID: "P{hash:1}"}, pseudonymSecret)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	var collision error
	for index := 0; index < 17 && collision == nil; index++ {
		identity := fmt.Sprintf("SYN-PATIENT-%02d", index)
		got, err := p.Pseudonym(identity)
		if err != nil {
			collision = err
			if strings.Contains(err.Error(), "SYN-PATIENT") {
				t.Fatalf("collision error leaks identity: %v", err)
			}
			break
		}
		if owner, ok := seen[got.PatientID]; ok && owner != identity {
			t.Fatalf("two identities share %s", got.PatientID)
		}
		seen[got.PatientID] = identity
	}
	if !errors.Is(collision, ErrPseudonymCollision) {
		t.Fatalf("collision error = %v, want ErrPseudonymCollision", collision)
	}
	// An identity that was assigned before the collision keeps its value.
	for pseudonym, identity := range seen {
		got, err := p.Pseudonym(identity)
		if err != nil || got.PatientID != pseudonym {
			t.Fatalf("assigned pseudonym changed after a collision: %v %v", got, err)
		}
	}
}

func TestBasicProfileAppliesPseudonymsWithoutReportingValues(t *testing.T) {
	pseudonymizer, err := NewPseudonymizer(PseudonymTemplate{PatientName: "ANON^{hash:8}", PatientID: "PSN-{hash:12}"}, pseudonymSecret)
	if err != nil {
		t.Fatal(err)
	}
	options := DefaultBasicProfileOptions()
	options.Pseudonymizer = pseudonymizer
	first, _ := redactionTestFile(t, transfer.ExplicitVRLittleEndian)
	second, _ := redactionTestFile(t, transfer.ExplicitVRLittleEndian)
	uids := NewUIDRemapper()
	clone1, report, err := CloneFileWithBasicProfile(context.Background(), first, options, uids)
	if err != nil {
		t.Fatal(err)
	}
	clone2, _, err := CloneFileWithBasicProfile(context.Background(), second, options, uids)
	if err != nil {
		t.Fatal(err)
	}
	name1, _ := clone1.Dataset.GetString(tagPatientName)
	id1, _ := clone1.Dataset.GetString(tagPatientID)
	id2, _ := clone2.Dataset.GetString(tagPatientID)
	want, _ := pseudonymizer.Pseudonym(PatientPseudonymIdentity(first.Dataset))
	if strings.TrimSpace(id1) != want.PatientID || strings.TrimSpace(name1) != want.PatientName || id1 != id2 {
		t.Fatalf("pseudonyms = %q/%q and %q, want %+v for the same patient", name1, id1, id2, want)
	}
	if report.Summary.PatientPseudonyms != 1 {
		t.Fatalf("PatientPseudonyms = %d", report.Summary.PatientPseudonyms)
	}
	if values, _ := clone1.Dataset.GetStrings(tagDeidentificationMethod); !containsString(values, "Template pseudonyms for patient name and ID") {
		t.Fatalf("De-identification Method = %q", values)
	}
	if removed, _ := clone1.Dataset.GetString(core.NewTag(0x0012, 0x0062)); strings.TrimSpace(removed) != "YES" {
		t.Fatalf("Patient Identity Removed = %q", removed)
	}
	encoded, _ := json.Marshal(report)
	for _, value := range []string{want.PatientID, want.PatientName, redactionPID, "SYNTHETIC"} {
		if bytes.Contains(encoded, []byte(value)) {
			t.Fatalf("report contains %q", value)
		}
	}
}

func TestPatientPseudonymIdentityFallsBackWithoutPatientID(t *testing.T) {
	withID := object.FromElements([]core.Element{core.NewRawElement(tagPatientID, core.VRLO, padText("A1"))}, nil)
	withIssuer := object.FromElements([]core.Element{
		core.NewRawElement(tagPatientID, core.VRLO, padText("A1")),
		core.NewRawElement(tagIssuerOfPatientID, core.VRLO, padText("SITE")),
	}, nil)
	byName := object.FromElements([]core.Element{core.NewRawElement(tagPatientName, core.VRPN, padText("A1"))}, nil)
	keys := map[string]bool{PatientPseudonymIdentity(withID): true, PatientPseudonymIdentity(withIssuer): true, PatientPseudonymIdentity(byName): true}
	if len(keys) != 3 {
		t.Fatalf("identity keys are not distinct: %d", len(keys))
	}
	if PatientPseudonymIdentity(object.FromElements(nil, nil)) != "" {
		t.Fatal("empty object identity is not empty")
	}
}
