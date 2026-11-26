package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

const constraintsBook = `<book xmlns="http://docbook.org/ns/docbook"><subtitle>DICOM PS3.3 2099z - Test</subtitle>
<table xml:id="table_A"><caption>Test Module Attributes</caption><tbody>
<tr><td>Include <xref linkend="table_B"/> and <xref linkend="table_C"/></td><td/><td/><td/></tr>
<tr><td>Flag</td><td>(0010,0040)</td><td>2</td><td><variablelist><title>Enumerated Values:</title><varlistentry><term>M</term><listitem>Meaning with a value that is not a literal.</listitem></varlistentry><varlistentry><term>F</term><listitem/></varlistentry></variablelist></td></tr>
<tr><td>&gt;PI</td><td>(0028,0004)</td><td>1</td><td><variablelist><title>Defined Terms for Value 1:</title><varlistentry><term><emphasis>MIP</emphasis></term><listitem>subtype elsewhere</listitem></varlistentry></variablelist></td></tr>
<tr><td>Bits</td><td>(0028,0100)</td><td>1</td><td><variablelist><title>Enumerated Values if Segmentation Type (0062,0001) is LABELMAP and Bits Stored (0028,0101) is 8:</title><varlistentry><term>8</term><listitem/></varlistentry></variablelist></td></tr>
<tr><td>Unproved</td><td>(0028,1055)</td><td>3</td><td><variablelist><title>Defined Terms for CT:</title><varlistentry><term>BONE</term><listitem/></varlistentry></variablelist><para>See <xref linkend="sect_unknown"/> for Enumerated Values.</para></td></tr>
<tr><td>Overlays</td><td>(60xx,0040)</td><td>1</td><td><variablelist><title>Enumerated Values:</title><varlistentry><term>G</term><listitem/></varlistentry></variablelist></td></tr>
</tbody></table>
<table xml:id="table_B"><caption>One Macro Attributes</caption><tbody><tr><td>Text</td><td>(0008,0060)</td><td>1</td><td/></tr></tbody></table>
<table xml:id="table_C"><caption>Two Macro Attributes</caption><tbody><tr><td>Text</td><td>(0008,0060)</td><td>1</td><td/></tr></tbody></table>
</book>`

func TestConstraintsGenerationRowIdentityAndQualifiers(t *testing.T) {
	book, err := parse(strings.NewReader(constraintsBook))
	if err != nil {
		t.Fatal(err)
	}
	got := constraintsForTable(book, book.tables["table_A"], "PS3.3 2099z")
	// Two emitted Include rows precede source row 2. Source ordinal and
	// emitted index deliberately differ; attaching by source ordinal is wrong.
	sex := got[2][0]
	if sex.Source.Row != 2 || sex.Source.Index != 2 || sex.Source.Tag != 0x00100040 || sex.Kind != "enumerated" || len(sex.Values) != 2 {
		t.Fatalf("wrong row binding: %+v", sex)
	}
	position := got[3][0]
	if position.Position != 1 || position.Values[0] != "MIP" || position.Source.Depth != 1 {
		t.Fatalf("mixed content or position: %+v", position)
	}
	pred := got[4][0].Predicate
	if pred.Operator != "and" || len(pred.Children) != 2 || pred.Children[0].Tag != 0x00620001 {
		t.Fatalf("predicate: %+v", pred)
	}
	for _, index := range []int{5, 6} {
		if got[index][0].Kind != "manual" || got[index][0].Reason == "" {
			t.Fatal("qualified/repeating list became global")
		}
	}
	if len(got[5]) != 2 || got[5][1].Source.Section != "sect_unknown" {
		t.Fatal("unproved xref did not remain explicit")
	}
	first, err := generateConstraints(book)
	if err != nil {
		t.Fatal(err)
	}
	second, err := generateConstraints(book)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatal("nondeterministic generation")
	}
	if bytes.Contains(first, []byte("Meaning with a value")) {
		t.Fatal("listitem prose became a literal")
	}
}

