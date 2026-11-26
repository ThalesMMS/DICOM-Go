package main

import (
	"bytes"
	"os"
	"path/filepath"
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

// TestCommittedFilesAreCurrent: the vendored TSV is canonical, its rules are
// the current ones, and dictionary/uid/storage_gen.go is what go generate
// writes from it.
func TestCommittedFilesAreCurrent(t *testing.T) {
	data, table := readCommittedTSV(t)
	if got := formatTSV(table, ""); !bytes.Equal(got, bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))) {
		t.Fatal("internal/standard/storage_sop_classes.tsv is not in canonical form; re-extract it")
	}
	for _, class := range table.Classes {
		category, modality, err := classify(class.Name)
		if err != nil || category != class.Category || modality != class.Modality {
			t.Errorf("%s %q: the rules give %s/%s (%v), the TSV %s/%s; re-extract it", class.UID, class.Name, category, modality, err, class.Category, class.Modality)
		}
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
		t.Fatal("dictionary/uid/storage_gen.go is stale; run go generate ./dictionary/uid")
	}
	if table.Edition != "2026e" {
		t.Fatalf("edition %q", table.Edition)
	}
	// go generate ./dictionary/uid passes the TSV relative to dictionary/uid.
	fromPackage, err := generateSource("../../"+defaultInputPath, table)
	if err != nil || !bytes.Equal(fromPackage, want) {
		t.Fatalf("go generate writes a different header than the module-root run (%v)", err)
	}
}

const pageTemplate = `<span class="documentreleaseinformation">DICOM PS3.4 2099z - Service Class Specifications</span>
<div class="table"><p class="title"><strong>Table` + " " + `NUMBER.` + " " + `Standard SOP Classes</strong></p>
<div class="table-contents"><table>
<thead><tr><th><p>SOP Class Name</p></th><th><p>SOP Class UID</p></th></tr></thead>
<tbody>ROWS</tbody></table></div></div><table><tr><td>Not</td><td>this table</td></tr></table>`

