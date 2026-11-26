package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCommittedFilesAreCurrent: the catalog TSV is in canonical form and
// sr/template_catalog_gen.go is what go generate writes from it.
func TestCommittedFilesAreCurrent(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", defaultInputPath))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := parseTSV(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := formatTSV(catalog); !bytes.Equal(got, data) {
		t.Fatal("internal/standard/sr_templates.tsv is not in canonical form")
	}
	want, err := generateSource(defaultInputPath, data, catalog)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join("..", "..", defaultOutputPath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), want) {
		t.Fatal("sr/template_catalog_gen.go is stale; run go generate ./sr")
	}
	if catalog.Edition == "" || len(catalog.Templates) == 0 {
		t.Fatalf("edition %q, %d templates", catalog.Edition, len(catalog.Templates))
	}
}

// syntheticDocBook follows the structure of the PS3.16 DocBook: the edition
// in the subtitle, one section per template with its attributes as a
// variablelist and the template table. The templates are invented.
const syntheticDocBook = `<?xml version="1.0" encoding="UTF-8"?>
<book xmlns="http://docbook.org/ns/docbook" xmlns:xml="http://www.w3.org/XML/1998/namespace">
<info><subtitle>DICOM PS3.16 2099z - Content Mapping Resource</subtitle></info>
<chapter xml:id="chapter_A">
<section xml:id="sect_TID_9001" label="TID 9001">
<title>Synthetic Report</title>
<variablelist><varlistentry><term>Type:</term><listitem><para>Extensible</para></listitem></varlistentry>
<varlistentry><term>Order:</term><listitem><para>Significant</para></listitem></varlistentry>
<varlistentry><term>Root:</term><listitem><para>Yes</para></listitem></varlistentry></variablelist>
<table xml:id="table_TID_9001_parameters"><thead><tr><th><para>Parameter Name</para></th><th><para>Parameter Usage</para></th></tr></thead>
<tbody><tr><td><para>$Thing</para></td><td><para>Concept</para></td></tr></tbody></table>
<table xml:id="table_TID_9001"><thead><tr><th/><th><para>NL</para></th><th><para>Rel with Parent</para></th><th><para>VT</para></th><th><para>Concept Name</para></th><th><para>VM</para></th><th><para>Req Type</para></th><th><para>Condition</para></th><th><para>Value Set Constraint</para></th></tr></thead>
<tbody>
<tr><td><para>1</para></td><td/><td/><td><para>CONTAINER</para></td><td><para>EV (126000, DCM, “Imaging Measurement Report”)</para></td><td><para>1</para></td><td><para>M</para></td><td/><td/></tr>
<tr><td><para>2</para></td><td><para>&gt;</para></td><td><para>CONTAINS</para></td><td><para>INCLUDE</para></td><td><para>DTID 9002 “Synthetic Group”</para></td><td><para>1-n</para></td><td><para>U</para></td><td/><td><para>$Thing = BCID 218 “Quantitative Image Feature”</para><para>$Units = DCID 7181 “Units”</para></td></tr>
<tr><td><para>3</para></td><td><para>&gt;</para></td><td><para>CONTAINS</para></td><td><para>CONTAINER</para></td><td><para>EV (126010, DCM, "Imaging Measurements")</para></td><td><para>1</para></td><td><para>MC</para></td><td><para>Required if row 4 is absent</para></td><td/></tr>
<tr><td><para>4</para></td><td><para>&gt;</para></td><td><para>CONTAINS</para></td><td><para>TEXT</para></td><td><para>$Thing</para></td><td><para>1</para></td><td><para>MC</para></td><td><para>XOR row 3</para></td><td/></tr>
<tr><td><para>5</para></td><td><para>&gt;&gt;</para></td><td><para>R-INFERRED FROM</para></td><td><para>CODE</para></td><td><para>DCID 7021 “Measurement Report Document Titles”</para></td><td><para>1</para></td><td><para>UC</para></td><td><para>IF row 1 value is something</para></td><td><para>DCID 244 “Laterality”</para></td></tr>
</tbody></table>
</section>
<section xml:id="sect_TID_9002" label="TID 9002">
<title>Synthetic Group</title>
<variablelist><varlistentry><term>Type:</term><listitem><para>Non-Extensible</para></listitem></varlistentry>
<varlistentry><term>Order:</term><listitem><para>Non-Significant</para></listitem></varlistentry>
<varlistentry><term>Root:</term><listitem><para>No</para></listitem></varlistentry></variablelist>
<table xml:id="table_TID_9002"><thead><tr><th/><th><para>NL</para></th><th><para>Rel with Parent</para></th><th><para>VT</para></th><th><para>Concept Name</para></th><th><para>VM</para></th><th><para>Req Type</para></th><th><para>Condition</para></th><th><para>Value Set Constraint</para></th></tr></thead>
<tbody>
<tr><td><para>1</para></td><td/><td/><td><para>CONTAINER</para></td><td><para>EV (125007, DCM, "Measurement Group")</para></td><td><para>1</para></td><td><para>M</para></td><td/><td/></tr>
<tr><td><para>2</para></td><td><para>&gt;</para></td><td><para>HAS OBS CONTEXT</para></td><td><para>TEXT</para></td><td><para>DT (112039, DCM, "Tracking Identifier")</para></td><td><para>1</para></td><td><para>U</para></td><td/><td/></tr>
</tbody></table>
</section>
<section xml:id="sect_TID_9003" label="TID 9003"><title>Retired Synthetic Template</title><para>Retired.</para></section>
</chapter>
</book>
`

