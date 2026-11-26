package sr

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// dcmsr_TID1204_LanguageOfContentItemAndDescendants allows an inline locale
// or a language with a separate country child. Use the generic SR tree to
// exercise both forms through the standard template validator and wire path.
func TestDCMTKReferenceLanguageTemplate(t *testing.T) {
	key, ok := StandardTemplateKey(1204)
	if !ok {
		t.Fatal("TID 1204 is absent from the standard catalog")
	}
	registry, err := NewStandardTemplateRegistry(1204)
	if err != nil {
		t.Fatal(err)
	}
	validator, err := NewTemplateValidator(registry, TemplateValidationOptions{Mode: ValidationModeStrict})
	if err != nil {
		t.Fatal(err)
	}
	language := ContentItem{ValueType: ValueCode, RelationshipType: RelationshipHasConceptMod,
		ConceptName: CodedEntry{"121049", "DCM", "Language of Content Item and Descendants"}, Code: CodedEntry{"deu", "RFC5646", "German"}}
	country := ContentItem{ValueType: ValueCode, RelationshipType: RelationshipHasConceptMod,
		ConceptName: CodedEntry{"121046", "DCM", "Country of Language"}, Code: CodedEntry{"DE", "ISO3166_1", "Germany"}}
	locale := language
	locale.Code = CodedEntry{"de-DE", "RFC5646", "German (DE)"}
	withCountry := language
	withCountry.Children = []ContentItem{country}
	duplicateCountry := language
	duplicateCountry.Children = []ContentItem{country, country}
	for _, tc := range []struct {
		name    string
		content []ContentItem
		finding string
	}{
		{"regional-code", []ContentItem{locale}, ""},
		{"separate-country-child", []ContentItem{withCountry}, ""},
		{"empty-template", nil, TemplateCodeCardinality},
		{"country-as-sibling", []ContentItem{language, country}, TemplateCodeUnknownContent},
		{"duplicate-country", []ContentItem{duplicateCountry}, TemplateCodeCardinality},
	} {
		t.Run(tc.name, func(t *testing.T) {
			document := &Document{SOPClassUID: EnhancedSRStorage, SOPInstanceUID: "1.2.3.93", StudyInstanceUID: testStudy,
				Title: CodedEntry{"121111", "DCM", "Summary"}, ContentDate: "20261010", ContentTime: "120000", Content: tc.content}
			var encoded bytes.Buffer
			syntax := transfer.ImplicitVRLittleEndian
			if err := object.WriteDataSet(&encoded, mustDocumentDataset(t, document), syntax); err != nil {
				t.Fatal(err)
			}
			dataset, err := object.ReadDataSet(bytes.NewReader(encoded.Bytes()), syntax)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := ReadDocument(dataset)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded.Content, tc.content) {
				t.Fatalf("decoded language content = %+v, want %+v", decoded.Content, tc.content)
			}
			report, err := validator.Validate(context.Background(), decoded, key)
			if tc.finding == "" {
				if err != nil || len(report.Findings) != 0 {
					t.Fatalf("valid TID 1204: %v, %+v", err, report.Findings)
				}
				return
			}
			if !errors.Is(err, ErrTemplateValidation) {
				t.Fatalf("invalid TID 1204: %v, %+v", err, report.Findings)
			}
			assertTemplateFinding(t, report, tc.finding)
		})
	}
}
