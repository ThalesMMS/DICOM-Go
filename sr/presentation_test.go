package sr

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func presentationFixture() *Document {
	concept := func(meaning string) CodedEntry {
		return CodedEntry{CodeValue: "SYNTHETIC", CodingSchemeDesignator: "99TEST", CodeMeaning: meaning}
	}
	return &Document{SOPClassUID: ComprehensiveSRStorage, SOPInstanceUID: "1.2.3.1534",
		Title: concept("Synthetic report"), ContinuityOfContent: "SEPARATE", CompletionFlag: "COMPLETE", VerificationFlag: "UNVERIFIED",
		Content: []ContentItem{
			{ValueType: ValueContainer, RelationshipType: RelationshipContains, ConceptName: concept("Findings"), ContinuityOfContent: "CONTINUOUS", Children: []ContentItem{
				{ValueType: ValueText, RelationshipType: RelationshipContains, Text: "A mass of"},
				{ValueType: ValueNum, RelationshipType: RelationshipContains, ConceptName: concept("Diameter"), Measurement: &Measurement{Value: 3, Units: CodedEntry{CodeValue: "cm", CodingSchemeDesignator: "UCUM", CodeMeaning: "centimeter"}}, Children: []ContentItem{
					{RelationshipType: RelationshipInferredFrom, ReferencedContentItemIdentifier: ContentItemIdentifier{1, 2, 1}},
				}},
				{ValueType: ValueText, RelationshipType: RelationshipContains, Text: "was detected."},
				{ValueType: ValuePName, RelationshipType: RelationshipHasObsContext, ConceptName: concept("Observer"), PersonName: "Synthetic^Reader"},
				{ValueType: ValueContainer, RelationshipType: RelationshipContains, ConceptName: concept("Conclusions"), ContinuityOfContent: "SEPARATE", Children: []ContentItem{
					{ValueType: ValueText, RelationshipType: RelationshipContains, Text: "Synthetic follow-up recommended."},
					{ValueType: ValueContainer, RelationshipType: RelationshipContains, ConceptName: concept("Note"), ContinuityOfContent: "CONTINUOUS", Children: []ContentItem{
						{ValueType: ValueText, RelationshipType: RelationshipContains, Text: "Nested narrative"},
						{RelationshipType: RelationshipContains, ReferencedContentItemIdentifier: ContentItemIdentifier{1, 1, 2}},
					}},
				}},
			}},
			{ValueType: ValueContainer, RelationshipType: RelationshipContains, ConceptName: concept("Evidence"), ContinuityOfContent: "SEPARATE", Children: []ContentItem{
				{ValueType: ValueImage, RelationshipType: RelationshipContains, Image: ImageReference{SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: "1.2.3.4"}},
			}},
		}}
}

func TestPresentationContinuityHierarchyAndOrder(t *testing.T) {
	doc := presentationFixture()
	presentation, err := RenderPresentation(doc, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		path, kind string
		depth      int
	}{
		{"1.1", PresentationNarrative, 0}, {"1.1.1", PresentationInline, 1},
		{"1.1.2", PresentationInline, 1}, {"1.1.2.1", PresentationReference, 2},
		{"1.1.3", PresentationInline, 1}, {"1.1.4", PresentationItem, 1},
		{"1.1.5", PresentationSection, 1}, {"1.1.5.1", PresentationItem, 2},
		{"1.1.5.2", PresentationNarrative, 2}, {"1.1.5.2.1", PresentationInline, 3},
		{"1.1.5.2.2", PresentationReference, 3}, {"1.2", PresentationSection, 0}, {"1.2.1", PresentationItem, 1},
	}
	if len(presentation.Blocks) != len(want) || presentation.Continuity != "SEPARATE" {
		t.Fatalf("presentation: %+v", presentation)
	}
	for i, expected := range want {
		block := presentation.Blocks[i]
		if block.Path != expected.path || block.Kind != expected.kind || block.Depth != expected.depth {
			t.Errorf("block %d = %+v, want %+v", i, block, expected)
		}
	}
	if presentation.Blocks[3].ReferenceTo != "1.2.1" || presentation.Blocks[5].Relationship != "has obs context" {
		t.Fatal("reference/context relationships lost")
	}
	for i, block := range presentation.Blocks {
		wantFlow := i == 1 || i == 2 || i == 4 || i == 9 || i == 10
		if block.InNarrative != wantFlow {
			t.Errorf("narrative membership %s = %v, want %v", block.Path, block.InNarrative, wantFlow)
		}
	}
	// Root continuity does not leak into nested SEPARATE containers, nor from
	// a NUM into its modifiers, even when that NUM is part of a narrative.
	doc.ContinuityOfContent = "CONTINUOUS"
	doc.Content = append(doc.Content, ContentItem{ValueType: ValueText, RelationshipType: RelationshipContains, Text: "Root narrative"})
	presentation, err = RenderPresentation(doc, RenderOptions{})
	if err != nil || presentation.Blocks[len(presentation.Blocks)-1].Kind != PresentationInline || presentation.Blocks[12].Kind != PresentationItem {
		t.Fatalf("immediate continuity: %+v, %v", presentation, err)
	}
}

