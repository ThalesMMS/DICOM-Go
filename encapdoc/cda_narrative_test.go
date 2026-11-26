package encapdoc

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
)

func cdaNarrativeFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "cda_narrative", "supported.xml"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func cdaNarrativeDocument(text string) []byte {
	return []byte(`<ClinicalDocument xmlns="urn:hl7-org:v3"><id root="1.2.3"/><title>Synthetic</title><component><structuredBody><component><section><title>Section</title><text>` + text + `</text></section></component></structuredBody></component></ClinicalDocument>`)
}

func TestCDANarrativeStructuredBody(t *testing.T) {
	data := cdaNarrativeFixture(t)
	before := bytes.Clone(data)
	model, err := ParseCDANarrative(context.Background(), data, CDANarrativeLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if model.Title != "Synthetic CDA report" || model.HL7InstanceIdentifier != "1.2.826.0.1.3680043.10.1470^SYNTHETIC" {
		t.Fatalf("header: %+v", model)
	}
	for _, text := range []string{"Findings", "A synthetic finding & observation.\n  Second line.", "1. First step", "2. Second step", "• Nested detail", "Continuation after nested list.", "Synthetic measurements", "Measure | Value", "Diameter | 3 cm", "Nested conclusion", "Independent nested section.", "Literal <b>markup</b> remains inert text."} {
		if !strings.Contains(model.PlainText, text) {
			t.Errorf("missing narrative %q:\n%s", text, model.PlainText)
		}
	}
	if strings.Contains(model.PlainText, "CLINICAL ENTRY") || !bytes.Equal(before, data) {
		t.Fatal("clinical entry leaked or source was changed")
	}
	wantKinds := []string{"section", "paragraph", "list", "caption", "list-item", "list-item", "list", "list-item", "paragraph", "table", "caption", "table-row", "table-row", "table-row", "section", "paragraph", "section", "paragraph"}
	var kinds []string
	for _, block := range model.Blocks {
		kinds = append(kinds, block.Kind)
		if block.Text == "Nested conclusion" && block.Depth != 1 {
			t.Errorf("nested section depth %d", block.Depth)
		}
		if block.Kind == CDANarrativeTableRow && len(block.Cells) != 2 {
			t.Errorf("row cells: %+v", block)
		}
	}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Fatalf("block order %v, want %v", kinds, wantKinds)
	}
	if !reflect.DeepEqual(model.Diagnostics, []CDANarrativeDiagnostic{{Code: "style-hints-ignored", Count: 1}}) {
		t.Fatalf("diagnostics: %+v", model.Diagnostics)
	}
	// All returned text is detached from the caller-owned byte buffer.
	for i := range data {
		data[i] = 'x'
	}
	if model.Title != "Synthetic CDA report" || !strings.Contains(model.PlainText, "3 cm") {
		t.Fatal("model retained mutable input")
	}
}

func TestCDANarrativeNamespaceAndLiteralText(t *testing.T) {
	data := string(cdaNarrativeFixture(t))
	data = strings.Replace(data, `xmlns="urn:hl7-org:v3"`, `xmlns:h="urn:hl7-org:v3"`, 1)
	data = regexp.MustCompile(`<(/?)([A-Za-z][A-Za-z0-9]*)`).ReplaceAllString(data, `<${1}h:${2}`)
	model, err := ParseCDANarrative(nil, []byte(data), CDANarrativeLimits{})
	if err != nil || model.Title != "Synthetic CDA report" || !strings.Contains(model.PlainText, "3 cm") {
		t.Fatalf("prefixed namespace: %+v %v", model, err)
	}
	model, err = ParseCDANarrative(nil, cdaNarrativeDocument(`<paragraph><![CDATA[<script>alert('literal text')</script>]]> &#x202e;safe<content> suffix</content></paragraph>`), CDANarrativeLimits{})
	if err != nil || !strings.Contains(model.PlainText, "<script>alert('literal text')</script>") || strings.Contains(model.PlainText, "\u202e") || !strings.Contains(model.PlainText, `\x202e`) {
		t.Fatalf("literal/safe text: %+v %v", model, err)
	}
}

