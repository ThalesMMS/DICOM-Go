package encapdoc

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

func TestNewCDARejectsConflictingDeclaredIdentity(t *testing.T) {
	data := []byte(syntheticCDA)
	for _, identifier := range []string{"2.25.12345^doc-7", "2.25.1051001^PRIVATE-MARKER", "2.25.1051001", "2.25.1051001^DOC-7"} {
		t.Run(identifier, func(t *testing.T) {
			file, err := NewCDA(CDAOptions{
				DocumentOptions:       documentOptions("2.25.1051020", data),
				HL7InstanceIdentifier: identifier,
			})
			if file != nil || !errors.Is(err, ErrCDAIdentityMismatch) || !errors.Is(err, ErrInvalidDocument) || strings.Contains(err.Error(), "PRIVATE-MARKER") {
				t.Fatalf("NewCDA returned a file=%t, error=%v", file != nil, err)
			}
		})
	}

	file, err := NewCDA(CDAOptions{DocumentOptions: documentOptions("2.25.1051020", data), HL7InstanceIdentifier: "2.25.1051001^doc-7"})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Extract(file.Dataset)
	if err != nil || !bytes.Equal(doc.Data, data) || doc.HL7InstanceIdentifier != "2.25.1051001^doc-7" {
		t.Fatalf("matching CDA identity or bytes changed: %v", err)
	}
}

func TestParseEncapsulatedCDANarrativeChecksEnvelopeIdentifier(t *testing.T) {
	const uuid = "21EC2020-3AEA-4069-A2DD-08002B30309D"
	data := cdaNarrativeDocument("<paragraph>Reviewed clinical narrative.</paragraph>")
	data = bytes.Replace(data, []byte(`<id root="1.2.3"/>`), []byte(`<id root="`+uuid+`" extension="SYNTHETIC"/>`), 1)
	for _, test := range []struct {
		name, declared string
		wantErr        error
	}{
		{name: "no envelope identifier"},
		{name: "exact", declared: uuid + "^SYNTHETIC"},
		{name: "uuid case and ST padding", declared: strings.ToLower(uuid) + "^SYNTHETIC "},
		{name: "leading space is significant", declared: " " + uuid + "^SYNTHETIC", wantErr: ErrCDAIdentityMismatch},
		{name: "extension case is significant", declared: uuid + "^synthetic", wantErr: ErrCDAIdentityMismatch},
		{name: "extension omitted", declared: uuid, wantErr: ErrCDAIdentityMismatch},
	} {
		t.Run(test.name, func(t *testing.T) {
			model, err := ParseEncapsulatedCDANarrative(nil, data, test.declared, CDANarrativeLimits{})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("parse error = %v, want %v", err, test.wantErr)
			}
			if test.wantErr != nil {
				if model.Title != "" || model.HL7InstanceIdentifier != "" || model.PlainText != "" || len(model.Blocks) != 0 {
					t.Fatal("mismatched CDA returned a partial narrative")
				}
				return
			}
			if model.HL7InstanceIdentifier != uuid+"^SYNTHETIC" || !strings.Contains(model.PlainText, "Reviewed clinical narrative.") {
				t.Fatal("narrative did not retain XML identity and content")
			}
		})
	}
}

func TestParseEncapsulatedCDANarrativeRejectsAmbiguousIdentityAttributes(t *testing.T) {
	for _, id := range []string{
		`<id root="2.25.1" root="1.2.3"/>`,
		`<id root="1.2.3" extension="" extension="SYNTHETIC"/>`,
	} {
		data := bytes.Replace(cdaNarrativeDocument("Reviewed clinical narrative."), []byte(`<id root="1.2.3"/>`), []byte(id), 1)
		if model, err := ParseEncapsulatedCDANarrative(nil, data, "", CDANarrativeLimits{}); !errors.Is(err, ErrInvalidDocument) || model.PlainText != "" {
			t.Fatalf("ambiguous document ID returned a narrative: %v", err)
		}
	}
}

func TestParseEncapsulatedCDANarrativeAcceptsOIDWithUUIDLength(t *testing.T) {
	const oid = "1.2.826.0.1.3680043.10.1470.12345678"
	data := bytes.Replace(cdaNarrativeDocument("Reviewed clinical narrative."), []byte("1.2.3"), []byte(oid), 1)
	if model, err := ParseEncapsulatedCDANarrative(nil, data, oid, CDANarrativeLimits{}); err != nil || model.HL7InstanceIdentifier != oid {
		t.Fatalf("matching OID was refused: %v", err)
	}
}

func TestCDAEnvelopeIdentifierRejectsUnreadablePresentElement(t *testing.T) {
	dataset := object.New(nil)
	if got, err := CDAEnvelopeIdentifier(dataset); err != nil || got != "" {
		t.Fatalf("absent element returned %q, error=%v", got, err)
	}
	dataset.Put(core.Element{
		Header: core.ElementHeader{Tag: tagHL7InstanceIdentifier, VR: core.VRST},
		Value:  core.StringValue{"2.25.1", "2.25.2"},
	})
	if got, err := CDAEnvelopeIdentifier(dataset); !errors.Is(err, ErrInvalidDocument) || got != "" {
		t.Fatalf("malformed multiplicity returned %q, error=%v", got, err)
	}
	dataset.Put(stringElement(tagHL7InstanceIdentifier, core.VRUI, "2.25.1"))
	if got, err := CDAEnvelopeIdentifier(dataset); !errors.Is(err, ErrInvalidDocument) || got != "" {
		t.Fatalf("wrong VR returned %q, error=%v", got, err)
	}
}
