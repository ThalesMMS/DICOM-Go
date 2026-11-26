package encapdoc

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const (
	cdaValidationTargets   = `<recordTarget><patientRole><id root="2.25.31"/></patientRole></recordTarget>`
	cdaValidationCustodian = `<custodian><assignedCustodian><representedCustodianOrganization><id root="2.25.32"/></representedCustodianOrganization></assignedCustodian></custodian>`
	cdaValidationTemplate  = `<templateId root="2.16.840.1.113883.10.20.22.1.2" extension="2024-05-01"/>`
	cdaValidationRevision  = `<setId root="2.25.34"/><versionNumber value="1"/>`
	cdaValidationCode      = `<code code="34133-9" codeSystem="2.16.840.1.113883.6.1"/>`
)

func cdaValidationDocument(sections string) string {
	return `<ClinicalDocument xmlns="urn:hl7-org:v3"><typeId root="2.16.840.1.113883.1.3" extension="POCD_HD000040"/>` +
		cdaValidationTemplate + `<id root="2.25.30"/>` + cdaValidationCode +
		`<title>Synthetic</title><effectiveTime value="20261115"/><confidentialityCode code="N" codeSystem="2.16.840.1.113883.5.25"/>` +
		cdaValidationRevision + cdaValidationTargets +
		`<author><time value="20261115"/><assignedAuthor><id root="2.25.33"/></assignedAuthor></author>` +
		cdaValidationCustodian + `<component><structuredBody>` + sections + `</structuredBody></component></ClinicalDocument>`
}

func cdaValidationSection(text, entries string) string {
	return `<component><section><text>` + text + `</text>` + entries + `</section></component>`
}

func TestCDAValidationHeaderProfiles(t *testing.T) {
	const wrongCode = `<code code="private-code" codeSystem="2.16.840.1.113883.6.1"/>`
	valid := cdaValidationDocument(cdaValidationSection(`<paragraph>Reviewed.</paragraph>`, ""))
	for _, tc := range []struct {
		name, from, to string
		profile        CDAValidationProfile
		want           []CDAValidationFinding
	}{
		{name: "base"},
		{name: "ccd", profile: CDAProfileCCDHeader},
		{name: "multiple-targets", from: cdaValidationTargets, to: strings.Repeat(cdaValidationTargets, 2)},
		{name: "optional-revisions-omitted-together", from: cdaValidationRevision, to: ""},
		{name: "foreign-custodian", from: cdaValidationCustodian, to: `<custodian xmlns="urn:other"/>`, want: []CDAValidationFinding{{"header-cardinality", cdaHeaderPath + "/custodian", 1}}},
		{name: "duplicate-custodian", from: cdaValidationCustodian, to: strings.Repeat(cdaValidationCustodian, 2), want: []CDAValidationFinding{{"header-cardinality", cdaHeaderPath + "/custodian", 1}}},
		{name: "wrong-type-id", from: "POCD_HD000040", to: "private-type", want: []CDAValidationFinding{{"header-fixed-value", cdaHeaderPath + "/typeId", 1}}},
		{name: "version-without-set", from: `<setId root="2.25.34"/>`, to: "", want: []CDAValidationFinding{{"revision-pair", cdaHeaderPath, 1}}},
		{name: "set-without-version", from: `<versionNumber value="1"/>`, to: "", want: []CDAValidationFinding{{"revision-pair", cdaHeaderPath, 1}}},
		{name: "two-body-choices", from: `</structuredBody>`, to: `</structuredBody><nonXMLBody/>`, want: []CDAValidationFinding{{"body-choice", cdaHeaderPath + "/component", 1}}},
		{name: "wrong-code-base-can-pass", from: cdaValidationCode, to: wrongCode},
		{name: "wrong-code-ccd-fails", from: cdaValidationCode, to: wrongCode, profile: CDAProfileCCDHeader, want: []CDAValidationFinding{{"ccd-document-code", cdaHeaderPath + "/code", 1}}},
		{name: "wrong-code-system", from: cdaValidationCode, to: `<code code="34133-9" codeSystem="1.2.3"/>`, profile: CDAProfileCCDHeader, want: []CDAValidationFinding{{"ccd-document-code", cdaHeaderPath + "/code", 1}}},
		{name: "ccd-document-template-required", from: cdaValidationTemplate, to: "", profile: CDAProfileCCDHeader, want: []CDAValidationFinding{{"ccd-template", cdaHeaderPath + "/templateId", 1}}},
		{name: "local-template-is-outside-base-check", from: cdaValidationTemplate, to: `<templateId root="1.2.3"/>`},
		{name: "legacy-ccd-root", from: cdaValidationTemplate, to: `<templateId root="2.16.840.1.113883.10.20.22.1.2"/>`, profile: CDAProfileCCDHeader},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := []byte(strings.Replace(valid, tc.from, tc.to, 1))
			report, err := ValidateCDA(nil, input, CDAValidationOptions{Profile: tc.profile})
			wantProfile := tc.profile
			if wantProfile == "" {
				wantProfile = CDAProfileBaseHeader
			}
			if err != nil || report.Profile != wantProfile || report.HasErrors() != (len(tc.want) > 0) || !reflect.DeepEqual(report.Findings, tc.want) {
				t.Fatalf("report = %+v, err = %v, want profile %s and findings %+v", report, err, wantProfile, tc.want)
			}
			if strings.Contains(fmt.Sprint(report), "private-") {
				t.Fatalf("finding contains source data: %+v", report)
			}
			if _, err := ParseCDANarrative(nil, input, CDANarrativeLimits{}); err == nil && tc.name == "two-body-choices" {
				t.Fatal("reader accepted an unsupported nonXML body")
			} else if err != nil && tc.name != "two-body-choices" {
				t.Fatalf("opt-in finding changed compatibility reader: %v", err)
			}
		})
	}
}

