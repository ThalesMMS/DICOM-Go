package main

import (
	"strings"
	"testing"
)

const testBook = `<?xml version="1.0" encoding="utf-8"?>
<book xmlns="http://docbook.org/ns/docbook" xml:id="PS3.3">
<subtitle>DICOM PS3.3 2099z - Information Object Definitions</subtitle>
<table xml:id="table_A"><caption>Example Module Attributes</caption><tbody>
<tr><td><para>Patient's Name</para></td><td><para>(0010,0010)</para></td><td><para>2</para></td><td/></tr>
<tr><td><para>&gt;Nested</para></td><td><para>(0008,0100)</para></td><td><para>1C</para></td><td><para>Some text. Required if Rows (0028,0010) is present.</para></td></tr>
<tr><td><para>Include <xref linkend="table_B"/></para></td><td/><td/><td/></tr>
<tr><td><para>Overlay Data</para></td><td><para>(60xx,3000)</para></td><td><para>1</para></td><td/></tr>
</tbody></table>
<table xml:id="table_B"><caption>Shared Macro Attributes</caption><tbody>
<tr><td><para>Rows</para></td><td><para>(0028,0010)</para></td><td><para>1</para></td><td/></tr>
</tbody></table>
<table xml:id="table_C"><caption>Example IOD Modules</caption><tbody>
<tr><td><para>Patient</para></td><td><para>Example</para></td><td><xref linkend="sect_A"/></td><td><para>M</para></td></tr>
<tr><td><para>Other</para></td><td><xref linkend="sect_B"/></td><td><para>C - Required if something</para></td></tr>
</tbody></table>
</book>`

func TestGenerateProjectsModulesAndIODs(t *testing.T) {
	book, err := parse(strings.NewReader(testBook))
	if err != nil {
		t.Fatal(err)
	}
	code, err := generate(book)
	if err != nil {
		t.Fatal(err)
	}
	out := string(code)
	for _, want := range []string{
		`StandardVersion = "PS3.3 2099z"`,
		`"Example Module": {{0x00100010, "2"}, {0x00280010, "1"}}`,
		`"Shared Macro":`,
		`"Example": {{"Example", "M", ""}, {"Other", "C", "Required if something"}}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated code lacks %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "0x00080100") {
		t.Error("nested Attribute listed at the top level")
	}
	rules, err := generateRules(book)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`{0, 0x00100010, "2", "", ""}`,
		`{1, 0x00080100, "1C", "", "Required if Rows (0028,0010) is present."}`,
		`{0, 0, "", "Shared Macro", ""}`,
		`{0, 0, "1", "", ""}`,
		`"Shared Macro": {`,
	} {
		if !strings.Contains(string(rules), want) {
			t.Errorf("generated rules lack %s:\n%s", want, rules)
		}
	}
}

func TestGenerateRejectsShortSubtitle(t *testing.T) {
	for _, subtitle := range []string{"", "DICOM", "DICOM PS3.3", " \t "} {
		if code, err := generate(&book{subtitle: subtitle}); err == nil || code != nil || !strings.Contains(err.Error(), "subtitle") {
			t.Errorf("subtitle %q: code=%q error=%v", subtitle, code, err)
		}
	}
}

func TestGenerateBodyParts(t *testing.T) {
	page := `<html><head><title>L Correspondence of Anatomic Region Codes and Body Part Examined Defined Terms</title></head><body>DICOM PS3.16 2099z<table>`
	for i := 0; i < 120; i++ {
		page += `<tr><td>SCT</td><td>` + string(rune('0'+i%10)) + `</td><td>Region</td><td>PART` + strings.Repeat("X", i) + `</td></tr>`
	}
	page += `<tr><td>SCT</td><td>123</td><td>Chest</td><td>CHEST</td></tr><tr><td>SCT</td><td>999</td><td>Thorax</td><td>CHEST</td></tr></table></body></html>`
	code, err := generateBodyParts([]byte(page), "p")
	if err != nil {
		t.Fatal(err)
	}
	if out := string(code); !strings.Contains(out, `"CHEST":`) || !strings.Contains(out, `{"SCT", "123", "Chest"}`) || !strings.Contains(out, "PS3.16 2099z") {
		t.Fatalf("generated:\n%s", out)
	}
	if _, err := generateBodyParts([]byte("<title>Other</title>"), "p"); err == nil {
		t.Fatal("accepted another page")
	}
}
