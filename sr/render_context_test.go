package sr

import (
	"bytes"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func contextRenderFixture() (*Document, TemplateValidationMetadata) {
	title, tc := NewCode("synthetic-long-title-code", "99TITLE", "Title")
	concept, cc := NewCode("urn:example:concept", "", "Concept")
	value, vc := NewCode("synthetic-long-value-code", "99VALUE", "Value")
	units, uc := NewCode("synthetic-long-units-code", "99UNITS", "Units")
	qualifier, qc := NewCode("urn:example:qualifier", "99QUAL", "Qualifier")
	tc.CodingSchemeVersion, vc.CodingSchemeVersion, uc.CodingSchemeVersion, qc.CodingSchemeVersion = "V1", "V2", "V3", "V4"
	doc := &Document{SOPClassUID: ComprehensiveSRStorage, ContentDate: "20261009", ContentTime: "120000", Title: title,
		Content: []ContentItem{
			{ValueType: ValueCode, RelationshipType: RelationshipContains, ConceptName: concept, Code: value},
			{ValueType: ValueNum, RelationshipType: RelationshipContains, ConceptName: CodedEntry{CodeValue: "SIZE", CodingSchemeDesignator: "99TEST", CodeMeaning: "Size"}, Measurement: &Measurement{Value: 12.5, Units: units}, NumericValueQualifier: qualifier},
			{ValueType: ValueNum, RelationshipType: RelationshipContains, ConceptName: CodedEntry{CodeValue: "ABSENT", CodingSchemeDesignator: "99TEST", CodeMeaning: "Absent"}, NumericValueQualifier: qualifier},
			{ValueType: ValueCode, RelationshipType: RelationshipContains, Code: CodedEntry{CodeValue: "SHORT", CodingSchemeDesignator: "99TEST", CodeMeaning: "Short meaning"}},
		}}
	metadata := TemplateValidationMetadata{CodeContexts: []ContentItemCodeContexts{
		{Path: "1", ConceptName: tc}, {Path: "1.1", ConceptName: cc, Value: vc},
		{Path: "1.2", MeasurementUnits: uc, NumericValueQualifier: qc}, {Path: "1.3", NumericValueQualifier: qc},
		{Path: "1.4", Value: CodeContext{CodingSchemeVersion: "V5"}},
	}}
	return doc, metadata
}

func TestContextRenderShowsIdentifiersInEveryCodeRole(t *testing.T) {
	doc, metadata := contextRenderFixture()
	snapshot := mustSnapshot(t, doc, metadata)
	rendered, err := RenderItemsWithMetadataSnapshot(doc, snapshot, RenderOptions{ShowCodes: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Title (synthetic-long-title-code, 99TITLE, V1)",
		"Concept (urn:example:concept)",
		"Value (synthetic-long-value-code, 99VALUE, V2)",
		"12.5 Units (synthetic-long-units-code, 99UNITS, V3) [qualifier: Qualifier (urn:example:qualifier, 99QUAL, V4)]",
		"Qualifier (urn:example:qualifier, 99QUAL, V4)",
		"Short meaning (SHORT, 99TEST, V5)",
	}
	got := []string{rendered.Title, rendered.Items[0].Concept, rendered.Items[0].Value, rendered.Items[1].Value, rendered.Items[2].Value, rendered.Items[3].Value}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("role %d = %q, want %q", i, got[i], want[i])
		}
	}
	for _, render := range []func(*bytes.Buffer) error{
		func(b *bytes.Buffer) error {
			return RenderTextWithMetadataSnapshot(b, doc, snapshot, RenderOptions{ShowCodes: true})
		},
		func(b *bytes.Buffer) error {
			return RenderHTMLWithMetadataSnapshot(b, doc, snapshot, RenderOptions{ShowCodes: true})
		},
	} {
		var out bytes.Buffer
		if err := render(&out); err != nil {
			t.Fatal(err)
		}
		for _, value := range want {
			if !strings.Contains(out.String(), value) {
				t.Errorf("missing actual presentation %q", value)
			}
		}
	}
	// A read/write/read path carries exactly these code identifiers to rendering.
	dataset, err := doc.DatasetWithMetadataSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	read, rebound, err := ReadDocumentWithMetadataSnapshot(dataset, ReadOptions{}, MetadataSnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	after, err := RenderItemsWithMetadataSnapshot(read.Document, rebound, RenderOptions{ShowCodes: true})
	if err != nil {
		t.Fatal(err)
	}
	if after.Title != rendered.Title || after.Items[0].Concept != rendered.Items[0].Concept || after.Items[1].Value != rendered.Items[1].Value {
		t.Fatal("round-trip presentation changed")
	}
}

func TestContextRenderLegacyNilAndMeaningOnlyPaths(t *testing.T) {
	doc, metadata := contextRenderFixture()
	snapshot := mustSnapshot(t, doc, metadata)
	for _, opts := range []RenderOptions{{}, {ShowCodes: true}, {MaxItems: 1}} {
		var legacyText, additiveText, legacyHTML, additiveHTML bytes.Buffer
		if err := RenderText(&legacyText, doc, opts); err != nil {
			t.Fatal(err)
		}
		if err := RenderTextWithMetadataSnapshot(&additiveText, doc, nil, opts); err != nil {
			t.Fatal(err)
		}
		if err := RenderHTML(&legacyHTML, doc, opts); err != nil {
			t.Fatal(err)
		}
		if err := RenderHTMLWithMetadataSnapshot(&additiveHTML, doc, nil, opts); err != nil {
			t.Fatal(err)
		}
		if legacyText.String() != additiveText.String() || legacyHTML.String() != additiveHTML.String() {
			t.Fatal("nil snapshot differs from legacy")
		}
	}
	rendered, err := RenderItemsWithMetadataSnapshot(doc, snapshot, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rendered.Title != "Title" || rendered.Items[0].Value != "Value" || rendered.Items[1].Value != "12.5 Units" {
		t.Fatal("ShowCodes false changed meaning-only presentation")
	}
	doc2 := renderFixture()
	snapshot2 := mustSnapshot(t, doc2, TemplateValidationMetadata{})
	normal, err := RenderItemsWithMetadataSnapshot(doc2, snapshot2, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range normal.Items {
		if item.ValueType == ValueNum && item.Value == "12.5 millimeter" {
			t.Fatal("UCUM compact form lost")
		}
	}
	// Meaning-free malformed input still uses the preserved identifier for display.
	doc3 := &Document{Title: CodedEntry{CodingSchemeDesignator: "99TEST"}}
	meta3 := TemplateValidationMetadata{CodeContexts: []ContentItemCodeContexts{{Path: "1", ConceptName: CodeContext{LongCodeValue: "long-code-without-meaning"}}}}
	s3 := mustSnapshot(t, doc3, meta3)
	r3, err := RenderItemsWithMetadataSnapshot(doc3, s3, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r3.Title != "long-code-without-meaning" {
		t.Fatalf("missing-meaning fallback: %q", r3.Title)
	}
}

func TestContextRenderEscapesCodesAndDoesNotActivateURLs(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	identifier := server.URL + "/<script>?a=1&b=\"x\"\x1b\u202e"
	code, context := NewCode(identifier, "", "URL <img>")
	doc := &Document{Title: code}
	metadata := TemplateValidationMetadata{CodeContexts: []ContentItemCodeContexts{{Path: "1", ConceptName: context}}}
	snapshot := mustSnapshot(t, doc, metadata)
	var text, page bytes.Buffer
	if err := RenderTextWithMetadataSnapshot(&text, doc, snapshot, RenderOptions{ShowCodes: true}); err != nil {
		t.Fatal(err)
	}
	if err := RenderHTMLWithMetadataSnapshot(&page, doc, snapshot, RenderOptions{ShowCodes: true}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 {
		t.Fatal("coded URL caused network I/O")
	}
	for _, output := range []string{text.String(), page.String()} {
		if strings.Contains(output, "\x1b") || strings.Contains(output, "\u202e") || !strings.Contains(output, `\x1b\x202e`) {
			t.Fatal("code controls were not escaped")
		}
	}
	if strings.Contains(page.String(), "<script>") || strings.Contains(page.String(), "<img>") || strings.Contains(page.String(), "href=") || strings.Contains(page.String(), "src=") {
		t.Fatal("URL became active markup")
	}
	if !strings.Contains(page.String(), html.EscapeString(textSafe(identifier))) {
		t.Fatal("escaped identifier omitted")
	}
	if !strings.Contains(text.String(), textSafe(identifier)) {
		t.Fatal("text identifier omitted")
	}
}

func TestContextRenderRejectsForeignDocumentAndInvalidAssociations(t *testing.T) {
	doc, metadata := contextRenderFixture()
	snapshot := mustSnapshot(t, doc, metadata)
	foreign := *doc
	_, err := RenderItemsWithMetadataSnapshot(&foreign, snapshot, RenderOptions{})
	requireSnapshotCode(t, err, MetadataSnapshotStale)
	metadata.CodeContexts[1].Path = "1.99"
	if _, err := BindTemplateMetadata(doc, metadata, MetadataSnapshotOptions{}); err == nil {
		t.Fatal("orphan context accepted")
	}
	doc, metadata = contextRenderFixture()
	metadata.CodeContexts[1].MeasurementUnits = CodeContext{URNCodeValue: "urn:example:wrong-role"}
	if _, err := BindTemplateMetadata(doc, metadata, MetadataSnapshotOptions{}); err == nil {
		t.Fatal("unit context on CODE accepted")
	}
}