func writePage(t *testing.T, dir, file, number string, rows ...[2]string) {
	t.Helper()
	var b strings.Builder
	for _, row := range rows {
		b.WriteString("<tr><td><p><a>" + row[0] + "</a></p></td><td><p>" + row[1] + "</p></td></tr>")
	}
	page := strings.NewReplacer("NUMBER", number, "ROWS", b.String()).Replace(pageTemplate)
	if err := os.WriteFile(filepath.Join(dir, file), []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExtractDirMergesTablesAndPS36(t *testing.T) {
	dir := t.TempDir()
	// The chtml pages break long UIDs with zero-width spaces.
	writePage(t, dir, "sect_B.5.html", "B.5-1",
		[2]string{"CT Image Storage", "1.2.840.10008.5.1.4.1.1.2"},
		[2]string{"Acquisition Context SR Storage", "1.2.840.10008.5.​1.​4.​1.​1.​88.​71"},
		[2]string{"Digital Mammography X-Ray Image Storage - For Presentation", "1.2.840.10008.5.1.4.1.1.1.2"})
	writePage(t, dir, "sect_B.6.html", "B.6-1", [2]string{"Ultrasound Image Storage", "1.2.840.10008.5.1.4.1.1.6"})
	writePage(t, dir, "sect_GG.3.html", "GG.3-1", [2]string{"Color Palette Storage", "1.2.840.10008.5.1.4.39.1"})
	uids := []byte("# PS3.6\n" +
		"1.2.840.10008.5.1.4.1.1.2\tCT Image Storage\tCTImageStorage\tSOP Class\tfalse\n" +
		"1.2.840.10008.5.1.4.1.1.6\tUltrasound Image Storage (Retired)\tUltrasoundImageStorageRetired\tSOP Class\ttrue\n" +
		"1.2.840.10008.5.1.4.1.1.9\tStandalone Curve Storage (Retired)\tStandaloneCurveStorage\tSOP Class\ttrue\n" +
		"1.2.840.10008.5.1.4.1.1.501.1\tDICOS CT Image Storage\tDICOSCTImageStorage\tSOP Class\tfalse\n" +
		"1.2.840.10008.1.20.1\tStorage Commitment Push Model SOP Class\tStorageCommitmentPushModel\tSOP Class\tfalse\n" +
		"1.2.840.10008.1.3.10\tMedia Storage Directory Storage\tMediaStorageDirectoryStorage\tSOP Class\tfalse\n" +
		"1.2.840.10008.1.2\tImplicit VR Little Endian\tImplicitVRLittleEndian\tTransfer Syntax\tfalse\n")
	table, err := extractDir(dir, uids)
	if err != nil {
		t.Fatal(err)
	}
	table.Date = "2099-01-01"
	if err := table.validate(); err != nil {
		t.Fatal(err)
	}
	got := map[string]Class{}
	var order []string
	for _, class := range table.Classes {
		got[class.UID] = class
		order = append(order, class.UID)
	}
	if table.Edition != "2099z" || len(table.Classes) != 7 {
		t.Fatalf("edition %s, classes %v", table.Edition, order)
	}
	checks := map[string]Class{
		"1.2.840.10008.5.1.4.1.1.2":     {Keyword: "CTImageStorage", Category: "image", Modality: "CT", Source: sourceB5},
		"1.2.840.10008.5.1.4.1.1.88.71": {Category: "sr", Modality: "SR", Source: sourceB5},
		"1.2.840.10008.5.1.4.1.1.1.2":   {Category: "image", Modality: "MG", Source: sourceB5},
		"1.2.840.10008.5.1.4.1.1.6":     {Keyword: "UltrasoundImageStorageRetired", Category: "image", Modality: "US", Source: sourceB6, Retired: true},
		"1.2.840.10008.5.1.4.39.1":      {Category: "nonpatient", Source: sourceGG3},
		"1.2.840.10008.5.1.4.1.1.9":     {Keyword: "StandaloneCurveStorage", Category: "legacy", Source: sourcePS36, Retired: true},
		"1.2.840.10008.5.1.4.1.1.501.1": {Keyword: "DICOSCTImageStorage", Category: "security", Source: sourcePS36},
	}
	for uid, want := range checks {
		class := got[uid]
		class.UID, class.Name = "", ""
		if class != want {
			t.Errorf("%s: %+v, want %+v", uid, class, want)
		}
	}
	if got["1.2.840.10008.5.1.4.1.1.9"].Name != "Standalone Curve Storage" || order[0] != "1.2.840.10008.5.1.4.1.1.1.2" {
		t.Fatalf("name %q, order %v", got["1.2.840.10008.5.1.4.1.1.9"].Name, order)
	}
	// Tables are read in order: a class also in PS3.6 keeps its PS3.4 row.
	if _, _, err := classify("Quantum Flux Storage"); err == nil {
		t.Fatal("an unknown name was classified")
	}
	writePage(t, dir, "sect_GG.3.html", "GG.3-1", [2]string{"Quantum Flux Storage", "1.2.840.10008.5.1.4.99.1"})
	if _, err := extractDir(dir, uids); err == nil || !strings.Contains(err.Error(), "no rule classifies") {
		t.Fatalf("unknown class: %v", err)
	}
	writePage(t, dir, "sect_GG.3.html", "GG.3-1", [2]string{"Color Palette Storage", "1.2.3"})
	if _, err := extractDir(dir, uids); err == nil {
		t.Fatal("a non-DICOM UID was accepted")
	}
}

func TestParseTSVRejectsInvalidTables(t *testing.T) {
	data, _ := readCommittedTSV(t)
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	firstRow := 5
	for name, mutate := range map[string]func([]string) []string{
		"duplicate": func(l []string) []string { return append(l[:firstRow+1], l[firstRow:]...) },
		"unsorted":  func(l []string) []string { l[firstRow], l[firstRow+1] = l[firstRow+1], l[firstRow]; return l },
		"category": func(l []string) []string {
			fields := strings.Split(l[firstRow], "\t")
			fields[3] = "photo"
			l[firstRow] = strings.Join(fields, "\t")
			return l
		},
		"no source header": func(l []string) []string { return append(l[:1], l[2:]...) },
		"no column header": func(l []string) []string { return append(l[:4], l[5:]...) },
		"bad fields":       func(l []string) []string { l[firstRow] += "\textra"; return l },
	} {
		mutated := mutate(append([]string(nil), lines...))
		if _, err := parseTSV([]byte(strings.Join(mutated, "\n"))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRunUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-extract", "dir"}, &stdout, &stderr); code != 2 {
		t.Fatalf("-extract without -date: %d", code)
	}
	if code := run([]string{"stray"}, &stdout, &stderr); code != 2 {
		t.Fatalf("stray argument: %d", code)
	}
}