func TestPresentationLifecycleHeadersAndAbsentFacts(t *testing.T) {
	doc := presentationFixture()
	doc.CompletionFlagDescription, doc.VerificationFlag = "Reviewed <synthetic>", "VERIFIED"
	doc.VerifyingObservers = []VerifyingObserver{{Name: "Reader^Synthetic", Organization: "Synthetic Hospital", DateTime: "20261010123000",
		Code: CodedEntry{CodeValue: "PERSON", CodingSchemeDesignator: "99TEST", CodeMeaning: "Synthetic verifier"}}}
	doc.PredecessorDocuments = []DocumentReference{{StudyInstanceUID: "1.2.3.1", SeriesInstanceUID: "1.2.3.2", SOPClassUID: EnhancedSRStorage, SOPInstanceUID: "1.2.3.3"}}
	presentation, err := RenderPresentation(doc, RenderOptions{ShowCodes: true})
	if err != nil {
		t.Fatal(err)
	}
	header := map[string]string{}
	for _, field := range presentation.Header {
		header[field.Label] = field.Value
	}
	for label, value := range map[string]string{
		"Completion": "COMPLETE", "Document verification": "VERIFIED", "Completion description": "Reviewed <synthetic>",
		"Verifying observer 1": "Reader Synthetic", "Verifying observer 1 organization": "Synthetic Hospital",
		"Verifying observer 1 date/time": "2026-10-10 12:30:00", "Verifying observer 1 identification": "Synthetic verifier (PERSON, 99TEST)",
		"Predecessor document 1 study UID": "1.2.3.1", "Predecessor document 1 series UID": "1.2.3.2",
	} {
		if header[label] != value {
			t.Errorf("header %q = %q, want %q", label, header[label], value)
		}
	}
	if !strings.Contains(header["Predecessor document 1"], "1.2.3.3") {
		t.Fatal("predecessor instance omitted")
	}
	legacy, err := RenderItems(doc, RenderOptions{ShowCodes: true})
	for i := range legacy.Header {
		if legacy.Header[i].Label == "Verification" {
			legacy.Header[i].Label = "Document verification"
		}
	}
	if err != nil || !reflect.DeepEqual(legacy.Header, presentation.Header) {
		t.Fatalf("shared lifecycle header: %v", err)
	}
	missing, err := RenderPresentation(&Document{CompletionFlag: "PARTIAL", VerificationFlag: "UNVERIFIED"}, RenderOptions{})
	if err != nil || len(missing.Header) != 2 || missing.Title != "Structured Report" {
		t.Fatalf("missing facts: %+v, %v", missing, err)
	}
	if _, err := RenderPresentation(nil, RenderOptions{}); !errors.Is(err, ErrNilDocument) {
		t.Fatal(err)
	}
}