func TestExtractReadsTemplateTables(t *testing.T) {
	catalog, err := Extract(strings.NewReader(syntheticDocBook), "2099-01-02")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if catalog.Edition != "2099z" || catalog.Source != sourceDocBook || len(catalog.SHA256) != 64 || catalog.Extracted != "2099-01-02" {
		t.Fatalf("catalog header %+v", catalog)
	}
	if len(catalog.Templates) != 2 {
		t.Fatalf("templates %+v; want TID 9001 and 9002 (9003 has no table)", catalog.Templates)
	}
	report, group := catalog.Templates[0], catalog.Templates[1]
	if report.ID != 9001 || report.Name != "Synthetic Report" || report.Type != "Extensible" || report.Order != "Significant" || report.Root != "Yes" {
		t.Fatalf("TID 9001 = %+v", report)
	}
	if group.Type != "Non-Extensible" || group.Order != "Non-Significant" || group.Root != "No" || len(group.Rows) != 2 {
		t.Fatalf("TID 9002 = %+v", group)
	}
	want := []Row{
		{Label: "1", VT: "CONTAINER", Concept: `EV (126000, DCM, "Imaging Measurement Report")`, VM: "1", Req: "M"},
		{Label: "2", NL: ">", Rel: "CONTAINS", VT: "INCLUDE", Concept: `DTID 9002 "Synthetic Group"`, VM: "1-n", Req: "U",
			ValueSet: `$Thing = BCID 218 "Quantitative Image Feature"; $Units = DCID 7181 "Units"`},
		{Label: "3", NL: ">", Rel: "CONTAINS", VT: "CONTAINER", Concept: `EV (126010, DCM, "Imaging Measurements")`, VM: "1", Req: "MC",
			Condition: "Required if row 4 is absent", Formal: "absent(4)"},
		{Label: "4", NL: ">", Rel: "CONTAINS", VT: "TEXT", Concept: "$Thing", VM: "1", Req: "MC", Condition: "XOR row 3", Formal: "xor(3)"},
		{Label: "5", NL: ">>", Rel: "R-INFERRED FROM", VT: "CODE", Concept: `DCID 7021 "Measurement Report Document Titles"`, VM: "1", Req: "UC",
			Condition: "IF row 1 value is something", ValueSet: `DCID 244 "Laterality"`},
	}
	if len(report.Rows) != len(want) {
		t.Fatalf("rows %+v", report.Rows)
	}
	for i := range want {
		if report.Rows[i] != want[i] {
			t.Errorf("row %d = %+v\nwant %+v", i+1, report.Rows[i], want[i])
		}
	}
	round, err := parseTSV(formatTSV(catalog))
	if err != nil {
		t.Fatalf("parseTSV(formatTSV): %v", err)
	}
	if !bytes.Equal(formatTSV(round), formatTSV(catalog)) {
		t.Fatal("TSV does not round-trip")
	}
	source, err := generateSource("synthetic.tsv", formatTSV(catalog), catalog)
	if err != nil {
		t.Fatalf("generateSource: %v", err)
	}
	for _, fragment := range []string{
		`evaluate: &catalogCondition{op: "absent", rows: []string{"4"}}`,
		`concept: "CID", conceptCID: 7021, conceptStrength: "D"`,
		`relationship: "INFERRED FROM", byReference: true`,
		`valueSetCID: 244, valueSetStrength: "D"`,
		`StandardTemplateSource = "part16.xml"`,
	} {
		if !bytes.Contains(source, []byte(fragment)) {
			t.Errorf("generated source lacks %s", fragment)
		}
	}
}