func TestCDANarrativeRevisionsReferencesAndUnsupportedDiagnostics(t *testing.T) {
	data := cdaNarrativeDocument(`<paragraph>Value H<sub>2</sub>O and cm<sup>2</sup>; <content revised="delete">DELETED</content><content revised="insert">new</content>. <linkHtml href="#local">Local label</linkHtml><footnote>Footnote text</footnote><footnoteRef IDREF="unknown"/><unknown>HIDDEN</unknown><x:note xmlns:x="urn:synthetic">FOREIGN HIDDEN</x:note></paragraph>`)
	model, err := ParseCDANarrative(nil, data, CDANarrativeLimits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"H_(2)O", "cm^(2)", "[inserted: new]", "Local label", "[footnote: Footnote text]"} {
		if !strings.Contains(model.PlainText, text) {
			t.Errorf("missing %q in %q", text, model.PlainText)
		}
	}
	if strings.Contains(model.PlainText, "DELETED") || strings.Contains(model.PlainText, "HIDDEN") {
		t.Fatal("deleted/unsupported text became current narrative")
	}
	diagnostics := map[string]int{}
	for _, diagnostic := range model.Diagnostics {
		diagnostics[diagnostic.Code] = diagnostic.Count
	}
	if !reflect.DeepEqual(diagnostics, map[string]int{"deleted-content-suppressed": 1, "local-link-inert": 1, "footnote-reference-unresolved": 1, "unsupported-element-omitted": 2}) {
		t.Fatalf("diagnostics: %+v", model.Diagnostics)
	}
}