func TestPresentationReferenceSuppressesMalformedPayloadAndChildren(t *testing.T) {
	doc := &Document{Content: []ContentItem{
		{ValueType: ValueText, RelationshipType: RelationshipContains, Text: "Target"},
		{RelationshipType: RelationshipInferredFrom, ReferencedContentItemIdentifier: ContentItemIdentifier{1, 1}, ValueType: ValueContainer,
			Text: "INVALID PAYLOAD", ConceptName: CodedEntry{CodeMeaning: "INVALID CONCEPT"}, ContinuityOfContent: "CONTINUOUS",
			Children: []ContentItem{{ValueType: ValueText, Text: "INVALID CHILD"}}},
	}}
	presentation, err := RenderPresentation(doc, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ref := presentation.Blocks[1]
	if len(presentation.Blocks) != 2 || ref.ReferenceTo != "1.1" || ref.Kind != PresentationReference || ref.ValueType != "" || ref.Concept != "" || ref.Value != "" || ref.Continuity != "" {
		t.Fatalf("reference: %+v", presentation)
	}
	legacy, err := RenderItems(doc, RenderOptions{})
	if err != nil || len(legacy.Items) != 2 || legacy.Items[1].ValueType != "" {
		t.Fatalf("technical reference: %+v, %v", legacy, err)
	}
	for _, render := range []func(*bytes.Buffer) error{
		func(b *bytes.Buffer) error { return RenderPresentationHTML(b, doc, RenderOptions{}) },
		func(b *bytes.Buffer) error { return RenderPresentationText(b, doc, RenderOptions{}) },
		func(b *bytes.Buffer) error { return RenderHTML(b, doc, RenderOptions{}) },
		func(b *bytes.Buffer) error { return RenderText(b, doc, RenderOptions{}) },
	} {
		var out bytes.Buffer
		if err := render(&out); err != nil || strings.Contains(out.String(), "INVALID") {
			t.Fatalf("invalid reference expansion: %q, %v", out.String(), err)
		}
	}
	limited, err := RenderPresentation(doc, RenderOptions{MaxItems: 1})
	limitedLegacy, legacyErr := RenderItems(doc, RenderOptions{MaxItems: 1})
	if err != nil || legacyErr != nil || limited.Truncated != 1 || limitedLegacy.Truncated != 1 {
		t.Fatalf("suppressed descendants counted: %+v %+v %v %v", limited, limitedLegacy, err, legacyErr)
	}
}

func TestPresentationBoundsAndNoPartialOutput(t *testing.T) {
	presentation, err := RenderPresentation(renderFixture(), RenderOptions{MaxItems: 3, MaxPoints: 1})
	if err != nil || len(presentation.Blocks) != 3 || presentation.Truncated != 14 {
		t.Fatalf("item bound: %+v, %v", presentation, err)
	}
	all, err := RenderPresentation(renderFixture(), RenderOptions{MaxPoints: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, block := range all.Blocks {
		if block.ValueType == ValueSCoord && block.Value != "POLYLINE (1,2) ... (2 points)" {
			t.Fatalf("point bound: %+v", block)
		}
	}
	doc := &Document{VerifyingObservers: []VerifyingObserver{{Name: "One"}, {Name: "Two"}}, PredecessorDocuments: []DocumentReference{{SOPInstanceUID: "1.1"}, {SOPInstanceUID: "1.2"}}}
	header, err := RenderPresentation(doc, RenderOptions{MaxItems: 1})
	if err != nil {
		t.Fatal(err)
	}
	wantHeader := []RenderField{{"Verifying observer 1", "One"}, {"Verifying observers omitted", "1"}, {"Predecessor document 1", "1.1"}, {"Predecessor documents omitted", "1"}}
	if !reflect.DeepEqual(header.Header, wantHeader) {
		t.Fatalf("header limits: %+v", header.Header)
	}
	// Even a cyclic authored tree terminates; a small output limit does not
	// conceal excessive input depth or publish a prefix before the error.
	cycle := []ContentItem{{ValueType: ValueContainer}}
	cycle[0].Children = cycle
	doc.Content = cycle
	for _, render := range []func(*bytes.Buffer) error{
		func(b *bytes.Buffer) error { return RenderPresentationText(b, doc, RenderOptions{MaxItems: 1}) },
		func(b *bytes.Buffer) error { return RenderPresentationHTML(b, doc, RenderOptions{MaxItems: 1}) },
	} {
		out := bytes.NewBufferString("sentinel")
		if err := render(out); !errors.Is(err, ErrResourceLimitExceeded) || out.String() != "sentinel" {
			t.Fatalf("depth refusal: %q, %v", out.String(), err)
		}
	}
}

func TestPresentationReusesGuardedCodeContexts(t *testing.T) {
	doc, metadata := contextRenderFixture()
	snapshot := mustSnapshot(t, doc, metadata)
	opts := RenderOptions{ShowCodes: true}
	presentation, err := RenderPresentationWithMetadataSnapshot(doc, snapshot, opts)
	if err != nil {
		t.Fatal(err)
	}
	technical, err := RenderItemsWithMetadataSnapshot(doc, snapshot, opts)
	if err != nil || presentation.Title != technical.Title {
		t.Fatalf("title context: %v", err)
	}
	for i, block := range presentation.Blocks {
		if block.Concept != technical.Items[i].Concept || block.Value != technical.Items[i].Value {
			t.Fatalf("code role %d: %+v, %+v", i, block, technical.Items[i])
		}
	}
	legacy, err := RenderPresentation(doc, opts)
	nilSnapshot, nilErr := RenderPresentationWithMetadataSnapshot(doc, nil, opts)
	if err != nil || nilErr != nil || !reflect.DeepEqual(legacy, nilSnapshot) {
		t.Fatalf("nil context: %v, %v", err, nilErr)
	}
	doc.Content[3].Code.CodeMeaning = "Changed hidden item"
	for _, render := range []func(*bytes.Buffer) error{
		func(b *bytes.Buffer) error {
			return RenderPresentationTextWithMetadataSnapshot(b, doc, snapshot, RenderOptions{MaxItems: 1})
		},
		func(b *bytes.Buffer) error {
			return RenderPresentationHTMLWithMetadataSnapshot(b, doc, snapshot, RenderOptions{MaxItems: 1})
		},
	} {
		out := bytes.NewBufferString("sentinel")
		if err := render(out); !errors.Is(err, ErrMetadataSnapshot) || out.String() != "sentinel" {
			t.Fatalf("stale context: %q, %v", out.String(), err)
		}
	}
}

func TestPresentationSemanticOutputsAndEscaping(t *testing.T) {
	doc := &Document{Title: CodedEntry{CodeMeaning: "<script>Title</script>"}, ContinuityOfContent: "CONTINUOUS",
		CompletionFlagDescription: "<img src=x>\x1b\u202e", VerifyingObservers: []VerifyingObserver{{Name: "<svg>", Organization: "<script>Verifier</script>"}},
		PredecessorDocuments: []DocumentReference{{SOPInstanceUID: "<script>Predecessor</script>"}}, Content: []ContentItem{
			{ValueType: ValueText, RelationshipType: RelationshipContains, Text: "A mass of"},
			{ValueType: ValueNum, RelationshipType: RelationshipContains, ConceptName: CodedEntry{CodeMeaning: "Diameter"}, Measurement: &Measurement{Value: 3, Units: CodedEntry{CodeValue: "cm", CodingSchemeDesignator: "UCUM"}}},
			{ValueType: ValueText, RelationshipType: RelationshipContains, Text: "was detected. <b>\x1b\u202e"},
			{ValueType: ValueText, RelationshipType: RelationshipHasObsContext, Text: "Context"},
			{RelationshipType: RelationshipContains, ReferencedContentItemIdentifier: ContentItemIdentifier{1}},
		}}
	var page, text bytes.Buffer
	if err := RenderPresentationHTML(&page, doc, RenderOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := RenderPresentationText(&text, doc, RenderOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{page.String(), text.String()} {
		if strings.Contains(out, "\x1b") || strings.Contains(out, "\u202e") || !strings.Contains(out, `\x1b\x202e`) {
			t.Fatalf("controls reached output: %q", out)
		}
	}
	for _, forbidden := range []string{"<script>", "<img", "<svg", "<b>", "src=\"", "href=\"http"} {
		if strings.Contains(page.String(), forbidden) {
			t.Fatalf("active markup %q", forbidden)
		}
	}
	if !strings.Contains(text.String(), "A mass of Diameter: 3 cm was detected.") || !strings.Contains(text.String(), "[has obs context] Context") {
		t.Fatalf("text flow: %q", text.String())
	}
	if strings.Count(page.String(), `<div class="narrative flow" role="paragraph">`) != 2 || !strings.Contains(page.String(), `<a href="#item-1">Reference to 1</a>`) || !strings.Contains(page.String(), `id="item-1"`) {
		t.Fatalf("semantic paragraphs/root reference: %s", page.String())
	}
}

func TestPresentationHTMLHierarchyAndContainerRelationships(t *testing.T) {
	doc := presentationFixture()
	doc.Content[1].RelationshipType = RelationshipHasObsContext
	var page, text bytes.Buffer
	if err := RenderPresentationHTML(&page, doc, RenderOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := RenderPresentationText(&text, doc, RenderOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "[has obs context] Evidence [SEPARATE]") {
		t.Fatalf("container relation lost: %s", text.String())
	}
	// The HTML5 void meta element is normalized for the strict XML tokenizer;
	// all renderer-owned tags must balance without browser DOM repair. A NUM's
	// reference is structurally nested under that NUM, even within narrative.
	source := strings.Replace(page.String(), `<meta charset="utf-8">`, `<meta charset="utf-8"/>`, 1)
	decoder := xml.NewDecoder(strings.NewReader(source))
	var ids []string
	seenReference := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("unbalanced HTML: %v", err)
		}
		switch element := token.(type) {
		case xml.StartElement:
			id := ""
			for _, attribute := range element.Attr {
				if attribute.Name.Local == "id" {
					id = attribute.Value
				}
			}
			if id == "item-1.1.2.1" {
				seenReference = true
				if !containsPresentationAncestor(ids, "item-1.1.2") || !containsPresentationAncestor(ids, "item-1.1") {
					t.Fatalf("reference ancestors: %v", ids)
				}
			}
			ids = append(ids, id)
		case xml.EndElement:
			ids = ids[:len(ids)-1]
		}
	}
	if !seenReference {
		t.Fatal("reference missing from HTML")
	}
}

func containsPresentationAncestor(ids []string, target string) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

// DICOMGO_SR_PRESENTATION_PREVIEW_DIR exports independent synthetic scenarios
// for visual inspection. No fixture contains patient data or external resources.
func TestPresentationVisualFixtures(t *testing.T) {
	revision := presentationFixture()
	revision.CompletionFlagDescription, revision.VerificationFlag = "Revision of synthetic report", "VERIFIED"
	revision.VerifyingObservers = []VerifyingObserver{{Name: "Reader^Synthetic", Organization: "Synthetic Hospital", DateTime: "20261010123000"}}
	revision.PredecessorDocuments = []DocumentReference{{StudyInstanceUID: "1.2.3.1", SeriesInstanceUID: "1.2.3.2", SOPClassUID: ComprehensiveSRStorage, SOPInstanceUID: "1.2.3.1533"}}
	partial := &Document{Title: CodedEntry{CodeMeaning: "Synthetic partial report"}, ContinuityOfContent: "CONTINUOUS", CompletionFlag: "PARTIAL", VerificationFlag: "UNVERIFIED",
		CompletionFlagDescription: "Only initial findings available", Content: []ContentItem{{ValueType: ValueText, RelationshipType: RelationshipContains, Text: "Initial synthetic findings."}}}
	for name, doc := range map[string]*Document{"narrative-sections": presentationFixture(), "revision": revision, "partial": partial} {
		t.Run(name, func(t *testing.T) {
			for ext, render := range map[string]func(*bytes.Buffer) error{
				"html": func(b *bytes.Buffer) error { return RenderPresentationHTML(b, doc, RenderOptions{}) },
				"txt":  func(b *bytes.Buffer) error { return RenderPresentationText(b, doc, RenderOptions{}) },
			} {
				var out bytes.Buffer
				if err := render(&out); err != nil || out.Len() == 0 {
					t.Fatalf("fixture %s: %v", ext, err)
				}
				if directory := os.Getenv("DICOMGO_SR_PRESENTATION_PREVIEW_DIR"); directory != "" {
					if err := os.MkdirAll(directory, 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(directory, name+"."+ext), out.Bytes(), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}