func TestExtractRefusesWhatItCannotInterpret(t *testing.T) {
	tests := []struct{ name, from, to string }{
		{"not PS3.16", "DICOM PS3.16 2099z", ""},
		{"unknown value type", "<para>TEXT</para></td><td><para>$Thing", "<para>TXT</para></td><td><para>$Thing"},
		{"unknown relationship", "<para>HAS OBS CONTEXT</para>", "<para>HAS SOMETHING</para>"},
		{"unknown concept notation", "<para>$Thing</para></td><td><para>1</para>", "<para>Thing</para></td><td><para>1</para>"},
		{"missing type", "<term>Type:</term><listitem><para>Non-Extensible</para>", "<term>Kind:</term><listitem><para>Non-Extensible</para>"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := strings.Replace(syntheticDocBook, test.from, test.to, 1)
			if source == syntheticDocBook {
				t.Fatal("fixture replacement did not apply")
			}
			if _, err := Extract(strings.NewReader(source), "2099-01-02"); err == nil {
				t.Fatal("Extract accepted the changed DocBook")
			}
		})
	}
}

func TestDeriveFormalRecognizesRowPresenceWordings(t *testing.T) {
	tests := []struct{ label, text, want string }{
		{"6", "XOR Rows 10, 12", "xor(10,12)"},
		{"6", "XOR row 5", "xor(5)"},
		{"6", "Required if row 10 and 12 are absent.", "absent(10,12)"},
		{"6", "Required if Row 3 is not present", "absent(3)"},
		{"6", "Shall be present if row 7 is present", "present(7)"},
		{"6", "IFF row 2b is present", "present(2b)"},
		{"6", "At least one of rows 6, 10 and 12 shall be present", "absent(10,12)"},
		{"6", "IF Row 10 and Row 12 are absent", "absent(10,12)"},
		{"6", "IF Rows 3, 4 are absent", "absent(3,4)"},
		{"6", "IF Row 4 not present", "absent(4)"},
		{"6", "IF Row 4 does not exist", "absent(4)"},
		{"6", "IF Row 6 or Row 7 is absent", ""},
		{"6", "IF Rows 3 and 4 are present", "and(present(3),present(4))"},
		{"6", "IF Row 3 or 4 is present", "present(3,4)"},
		{"6", "XOR with Row 9", "xor(9)"},
		{"6", "IF row 1 value = (121007, DCM, \"Device\")", ""},
		{"6", "Required if all aspects of observer context are not inherited.", ""},
		{"6", "", ""},
	}
	for _, test := range tests {
		if got := deriveFormal(test.label, test.text); got != test.want {
			t.Errorf("deriveFormal(%q, %q) = %q, want %q", test.label, test.text, got, test.want)
		}
	}
}

