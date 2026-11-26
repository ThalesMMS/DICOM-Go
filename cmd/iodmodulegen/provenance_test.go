package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRuleSourceGenerationPreservesXrefsAndLegacy(t *testing.T) {
	xml := strings.Replace(testBook, `<table xml:id="table_A">`, `<section xml:id="sect_scope"><title>Scope</title><table xml:id="table_A">`, 1)
	xml = strings.Replace(xml, `</tbody></table>`, `</tbody></table></section>`, 1)
	xml = strings.Replace(xml, "Required if Rows (0028,0010) is present.", `Required if Rows (0028,0010) is present. See <xref linkend="sect_other"/> and <xref linkend="table_other"/>.`, 1)
	book, err := parse(strings.NewReader(xml))
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := generateRules(book)
	if err != nil {
		t.Fatal(err)
	}
	modules, err := generate(book)
	if err != nil {
		t.Fatal(err)
	}
	first, err := generateRuleSources(book)
	if err != nil {
		t.Fatal(err)
	}
	second, err := generateRuleSources(book)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatal("source generation nondeterministic")
	}
	for _, want := range []string{`sect_other`, `table_other`, `table_B`, `sect_scope`, `PS3.3 2099z`, book.digest, `table_A/row-2/index-1`} {
		if !strings.Contains(string(first), want) {
			t.Fatalf("source metadata lacks %s", want)
		}
	}
	newLegacy, _ := generateRules(book)
	newModules, _ := generate(book)
	if !bytes.Equal(legacy, newLegacy) || !bytes.Equal(modules, newModules) {
		t.Fatal("source generation mutated legacy projection")
	}
}

func TestExternalFamilySourceBindingsAreNarrow(t *testing.T) {
	pps := ruleRow{tag: "00081111", typ: "1C", condition: "Required if a Performed Procedure Step SOP Class was involved in the creation of this Series."}
	if externalFamily("Enhanced Series Module", pps) != "performed-procedure-step-participation" {
		t.Fatal("PPS binding lost")
	}
	pps.condition = "Required if the Modality Performed Procedure Step SOP Class is supported."
	if externalFamily("Enhanced Series Module", pps) != "" {
		t.Fatal("different external fact broadened")
	}
	consistent := ruleRow{tag: "00081090", typ: "1C", condition: "Required if present and consistent in the contributing SOP Instances."}
	if externalFamily("General Contributing Sources Macro", consistent) != "contributing-sop-instance-consistency" {
		t.Fatal("consistency binding lost")
	}
	if externalFamily("Unrelated Macro", consistent) != "" {
		t.Fatal("source scope ignored")
	}
	consistent.tag = "DEADBEEF"
	if externalFamily("General Contributing Sources Macro", consistent) != "" {
		t.Fatal("unsupported tag bound")
	}
}
