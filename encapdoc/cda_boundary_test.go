package encapdoc

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestCDANarrativeUsesOnlyTheDocumentID(t *testing.T) {
	for _, tc := range []struct {
		name, idElement, wantID string
		wantErr                 error
	}{
		{"oid", `<id root="2.25.541"/>`, "2.25.541", nil},
		{"oid-with-extension", `<id root="2.25.542" extension="Note-β"/>`, "2.25.542^Note-β", nil},
		{"uuid-with-extension", `<id root="59BAB3F2-36B3-49B8-89F2-CEDC184107EE" extension="v3"/>`, "59BAB3F2-36B3-49B8-89F2-CEDC184107EE^v3", nil},
		{"prefixed-id", `<h:id xmlns:h="urn:hl7-org:v3" root="2.25.543"/>`, "2.25.543", nil},
		{"nested-patient-id-precedes-document-id", `<recordTarget><patientRole><id root="2.25.999"/></patientRole></recordTarget><id root="2.25.544"/>`, "2.25.544", nil},
		{"only-nested-id", `<recordTarget><patientRole><id root="2.25.999"/></patientRole></recordTarget>`, "", ErrMissingHL7InstanceIdentifier},
		{"foreign-id-with-same-local-name", `<id xmlns="urn:synthetic:other" root="2.25.545"/>`, "", ErrMissingHL7InstanceIdentifier},
		{"foreign-root-attribute", `<id xmlns:x="urn:synthetic:other" x:root="2.25.546"/>`, "", ErrInvalidDocument},
		{"invalid-first-oid-arc", `<id root="3.25.547"/>`, "", ErrInvalidDocument},
		{"invalid-second-oid-arc", `<id root="1.40.548"/>`, "", ErrInvalidDocument},
		{"ambiguous-extension-delimiter", `<id root="2.25.549" extension="revision&#x5e;three"/>`, "", ErrInvalidDocument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := bytes.Replace(cdaNarrativeDocument(`<paragraph>A reviewed finding.</paragraph>`), []byte(`<id root="1.2.3"/>`), []byte(tc.idElement), 1)
			model, err := ParseCDANarrative(nil, data, CDANarrativeLimits{})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ParseCDANarrative error=%v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				if !reflect.DeepEqual(model, CDANarrative{}) {
					t.Fatalf("identity error returned partial narrative: %+v", model)
				}
				return
			}
			if model.HL7InstanceIdentifier != tc.wantID || !strings.Contains(model.PlainText, "A reviewed finding.") {
				t.Fatalf("identity or narrative changed: %+v", model)
			}
		})
	}
}

func TestCDANarrativeDistinguishesLiteralMarkupFromActiveDeclarations(t *testing.T) {
	const literal = `<!DOCTYPE specimen> <!ENTITY source "unknown">`
	for _, tc := range []struct {
		name, prolog, narrative, contains, excludes string
		wantErr                                     error
	}{
		{"comment-with-declaration-words", "<!--" + literal + "-->", `<paragraph>Ordinary &amp; known.</paragraph>`, "Ordinary & known.", literal, nil},
		{"cdata-with-declaration-words", "", `<paragraph><![CDATA[` + literal + `]]></paragraph>`, literal, "", nil},
		{"escaped-declaration-words", "", `<paragraph>&lt;!DOCTYPE specimen&gt;</paragraph>`, "<!DOCTYPE specimen>", "", nil},
		{"real-dtd-after-inert-comment", `<!--literal--><!DOCTYPE ClinicalDocument [<!ENTITY expanded "blocked">]>`, `<paragraph>Visible &expanded;</paragraph>`, "", "", ErrInvalidDocument},
		{"late-processing-instruction", "", `<paragraph>Private synthetic text.</paragraph><?note inert-looking?>`, "", "", ErrCDANarrativeUnsupported},
		{"unclosed-document-after-visible-narrative", "", `<paragraph>A partial note.</paragraph><paragraph>Late syntax error`, "", "", ErrInvalidDocument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := append([]byte(tc.prolog), cdaNarrativeDocument(tc.narrative)...)
			model, err := ParseCDANarrative(nil, data, CDANarrativeLimits{})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ParseCDANarrative error=%v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				if !reflect.DeepEqual(model, CDANarrative{}) {
					t.Fatalf("rejected XML returned partial narrative: %+v", model)
				}
				return
			}
			if !strings.Contains(model.PlainText, tc.contains) || tc.excludes != "" && strings.Contains(model.PlainText, tc.excludes) {
				t.Fatalf("rendered literal narrative = %q, want %q without %q", model.PlainText, tc.contains, tc.excludes)
			}
		})
	}
}

func TestCDANarrativeLocalReferencesStayInert(t *testing.T) {
	for _, tc := range []struct {
		name, narrative, wantText string
		links                     int
	}{
		{"existing-target", `<paragraph><content ID="source">Finding.</content> <linkHtml href="#source">See finding.</linkHtml></paragraph>`, "Finding. See finding.", 1},
		{"unresolved-target", `<paragraph><linkHtml href="#missing">Visible label.</linkHtml></paragraph>`, "Visible label.", 1},
		{"cycle", `<paragraph><content ID="left"><linkHtml href="#right">A.</linkHtml></content> <content ID="right"><linkHtml href="#left">B.</linkHtml></content></paragraph>`, "A. B.", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model, err := ParseCDANarrative(nil, cdaNarrativeDocument(tc.narrative), CDANarrativeLimits{})
			if err != nil {
				t.Fatal(err)
			}
			blocks := model.Blocks
			if len(blocks) != 2 || blocks[1].Kind != CDANarrativeParagraph || blocks[1].Text != tc.wantText {
				t.Fatalf("blocks = %+v, want one paragraph with %q", blocks, tc.wantText)
			}
			want := []CDANarrativeDiagnostic{{Code: "local-link-inert", Count: tc.links}}
			if !reflect.DeepEqual(model.Diagnostics, want) || strings.Contains(model.PlainText, "#") {
				t.Fatalf("local references returned unexpected text or diagnostics: %+v", model)
			}
		})
	}
}

func TestCDANarrativeChargesCombinedTextAcrossCDATAAndMarkup(t *testing.T) {
	data := cdaNarrativeDocument(`<paragraph>alpha<![CDATA[β]]><content>gamma</content></paragraph>`)
	const paragraph = "alphaβgamma"
	// The budget counts UTF-8 bytes from the ID, title, section, and paragraph.
	limit := int64(len("1.2.3") + len("Synthetic") + len("Section") + len(paragraph))
	model, err := ParseCDANarrative(nil, data, CDANarrativeLimits{MaxTextBytes: limit})
	if err != nil || len(model.Blocks) != 2 || model.Blocks[1].Text != paragraph {
		t.Fatalf("exact text budget rejected or text changed: %+v, %v", model, err)
	}
	model, err = ParseCDANarrative(nil, data, CDANarrativeLimits{MaxTextBytes: limit - 1})
	if !errors.Is(err, ErrEncapsulatedDocumentTooLarge) || !reflect.DeepEqual(model, CDANarrative{}) {
		t.Fatalf("overflowing text budget returned %+v with error %v", model, err)
	}
}