func TestCDANarrativeTableCellBoundaries(t *testing.T) {
	model, err := ParseCDANarrative(nil, cdaNarrativeDocument(`<table><tr><td>A | B</td><td>First<br/>Second</td><td>Plain cell</td></tr></table>`), CDANarrativeLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(model.PlainText, `"A | B" | "First\nSecond" | Plain cell`) {
		t.Fatalf("cell boundaries lost: %q", model.PlainText)
	}
	row := model.Blocks[len(model.Blocks)-1]
	if !reflect.DeepEqual(row.Cells, []string{"A | B", "First\nSecond", "Plain cell"}) {
		t.Fatalf("model cell text changed: %+v", row)
	}
}

func TestCDANarrativeRejectsMalformedAndAbsentNarrative(t *testing.T) {
	for name, data := range map[string][]byte{
		"wrong namespace":       []byte(`<ClinicalDocument xmlns="urn:wrong"><id root="1.2.3"/></ClinicalDocument>`),
		"malformed":             []byte(`<ClinicalDocument xmlns="urn:hl7-org:v3"><id root="1.2.3"/>`),
		"missing ID":            []byte(`<ClinicalDocument xmlns="urn:hl7-org:v3"/>`),
		"duplicate title":       []byte(`<ClinicalDocument xmlns="urn:hl7-org:v3"><id root="1.2.3"/><title>One</title><title>Two</title></ClinicalDocument>`),
		"unknown entity":        cdaNarrativeDocument(`<paragraph>&unknown;</paragraph>`),
		"misplaced declaration": cdaNarrativeDocument(`<paragraph>Before<?xml version="1.0"?>After</paragraph>`),
	} {
		t.Run(name, func(t *testing.T) {
			model, err := ParseCDANarrative(nil, data, CDANarrativeLimits{})
			if err == nil || model.PlainText != "" || len(model.Blocks) != 0 {
				t.Fatalf("malformed accepted: %+v %v", model, err)
			}
		})
	}
	for name, data := range map[string][]byte{
		"section title only": cdaNarrativeDocument(``),
		"empty list":         cdaNarrativeDocument(`<list><item/></list>`),
		"deleted only":       cdaNarrativeDocument(`<content revised="delete">Old text</content>`),
		"empty insertion":    cdaNarrativeDocument(`<content revised="insert"><content revised="delete">Old text</content></content>`),
		"entries only":       []byte(`<ClinicalDocument xmlns="urn:hl7-org:v3"><id root="1.2.3"/><component><structuredBody><component><section><title>Header</title><entry><observation><text>Not narrative</text></observation></entry></section></component></structuredBody></component></ClinicalDocument>`),
		"unsupported only":   cdaNarrativeDocument(`<unsupported>Hidden</unsupported>`),
	} {
		t.Run(name, func(t *testing.T) {
			model, err := ParseCDANarrative(nil, data, CDANarrativeLimits{})
			if !errors.Is(err, ErrCDANarrativeAbsent) || model.PlainText != "" {
				t.Fatalf("missing narrative: %+v %v", model, err)
			}
		})
	}
}

func TestCDANarrativeRejectsResourcesWithoutRequests(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer server.Close()
	for name, data := range map[string][]byte{
		"external link":     cdaNarrativeDocument(`<paragraph><linkHtml href="` + server.URL + `">Remote</linkHtml></paragraph>`),
		"file resource":     cdaNarrativeDocument(`<paragraph><img src="file:///private/secret"/></paragraph>`),
		"outer attributes":  cdaNarrativeDocument(`<paragraph href="` + server.URL + `">Text</paragraph>`),
		"section title":     bytes.Replace(cdaNarrativeDocument(`<paragraph>Text</paragraph>`), []byte(`<title>Section</title>`), []byte(`<title><linkHtml href="`+server.URL+`">Title</linkHtml></title>`), 1),
		"deleted resource":  cdaNarrativeDocument(`<content revised="delete"><linkHtml href="` + server.URL + `">Old resource</linkHtml></content>`),
		"omitted subtree":   cdaNarrativeDocument(`<unknown><x:a xmlns:x="urn:x" href="` + server.URL + `"/></unknown><paragraph>Other text</paragraph>`),
		"multimedia":        cdaNarrativeDocument(`<paragraph>Text<renderMultiMedia referencedObject="LOCAL"/></paragraph>`),
		"foreign active":    cdaNarrativeDocument(`<x:script xmlns:x="http://www.w3.org/1999/xhtml">BAD</x:script><paragraph>Text</paragraph>`),
		"DTD external":      append([]byte(`<!DOCTYPE ClinicalDocument SYSTEM "`+server.URL+`">`), cdaNarrativeDocument(`<paragraph>Text</paragraph>`)...),
		"DTD entity":        append([]byte(`<!DOCTYPE ClinicalDocument [<!ENTITY x "Expanded">]>`), cdaNarrativeDocument(`<paragraph>&x;</paragraph>`)...),
		"stylesheet":        append([]byte(`<?xml-stylesheet href="`+server.URL+`"?>`), cdaNarrativeDocument(`<paragraph>Text</paragraph>`)...),
		"unsupported spans": cdaNarrativeDocument(`<table><tr><td colspan="2">Span</td></tr></table>`),
		"nested cell list":  cdaNarrativeDocument(`<table><tr><td><list><item>Nested</item></list></td></tr></table>`),
		"nonXML body":       []byte(`<ClinicalDocument xmlns="urn:hl7-org:v3"><id root="1.2.3"/><component><nonXMLBody><text>Other body profile</text></nonXMLBody></component></ClinicalDocument>`),
	} {
		t.Run(name, func(t *testing.T) {
			model, err := ParseCDANarrative(nil, data, CDANarrativeLimits{})
			if err == nil || len(model.Blocks) != 0 || model.PlainText != "" {
				t.Fatalf("unsafe/unsupported accepted: %+v %v", model, err)
			}
			want := ErrCDANarrativeUnsupported
			if strings.HasPrefix(name, "DTD") {
				want = ErrInvalidDocument
			}
			if !errors.Is(err, want) {
				t.Fatalf("error %v, want %v", err, want)
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("external requests: %d", requests.Load())
	}
}

func TestCDANarrativeLimitsAndCancellation(t *testing.T) {
	data := cdaNarrativeFixture(t)
	model, err := ParseCDANarrative(nil, data, CDANarrativeLimits{})
	if err != nil {
		t.Fatal(err)
	}
	items := len(model.Blocks)
	for _, block := range model.Blocks {
		items += len(block.Cells)
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	tokens, depth, maxDepth := 0, 0, 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		tokens++
		switch token.(type) {
		case xml.StartElement:
			depth++
			if depth > maxDepth {
				maxDepth = depth
			}
		case xml.EndElement:
			depth--
		}
	}
	for name, limits := range map[string]CDANarrativeLimits{
		"bytes": {MaxBytes: int64(len(data) - 1)}, "depth": {MaxDepth: maxDepth - 1}, "tokens": {MaxTokens: tokens - 1},
		"items": {MaxItems: items - 1}, "text": {MaxTextBytes: 128},
	} {
		t.Run(name, func(t *testing.T) {
			model, err := ParseCDANarrative(nil, data, limits)
			if !errors.Is(err, ErrEncapsulatedDocumentTooLarge) || len(model.Blocks) != 0 {
				t.Fatalf("quota ignored: %+v %v", model, err)
			}
		})
	}
	if _, err := ParseCDANarrative(nil, data, CDANarrativeLimits{MaxBytes: int64(len(data)), MaxDepth: maxDepth, MaxTokens: tokens, MaxItems: items}); err != nil {
		t.Fatalf("exact admission: %v", err)
	}
	for _, limits := range []CDANarrativeLimits{{MaxBytes: -1}, {MaxDepth: 65}, {MaxTokens: -1}, {MaxItems: -1}, {MaxTextBytes: -1}} {
		if _, err := ParseCDANarrative(nil, data, limits); !errors.Is(err, ErrInvalidDocument) {
			t.Fatalf("invalid limits accepted: %+v %v", limits, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ParseCDANarrative(ctx, data, CDANarrativeLimits{}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored", err)
	}
	ctxDuring := &cdaCancelAfterChecks{Context: context.Background(), remaining: 100}
	if model, err := ParseCDANarrative(ctxDuring, cdaNarrativeDocument(strings.Repeat(`<content>Value</content>`, 1000)), CDANarrativeLimits{}); !errors.Is(err, context.Canceled) || len(model.Blocks) != 0 {
		t.Fatalf("mid-operation cancellation: %+v %v", model, err)
	}
}

type cdaCancelAfterChecks struct {
	context.Context
	remaining int
}

func (c *cdaCancelAfterChecks) Err() error {
	c.remaining--
	if c.remaining <= 0 {
		return context.Canceled
	}
	return nil
}

func TestCDANarrativeOriginalEnvelopeBytesRemainAvailable(t *testing.T) {
	data := cdaNarrativeFixture(t)
	if len(data)%2 == 0 {
		data = append(data, '\n')
	}
	file, err := NewCDA(CDAOptions{DocumentOptions: documentOptions("2.25.147074", data)})
	if err != nil {
		t.Fatal(err)
	}
	document, err := Extract(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseCDANarrative(nil, document.Data, CDANarrativeLimits{}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(document.Data, data) || document.HL7InstanceIdentifier != "1.2.826.0.1.3680043.10.1470^SYNTHETIC" {
		t.Fatal("original bytes/identity lost during narrative parsing")
	}
}

func TestCDANarrativeVisualFixtureExport(t *testing.T) {
	model, err := ParseCDANarrative(nil, cdaNarrativeFixture(t), CDANarrativeLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if directory := os.Getenv("DICOMGO_CDA_NARRATIVE_PREVIEW_DIR"); directory != "" {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		data, err := json.MarshalIndent(model, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		for name, value := range map[string][]byte{"supported.txt": []byte(model.PlainText), "supported.json": data, "supported.xml": cdaNarrativeFixture(t)} {
			if err := os.WriteFile(filepath.Join(directory, name), value, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}