func TestInterpretChecksRowsAndConditions(t *testing.T) {
	valid := func() Template {
		return Template{ID: 1, Type: "Extensible", Rows: []Row{
			{Label: "1", VT: "CONTAINER", VM: "1", Req: "M"},
			{Label: "2", NL: ">", Rel: "CONTAINS", VT: "TEXT", VM: "1-n", Req: "MC", Formal: "and(absent(3),not(present(4)))"},
			{Label: "3", NL: ">", Rel: "CONTAINS", VT: "CODE", VM: "1", Req: "U"},
			{Label: "4", NL: ">", Rel: "CONTAINS", VT: "NUM", VM: "2-3", Req: "UC", Formal: `value(3,(121007, DCM, "Device"))`},
			{Label: "5", NL: ">>", Rel: "HAS CONCEPT MOD", VT: "CODE", VM: "1", Req: "MC", Formal: "absent(3)"},
		}}
	}
	rows, err := interpret(valid())
	if err != nil {
		t.Fatalf("interpret: %v", err)
	}
	if rows[1].Formal == nil || rows[1].Formal.String() != "and(absent(3),not(present(4)))" {
		t.Fatalf("row 2 formal %+v", rows[1].Formal)
	}
	if rows[3].Formal != nil || rows[4].Formal != nil {
		t.Fatal("a value condition or one naming a row of another parent is evaluable")
	}
	if rows[3].Min != 2 || rows[3].Max != 3 || rows[1].Max != 0 || rows[4].Parent != 3 {
		t.Fatalf("rows %+v", rows)
	}
	tests := []struct {
		name   string
		change func(*Template)
	}{
		{"skipped level", func(t *Template) { t.Rows[2].NL = ">>>" }},
		{"duplicate label", func(t *Template) { t.Rows[2].Label = "2" }},
		{"bad VM", func(t *Template) { t.Rows[2].VM = "3-1" }},
		{"bad Req Type", func(t *Template) { t.Rows[2].Req = "X" }},
		{"formal on an M row", func(t *Template) { t.Rows[0].Formal = "absent(2)" }},
		{"unknown row in a condition", func(t *Template) { t.Rows[1].Formal = "absent(9)" }},
		{"bad formal", func(t *Template) { t.Rows[1].Formal = "maybe(3)" }},
		{"bad include", func(t *Template) { t.Rows[2].VT, t.Rows[2].Concept = "INCLUDE", "TID one" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			template := valid()
			test.change(&template)
			if _, err := interpret(template); err == nil {
				t.Fatal("interpret accepted the change")
			}
		})
	}
}

func TestRunNeedsADateToExtract(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-extract", "part16.xml"}, &stdout, &stderr); code != 2 {
		t.Fatalf("run = %d, %s", code, stderr.String())
	}
}

func TestAlphanumericExtractionRoundTripAndIncludes(t *testing.T) {
	source := strings.NewReplacer(
		"sect_TID_9002", "sect_TID_9002A",
		"table_TID_9002", "table_TID_9002A",
		"DTID 9002 “Synthetic Group”", `D<xref linkend="sect_TID_9002A"/> “Synthetic Group”`,
	).Replace(syntheticDocBook)
	catalog, err := Extract(strings.NewReader(source), "2099-01-02")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Templates) != 2 || catalog.Templates[0].ID != 9001 || catalog.Templates[0].Identifier != "" ||
		catalog.Templates[1].ID != 0 || catalog.Templates[1].Identifier != "9002A" {
		t.Fatalf("numeric/alpha identities: %+v", catalog.Templates)
	}
	rows, err := interpret(catalog.Templates[0])
	if err != nil || rows[1].Include != 0 || rows[1].IncludeIdentifier != "9002A" {
		t.Fatalf("alphanumeric include: %+v %v", rows, err)
	}
	tsv := formatTSV(catalog)
	round, err := parseTSV(tsv)
	if err != nil || !bytes.Equal(formatTSV(round), tsv) {
		t.Fatalf("alphanumeric TSV roundtrip: %v", err)
	}
	generated, err := generateSource("synthetic.tsv", tsv, round)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`id: 9001, name:`, `id: 0, identifier: "9002A"`, `includeIdentifier: "9002A"`} {
		if !bytes.Contains(generated, []byte(fragment)) {
			t.Errorf("generated source lacks %s", fragment)
		}
	}
	if bytes.Contains(generated, []byte("include: -1")) {
		t.Fatal("alphanumeric include emitted as unresolved sentinel")
	}
	if xrefText("sect_TID_10003A") != "TID 10003A" || xrefText("sect_TID_10003a") != "" {
		t.Fatal("DocBook cross-reference identity mismatch")
	}
}

