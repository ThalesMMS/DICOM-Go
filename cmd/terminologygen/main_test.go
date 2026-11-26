package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCommittedFilesAreCurrent regenerates the named-constant packages
// from the committed TSV files and compares them with the committed Go.
func TestCommittedFilesAreCurrent(t *testing.T) {
	dir := filepath.Join("..", "..", "codes")
	files, err := generateFromDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(namedSchemes) {
		t.Fatalf("%d files generated", len(files))
	}
	for path, want := range files {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s is stale: run go generate ./codes", path)
		}
	}
}

// testBook is a minimal PS3.16 DocBook: Annex D with a current and a
// retired code, a template paragraph and two Context Groups, one including
// the other and one defined by reference.
const testBook = `<?xml version="1.0" encoding="utf-8"?>
<book xmlns="http://docbook.org/ns/docbook" xmlns:xl="http://www.w3.org/1999/xlink">
<subtitle>DICOM PS3.16 2099z - Content Mapping Resource</subtitle>
<section xml:id="sect_TID_1"><table><tbody><tr><td><para>EV <link xl:href="x">(363698007, SCT, "Finding Site")</link></para></td></tr>
<tr><td><para>UNITS = EV (mm, UCUM, "millimeter")</para></td></tr></tbody></table></section>
<section label="CID 10" xml:id="sect_CID_10"><title>Units Group</title>
<variablelist>
<varlistentry><term><emphasis role="bold">Keyword:</emphasis></term><listitem><para><emphasis role="bold">UnitsGroup</emphasis></para></listitem></varlistentry>
<varlistentry><term><emphasis role="bold">Type:</emphasis></term><listitem><para><emphasis role="bold">Non-Extensible</emphasis></para></listitem></varlistentry>
<varlistentry><term><emphasis role="bold">Version:</emphasis></term><listitem><para><emphasis role="bold">20990101</emphasis></para></listitem></varlistentry>
<varlistentry><term><emphasis role="bold">UID:</emphasis></term><listitem><para><emphasis role="bold">1.2.840.10008.6.1.9999</emphasis></para></listitem></varlistentry>
</variablelist>
<table xml:id="table_CID_10"><thead><tr><th><para>Coding Scheme Designator</para></th><th><para>Code Value</para></th><th><para>Code Meaning</para></th></tr></thead>
<tbody><tr><td><para>UCUM</para></td><td><para>mm</para></td><td><para>mm</para></td></tr></tbody></table>
</section>
<section label="CID 11" xml:id="sect_CID_11"><title>Everything</title>
<variablelist><varlistentry><term><emphasis role="bold">Keyword:</emphasis></term><listitem><para>Everything</para></listitem></varlistentry>
<varlistentry><term><emphasis role="bold">Type:</emphasis></term><listitem><para>Extensible</para></listitem></varlistentry></variablelist>
<table xml:id="table_CID_11"><thead><tr><th><para>Coding Scheme Designator</para></th><th><para>Code Value</para></th><th><para>Code Meaning</para></th></tr></thead>
<tbody><tr><td colspan="3"><para><emphasis role="italic">Include <xref linkend="sect_CID_10"/></emphasis></para></td></tr>
<tr><td><para>DCM</para></td><td><para>121071</para></td><td><para>Finding</para></td></tr></tbody></table>
</section>
<section label="CID 12" xml:id="sect_CID_12"><title>Language</title><para>ISO 639 codes.</para></section>
<chapter xml:id="chapter_D"><table xml:id="table_D-1"><thead><tr><th/><th/><th/><th/></tr></thead><tbody>
<tr><td><para xml:id="DCM_121071">121071</para></td><td><para>Finding</para></td><td><para>A finding.</para></td><td><para/></td></tr>
<tr><td><para><emphasis role="italic">AS</emphasis></para></td><td><para><emphasis role="italic">Angioscopy</emphasis></para></td><td/><td/></tr>
</tbody></table></chapter>
</book>`

// TestExtractAndGenerate: extraction reads Annex D, the Context Groups and
// the template codes (preferring a CID's meaning), survives the TSV round
// trip, and generates named constants.
func TestExtractAndGenerate(t *testing.T) {
	x, err := Extract(strings.NewReader(testBook))
	if err != nil {
		t.Fatal(err)
	}
	if x.Edition != "PS3.16 2099z" || len(x.SHA256) != 64 {
		t.Fatalf("provenance %q %q", x.Edition, x.SHA256)
	}
	if len(x.DCM) != 2 || x.DCM[0] != (Term{"DCM", "121071", "Finding", false}) || !x.DCM[1].Retired {
		t.Fatalf("DCM terms %+v", x.DCM)
	}
	wantOthers := []Term{{"SCT", "363698007", "Finding Site", false}, {"UCUM", "mm", "mm", false}}
	if len(x.Others) != 2 || x.Others[0] != wantOthers[0] || x.Others[1] != wantOthers[1] {
		t.Fatalf("other terms %+v", x.Others)
	}
	if len(x.ContextGroups) != 3 {
		t.Fatalf("%d context groups", len(x.ContextGroups))
	}
	units, everything, language := x.ContextGroups[0], x.ContextGroups[1], x.ContextGroups[2]
	if units.Keyword != "UnitsGroup" || units.Extensible || units.Version != "20990101" || units.UID != "1.2.840.10008.6.1.9999" || len(units.Codes) != 1 {
		t.Fatalf("CID 10 %+v", units)
	}
	if !everything.Extensible || len(everything.Includes) != 1 || everything.Includes[0] != 10 || len(everything.Codes) != 1 {
		t.Fatalf("CID 11 %+v", everything)
	}
	if !language.WithoutMemberTables {
		t.Fatalf("CID 12 %+v", language)
	}

	var terms, groups bytes.Buffer
	if err := writeTerms(&terms, x); err != nil {
		t.Fatal(err)
	}
	if err := writeContextGroups(&groups, x); err != nil {
		t.Fatal(err)
	}
	back, err := readTSV(&terms, &groups)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.DCM) != 2 || len(back.Others) != 2 || len(back.ContextGroups) != 3 || back.ContextGroups[1].Includes[0] != 10 || !back.ContextGroups[2].WithoutMemberTables {
		t.Fatalf("round trip %+v", back)
	}
	files, err := generate("out", back)
	if err != nil {
		t.Fatal(err)
	}
	dcm := string(files[filepath.Join("out", "dcm", "dcm_gen.go")])
	for _, want := range []string{"Finding = codes.Code{Value: \"121071\"", "Deprecated: retired in PS3.16.", "Angioscopy = codes.Code"} {
		if !strings.Contains(dcm, want) {
			t.Fatalf("dcm package lacks %q:\n%s", want, dcm)
		}
	}
	if sct := string(files[filepath.Join("out", "sct", "sct_gen.go")]); !strings.Contains(sct, "FindingSite = codes.Code{Value: \"363698007\"") {
		t.Fatalf("sct package:\n%s", sct)
	}
}

func TestIdentifiers(t *testing.T) {
	terms := []Term{
		{"DCM", "1", "Area", false}, {"DCM", "2", "Area", true}, // one current: it keeps the name
		{"DCM", "3", "Length", false}, {"DCM", "4", "Length", false}, // two current: both suffixed
		{"DCM", "5", "2D view", false}, {"UCUM", "%", "%", false}, {"UCUM", "mm/s", "mm/s", false}, {"UCUM", "{x}", "", false},
	}
	want := []string{"Area", "Area_2", "Length_3", "Length_4", "N2DView", "Percent", "MmPerS", "CodeX"}
	got := identifiers(terms)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("identifiers = %v, want %v", got, want)
		}
	}
}