func TestCDAValidationGraphChecksLiteralLocalReferences(t *testing.T) {
	entryLink := func(target string) string {
		return `<entry><observation><code><originalText><reference value="` + target + `"/></originalText></code></observation></entry>`
	}
	for _, tc := range []struct {
		name, sections string
		want           []CDAValidationFinding
	}{
		{
			name: "document-local-links-and-footnotes-cross-sections",
			sections: `<component><section ID="second"><text><footnote ID="foot">A note.</footnote></text></section></component>` +
				cdaValidationSection(`<paragraph ID="original">Finding.</paragraph><paragraph><linkHtml href="#second">See section.</linkHtml> <footnoteRef IDREF="foot"/></paragraph>`, entryLink("#original")),
		},
		{
			name:     "external-links-remain-outside-graph",
			sections: cdaValidationSection(`<paragraph><linkHtml href="https://example.invalid/#private-ref">Visible.</linkHtml></paragraph>`, ""),
		},
		{
			name:     "missing-and-foreign-anchors",
			sections: cdaValidationSection(`<paragraph><content xmlns="urn:other" ID="foreign"/> <linkHtml href="#foreign">A.</linkHtml> <linkHtml href="#private-ref">B.</linkHtml><footnoteRef IDREF="missing"/></paragraph>`, ""),
			want:     []CDAValidationFinding{{"dangling-narrative-reference", cdaLinkPath, 2}, {"dangling-narrative-reference", cdaFootnotePath, 1}},
		},
		{
			name: "ambiguous-across-sections",
			sections: cdaValidationSection(`<paragraph ID="private-duplicate"><linkHtml href="#private-duplicate">A.</linkHtml></paragraph>`, "") +
				cdaValidationSection(`<paragraph ID="private-duplicate">B.</paragraph>`, ""),
			want: []CDAValidationFinding{{"duplicate-narrative-id", cdaBodyPath + "//@ID", 1}, {"ambiguous-narrative-reference", cdaLinkPath, 1}},
		},
		{
			name: "wrong-footnote-kind-and-cross-section-entry",
			sections: cdaValidationSection(`<paragraph ID="other">B.</paragraph>`, "") +
				cdaValidationSection(`<paragraph><footnoteRef IDREF="other"/>A.</paragraph>`, entryLink("#other")),
			want: []CDAValidationFinding{{"wrong-narrative-target", cdaFootnotePath, 1}, {"wrong-narrative-target", cdaEntryPath, 1}},
		},
		{
			name:     "self-reference",
			sections: cdaValidationSection(`<paragraph ID="a"><linkHtml href="#a">A.</linkHtml></paragraph>`, ""),
			want:     []CDAValidationFinding{{"cyclic-narrative-reference", cdaBodyPath, 1}},
		},
		{
			name: "descendant-links-create-cycle-across-sections",
			sections: cdaValidationSection(`<paragraph><content ID="a"><content><linkHtml href="#b">A.</linkHtml></content></content></paragraph>`, "") +
				cdaValidationSection(`<paragraph><content ID="b"><linkHtml href="#a">B.</linkHtml></content></paragraph>`, ""),
			want: []CDAValidationFinding{{"cyclic-narrative-reference", cdaBodyPath, 1}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := []byte(cdaValidationDocument(tc.sections))
			report, err := ValidateCDA(nil, input, CDAValidationOptions{})
			if err != nil || !reflect.DeepEqual(report.Findings, tc.want) {
				t.Fatalf("report = %+v, err = %v, want %+v", report, err, tc.want)
			}
			if strings.Contains(fmt.Sprint(report), "private-") {
				t.Fatalf("finding contains source data: %+v", report)
			}
		})
	}
}