func TestTemplateIdentifierOrderingAndRefusals(t *testing.T) {
	row := Row{Label: "1", VT: "TEXT", VM: "1", Req: "M"}
	catalog := Catalog{Edition: "2099z", Source: sourceCurated, SHA256: "-", Extracted: "-", Templates: []Template{
		{ID: 10004, Type: "Extensible", Rows: []Row{row}},
		{Identifier: "10003C", Type: "Extensible", Rows: []Row{row}},
		{ID: 2, Type: "Extensible", Rows: []Row{row}},
		{Identifier: "10003A", Type: "Extensible", Rows: []Row{row}},
		{ID: 10003, Type: "Extensible", Rows: []Row{row}},
		{Identifier: "10003B", Type: "Extensible", Rows: []Row{row}},
	}}
	round, err := parseTSV(formatTSV(catalog))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2", "10003", "10003A", "10003B", "10003C", "10004"}
	for i, template := range round.Templates {
		if template.identifier() != want[i] {
			t.Fatalf("identity order at %d: %q want %q", i, template.identifier(), want[i])
		}
	}
	canonical := formatTSV(round)
	for _, id := range []string{"0", "-1", "10003a", "10003AB", "10003.A", "+10003A", "010003A", strings.Repeat("9", 128)} {
		t.Run(id, func(t *testing.T) {
			bad := strings.Replace(string(canonical), "T\t10003A\t", "T\t"+id+"\t", 1)
			if _, err := parseTSV([]byte(bad)); err == nil {
				t.Fatal("malformed declaration admitted")
			}
			if _, err := interpret(Template{ID: 1, Rows: []Row{{Label: "1", VT: "INCLUDE", Concept: "DTID " + id, VM: "1", Req: "M"}}}); err == nil {
				t.Fatal("malformed include admitted")
			}
		})
	}
	for _, extra := range []string{
		"T\t10003A\tDuplicate\tExtensible\t\t\n",
		"T\t010003\tNumeric alias\tExtensible\t\t\n",
	} {
		if _, err := parseTSV(append(append([]byte{}, canonical...), []byte(extra)...)); err == nil {
			t.Fatal("duplicate canonical template admitted")
		}
	}
	if err := check(Catalog{Templates: []Template{{ID: 10003, Identifier: "10003A", Type: "Extensible", Rows: []Row{row}}}}); err == nil {
		t.Fatal("conflicting numeric and alpha fields admitted")
	}
	if err := check(Catalog{Templates: []Template{{ID: 10003, Type: "Extensible", Rows: []Row{row}}, {ID: 10003, Type: "Extensible", Rows: []Row{row}}}}); err == nil {
		t.Fatal("duplicate authored identity admitted")
	}
}

func TestExtractRefusesDuplicateAndMalformedTemplateIdentities(t *testing.T) {
	for _, id := range []string{"9001", "9002a", "9002AB", "09002A", "0", strings.Repeat("9", 128)} {
		t.Run(id, func(t *testing.T) {
			source := strings.Replace(syntheticDocBook, `xml:id="sect_TID_9002"`, `xml:id="sect_TID_`+id+`"`, 1)
			if _, err := Extract(strings.NewReader(source), "2099-01-02"); err == nil {
				t.Fatal("duplicate or malformed template identity admitted")
			}
		})
	}
}

func TestPinnedCatalogPreservesNumericAndAlphanumericTemplates(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", defaultInputPath))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := parseTSV(data)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Edition != "2026d" || catalog.Source != sourceDocBook || catalog.SHA256 != "293912fbc2acefe5d53dffa449070db488134da29aaa452e31d610d403888899" {
		t.Fatal("pinned source provenance changed")
	}
	numeric := 0
	alphaRows := map[string]int{}
	for _, template := range catalog.Templates {
		if template.Identifier == "" {
			numeric++
		} else {
			alphaRows[template.Identifier] = len(template.Rows)
		}
	}
	if numeric != 404 || len(alphaRows) != 3 || alphaRows["10003A"] != 5 || alphaRows["10003B"] != 27 || alphaRows["10003C"] != 23 {
		t.Fatalf("pinned template inventory: numeric %d alpha %v", numeric, alphaRows)
	}
}
