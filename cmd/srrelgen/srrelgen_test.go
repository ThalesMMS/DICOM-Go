package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func readCommittedTSV(t *testing.T) ([]byte, Table) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", defaultInputPath))
	if err != nil {
		t.Fatal(err)
	}
	table, err := parseTSV(data)
	if err != nil {
		t.Fatal(err)
	}
	return data, table
}

// TestCommittedFilesAreCurrent: the vendored TSV is in canonical form and
// sr/relationship_constraints_gen.go is what go generate writes from it.
func TestCommittedFilesAreCurrent(t *testing.T) {
	data, table := readCommittedTSV(t)
	date := regexp.MustCompile(`chtml pages on (\S+)`).FindSubmatch(data)
	if date == nil {
		t.Fatal("TSV header has no extraction date")
	}
	if got := formatTSV(table, string(date[1])); !bytes.Equal(got, data) {
		t.Fatal("internal/standard/sr_relationships.tsv is not in canonical form; re-extract it")
	}
	want, err := generateSource(defaultInputPath, table)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join("..", "..", defaultOutputPath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), want) {
		t.Fatal("sr/relationship_constraints_gen.go is stale; run go generate ./sr")
	}
	if table.Edition == "" || len(table.IODs) != len(sopClasses) {
		t.Fatalf("edition %q, %d IODs", table.Edition, len(table.IODs))
	}
}

const pageTemplate = `<span class="documentreleaseinformation">DICOM PS3.3 2099z - Information Object Definitions</span>
<p>Relationship Constraints POLICY</p>
<div class="table"><p class="title">
<strong>Table` + " " + `A.35.NUMBER-2.` + " " + `Relationship Content Constraints for NAME IOD</strong>
</p>
<div class="table-contents"><table>
<thead><tr><th><p>Source Value Type</p></th><th><p>Relationship Type (Enumerated Values)</p></th><th><p>Target Value Type</p></th></tr></thead>
<tbody>ROWS</tbody></table></div></div>`

func page(number, name, policy string, rows ...[3]string) string {
	var body strings.Builder
	for _, row := range rows {
		body.WriteString("<tr>")
		for _, cell := range row {
			body.WriteString("<td><p><a id=\"x\"></a>" + cell + "\n</p></td>")
		}
		body.WriteString("</tr>")
	}
	return strings.NewReplacer("NUMBER", number, "NAME", name, "POLICY", policy, "ROWS", body.String()).Replace(pageTemplate)
}