func TestCDAValidationLongAcyclicGraphAndLimits(t *testing.T) {
	var narrative strings.Builder
	const nodes = 6000
	for i := 0; i < nodes; i++ {
		if i != nodes-1 {
			fmt.Fprintf(&narrative, `<content ID="n%d"><linkHtml href="#n%d">Link.</linkHtml></content>`, i, i+1)
		} else {
			fmt.Fprintf(&narrative, `<content ID="n%d">End.</content>`, i)
		}
	}
	input := []byte(cdaValidationDocument(cdaValidationSection(narrative.String(), "")))
	report, err := ValidateCDA(nil, input, CDAValidationOptions{Limits: PayloadLimits{MaxBytes: int64(len(input))}})
	if err != nil || report.HasErrors() {
		t.Fatalf("exact byte limit / acyclic graph failed: %+v, %v", report, err)
	}
	for _, limits := range []PayloadLimits{{MaxBytes: int64(len(input) - 1)}, {MaxDepth: 4}, {MaxRecords: 10}} {
		report, err := ValidateCDA(nil, input, CDAValidationOptions{Limits: limits})
		if !errors.Is(err, ErrEncapsulatedDocumentTooLarge) || !reflect.DeepEqual(report, CDAValidationReport{}) {
			t.Fatalf("limit %+v returned %+v, %v", limits, report, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report, err = ValidateCDA(ctx, input, CDAValidationOptions{})
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(report, CDAValidationReport{}) {
		t.Fatalf("canceled validation returned %+v, %v", report, err)
	}
}

func TestCDAValidationRejectsMalformedAndAmbiguousXML(t *testing.T) {
	valid := cdaValidationDocument(cdaValidationSection(`<paragraph>Visible.</paragraph>`, ""))
	for _, input := range []string{
		`<!DOCTYPE ClinicalDocument SYSTEM "https://example.invalid/secret.dtd">` + valid,
		strings.TrimSuffix(valid, "</ClinicalDocument>"),
		strings.Replace(valid, `<paragraph>`, `<paragraph ID="left" ID="right">`, 1),
		strings.Replace(valid, `<paragraph>`, `<paragraph xmlns:x="urn:test" xmlns:y="urn:test" x:ID="left" y:ID="right">`, 1),
	} {
		report, err := ValidateCDA(nil, []byte(input), CDAValidationOptions{})
		if !errors.Is(err, ErrInvalidDocument) || !reflect.DeepEqual(report, CDAValidationReport{}) {
			t.Fatalf("malformed XML returned %+v, %v", report, err)
		}
	}
	report, err := ValidateCDA(nil, []byte(valid), CDAValidationOptions{Profile: "unsupported"})
	if !errors.Is(err, ErrUnsupportedCDAValidationProfile) || !reflect.DeepEqual(report, CDAValidationReport{}) {
		t.Fatalf("profile refusal returned %+v, %v", report, err)
	}
}