func TestConstraintsTitlePredicateRejectsUnprovedProse(t *testing.T) {
	for _, text := range []string{"a clinical diagnosis is present", "Segmentation Type (0062,0001) is BINARY whenever the operator thinks so", "Condition from an external SOP Instance"} {
		if _, ok := titlePredicate(text); ok {
			t.Fatalf("prose compiled: %q", text)
		}
	}
	for _, text := range []string{"Image Type (0008,0008) Value 3 is THUMBNAIL, LABEL or OVERVIEW", "Pixel Data (7FE0,0010) or Pixel Data Provider URL (0028,7FE0) is present", "Float Pixel Data (7FE0,0008) is present"} {
		if _, ok := titlePredicate(text); !ok {
			t.Fatalf("formal title rejected: %q", text)
		}
	}
}

// Opt-in source qualification: the exact pinned DocBook is intentionally not
// copied into the repository. This proves all three projections deterministic.
func TestConstraintsPinnedDocBookRegeneration(t *testing.T) {
	path := os.Getenv("DICOMGO_IOD_DOCBOOK")
	if path == "" {
		t.Skip("set DICOMGO_IOD_DOCBOOK to the pinned official part03.xml")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsedBook, err := parse(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	outputs := []struct {
		path     string
		generate func(*book) ([]byte, error)
	}{
		{"../../internal/iodmodules/modules_generated.go", generate},
		{"../../internal/iodmodules/rules_generated.go", generateRules},
		{"../../internal/iodmodules/constraints_generated.go", generateConstraints},
	}
	for _, output := range outputs {
		got, err := output.generate(parsedBook)
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(output.path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("pinned projection drifted: %s", output.path)
		}
	}
}

func TestConstraintsPrecedingQualifiedProseRemainsManual(t *testing.T) {
	for _, lead := range []string{"For humans:", "When View Code Sequence (0054,0220) indicates a short axis view, then the Enumerated Values are:"} {
		source := strings.Replace(constraintsBook, "<variablelist><title>Enumerated Values:</title>", "<para>"+lead+"</para><variablelist><title>Enumerated Values:</title>", 1)
		book, err := parse(strings.NewReader(source))
		if err != nil {
			t.Fatal(err)
		}
		got := constraintsForTable(book, book.tables["table_A"], "PS3.3 2099z")[2][0]
		if got.Kind != "manual" || got.Reason != "preceding prose qualifies the list scope" {
			t.Fatalf("antecedent lost: %+v", got)
		}
	}
}

func TestConstraintsUnprojectedIncludesAttributeHasValueOnlyBinding(t *testing.T) {
	source := strings.Replace(constraintsBook, "<td>Flag</td>", "<td>Includes Information</td>", 1)
	book, err := parse(strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	got := constraintsForTable(book, book.tables["table_A"], "PS3.3 2099z")[-1]
	if len(got) != 1 || got[0].Kind != "enumerated" || len(got[0].ParentPath) != 0 || got[0].AttributeType != "2" || got[0].Source.Tag != 0x00100040 || got[0].Source.Row != 2 || got[0].Source.Index != -1 {
		t.Fatalf("unprojected source disappeared or bound to wrong row: %+v", got)
	}
}

func TestConstraintsSupplementalNestingMustBeProved(t *testing.T) {
	for _, parent := range []struct {
		tag    string
		proved bool
	}{{"(0034,0009)", true}, {"(0010,0040)", false}, {"(9999,9999)", false}} {
		source := `<book><subtitle>DICOM PS3.3 2099z - Test</subtitle><table xml:id="table_A"><caption>Test Module Attributes</caption><tbody><tr><td>Parent</td><td>` + parent.tag + `</td><td>1</td><td/></tr><tr><td>&gt;Includes Information</td><td>(0034,000C)</td><td>3</td><td><variablelist><title>Enumerated Values:</title><varlistentry><term>YES</term><listitem/></varlistentry><varlistentry><term>NO</term><listitem/></varlistentry></variablelist></td></tr></tbody></table></book>`
		book, err := parse(strings.NewReader(source))
		if err != nil {
			t.Fatal(err)
		}
		got := constraintsForTable(book, book.tables["table_A"], "PS3.3 2099z")[-1]
		if len(got) != 1 || (got[0].Kind == "enumerated") != parent.proved {
			t.Fatalf("parent=%s: %+v", parent.tag, got)
		}
		if parent.proved && (len(got[0].ParentPath) != 1 || got[0].ParentPath[0] != 0x00340009) {
			t.Fatal("source parent path lost")
		}
	}
}