func TestExtractPageNormalizesTermsAndPolicies(t *testing.T) {
	tests := []struct {
		name, number, iod, policy string
		rows                      [][3]string
		wantPolicy                string
		wantRules                 []Rule
	}{
		{
			name: "by-value", number: "7", iod: "Procedure Log",
			policy:     "Relationships between Content Items in the content of this IOD shall be conveyed in the by-value mode.",
			rows:       [][3]string{{"any type (except CONTAINER)", "HAS PROPERTIES", "TEXT, CODE"}},
			wantPolicy: "none",
			wantRules:  []Rule{{"*-CONTAINER", "HAS PROPERTIES", "TEXT,CODE"}},
		},
		{
			name: "all but two", number: "3", iod: "Comprehensive SR",
			policy:     "Relationships may be conveyed either by-value or by-reference. The HAS CONCEPT MOD and CONTAINS relationships shall not be conveyed by-reference.",
			rows:       [][3]string{{"CONTAINER", "CONTAINS", "TEXT, CONTAINER (see below)"}},
			wantPolicy: "all-except:HAS CONCEPT MOD,CONTAINS",
			wantRules:  []Rule{{"CONTAINER", "CONTAINS", "TEXT,CONTAINER"}},
		},
		{
			name: "all", number: "15", iod: "Extensible SR",
			policy:     "Relationships may be conveyed either by-value or by-reference.",
			rows:       [][3]string{{"any type", "any type other than CONTAINS", "any type"}},
			wantPolicy: "all",
			wantRules:  []Rule{{"*", "*-CONTAINS", "*"}},
		},
		{
			name: "only listed", number: "5", iod: "Mammography CAD SR",
			policy:     "The IOD makes extensive use of by-reference INFERRED FROM, by-reference HAS PROPERTIES and by-reference SELECTED FROM relationships. Other relationships by-reference are forbidden.",
			rows:       [][3]string{{"SCOORD", "SELECTED FROM", "IMAGE"}},
			wantPolicy: "only:INFERRED FROM,HAS PROPERTIES,SELECTED FROM",
			wantRules:  []Rule{{"SCOORD", "SELECTED FROM", "IMAGE"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			iods, err := extractPage(page(test.number, test.iod, test.policy, test.rows...))
			if err != nil {
				t.Fatal(err)
			}
			if len(iods) != 1 {
				t.Fatalf("%d IODs", len(iods))
			}
			iod := iods[0]
			if iod.ByReference != test.wantPolicy || !reflect.DeepEqual(iod.Rules, test.wantRules) ||
				iod.Table != "A.35."+test.number+"-2" || iod.SOPClassUID != sopClasses[test.iod] {
				t.Fatalf("extracted %+v", iod)
			}
		})
	}
	for name, bad := range map[string]string{
		"unknown IOD":        page("99", "Future SR", "shall be conveyed by-value.", [3]string{"CONTAINER", "CONTAINS", "TEXT"}),
		"unknown value type": page("1", "Basic Text SR", "shall be conveyed by-value.", [3]string{"CONTAINER", "CONTAINS", "HOLOGRAM"}),
		"unknown relation":   page("1", "Basic Text SR", "shall be conveyed by-value.", [3]string{"CONTAINER", "IS NEAR", "TEXT"}),
		"no policy":          page("1", "Basic Text SR", "Nothing about modes.", [3]string{"CONTAINER", "CONTAINS", "TEXT"}),
	} {
		if _, err := extractPage(bad); err == nil {
			t.Errorf("%s: extracted", name)
		}
	}
}

func TestParseTSVRejectsInvalidTables(t *testing.T) {
	valid := "edition\t2099z\niod\t1.2.3\tA.35.1-2\tnone\tTest SR\nrel\tCONTAINER\tCONTAINS\tTEXT\n"
	if _, err := parseTSV([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"no edition":     strings.Replace(valid, "edition\t2099z\n", "", 1),
		"no rule":        "edition\t2099z\niod\t1.2.3\tA.35.1-2\tnone\tTest SR\n",
		"unknown policy": strings.Replace(valid, "\tnone\t", "\tsome\t", 1),
		"unknown term":   strings.Replace(valid, "\tTEXT\n", "\tTEXTS\n", 1),
		"duplicate":      valid + "iod\t1.2.3\tA.35.2-2\tnone\tOther SR\nrel\tCONTAINER\tCONTAINS\tTEXT\n",
		"rel first":      "edition\t2099z\nrel\tCONTAINER\tCONTAINS\tTEXT\n",
		"unknown row":    valid + "note\tx\n",
	} {
		if _, err := parseTSV([]byte(bad)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

func TestRunUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"extra"}, &stdout, &stderr); code != 2 {
		t.Fatalf("unexpected argument: exit %d", code)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sect_A.35.html"), []byte(page("1", "Basic Text SR", "shall be conveyed by-value.", [3]string{"CONTAINER", "CONTAINS", "TEXT"})), 0o644); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if code := run([]string{"-extract", dir, "-output", filepath.Join(dir, "out.tsv")}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "found 1 relationship tables") {
		t.Fatalf("partial extraction: exit %d, %s", code, stderr.String())
	}
	_, table := readCommittedTSV(t)
	input := filepath.Join(dir, "table.tsv")
	if err := os.WriteFile(input, formatTSV(table, "2026-09-26"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "gen.go")
	if code := run([]string{"-input", input, "-output", output}, &stdout, &stderr); code != 0 {
		t.Fatalf("generate: exit %d, %s", code, stderr.String())
	}
	if data, err := os.ReadFile(output); err != nil || !bytes.Contains(data, []byte(`const RelationshipConstraintsEdition = "`+table.Edition+`"`)) {
		t.Fatalf("generated file: %v", err)
	}
}
