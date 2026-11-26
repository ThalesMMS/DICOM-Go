package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGDCMKeyword(t *testing.T) {
	for name, want := range map[string]string{
		"Echo Train Length":                      "EchoTrainLength",
		"Table Speed in mm/rot":                  "TableSpeedInMmRot",
		"Expert Plan No.":                        "ExpertPlanNo",
		"Phase FOV Percent":                      "PhaseFOVPercent",
		"Equipment Type-Specific Information":    "EquipmentTypeSpecificInformation",
		"Number of Detector rows to Reconstruct": "NumberOfDetectorRowsToReconstruct",
		"?":                                      "",
		"?Gantry Tilt?":                          "",
		"2D Phase Oversampling Lines":            "",
		"Unknown":                                "",
		"<internal":                              "",
		"Café":                                   "",
		"":                                       "",
	} {
		if got := gdcmKeyword(name); got != want {
			t.Errorf("gdcmKeyword(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestParseGDCMRow(t *testing.T) {
	d, ok := parseGDCMRow(`  <entry owner="TOSHIBA_MEC_CT3" group="7005" element="xx0a" vr="DS" vm="1" name="Table Speed in mm/rot"/>`)
	if !ok || d.entry.Owner != "TOSHIBA_MEC_CT3" || d.group != "7005" || d.offset != "0A" || d.entry.VR != "DS" || d.entry.Name != "Table Speed in mm/rot" {
		t.Fatalf("parseGDCMRow = %+v, %v", d, ok)
	}
	if d, ok := parseGDCMRow(`<entry group="0021" element="0013" vr="US" vm="1" name="?" owner="SIEMENS MR FMRI"/>`); !ok || d.offset != "13" {
		t.Fatalf("00NN element: %+v, %v", d, ok)
	}
	for _, row := range []string{
		`<entry owner="X" group="60xx" element="xx01" vr="US" vm="1" name="Repeating"/>`,
		`<entry owner="X" group="0019" element="1001" vr="US" vm="1" name="Fixed"/>`,
		`<entry group="0019" element="xx01" vr="US" vm="1" name="No Owner"/>`,
		`<entry owner="X" group="0019" element="xx01" vr="US" vm="1" name="A"/><entry owner="X" group="0019" element="xx02" vr="US" vm="1" name="B"/>`,
		`<entry owner="X" group="0019" element="xx01" vr="US" vm="1" name="Open">`,
		`(0019,"X",01)	US	Dic	1	PrivateTag`,
	} {
		if _, ok := parseGDCMRow(row); ok {
			t.Errorf("parseGDCMRow accepted %q", row)
		}
	}
}

// gdcmFixture returns a synthetic privatedicts.xml excerpt and a selection.
// It is never distributed as manufacturer evidence.
func gdcmFixture() (xmlData, selection []byte) {
	xmlData = []byte(strings.Join([]string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		`<dict>`,
		`  <entry owner="SYNTHETIC A" group="0019" element="xx10" vr="DS" vm="1" name="Synthetic Speed in mm/s"/>`,
		`  <entry owner="SYNTHETIC A" group="0019" element="xx11" vr="LO" vm="1-n" name="Synthetic Label"/>`,
		`  <entry owner="SYNTHETIC A" group="0019" element="xx12" vr="SQ" vm="1" name="Synthetic Sequence"/>`,
		`  <entry owner="SYNTHETIC A" group="0019" element="xx13" vr="US" vm="1" name="?"/>`,
		`  <entry owner="SYNTHETIC A" group="0019" element="xx14" vr="US" vm="1" name="Twice"/>`,
		`  <entry owner="SYNTHETIC A" group="0019" element="0014" vr="SS" vm="1" name="Twice"/>`,
		`</dict>`,
		``,
	}, "\n"))
	selection = []byte("# synthetic\nSYNTHETIC A\t0019\t11\nSYNTHETIC A\t0019\t10\n")
	return xmlData, selection
}

func TestBuildGDCMSourceImportsSelectedRows(t *testing.T) {
	xmlData, selection := gdcmFixture()
	encoded, err := buildGDCMSource(xmlData, selection)
	if err != nil {
		t.Fatal(err)
	}
	s, err := decodeSource(encoded)
	if err != nil {
		t.Fatal(err)
	}
	records, err := validatedRecords(s)
	if err != nil || len(records) != 2 {
		t.Fatalf("validatedRecords = %d records, %v", len(records), err)
	}
	first := s.Records[0]
	hash := sha256.Sum256(xmlData)
	if first.Offset != "10" || first.VR != "DS" || first.Name != "Synthetic Speed in mm/s" || first.Keyword != "SyntheticSpeedInMmS" ||
		first.Origin.SourceLine != 3 || first.Origin.SourceSHA256 != hex.EncodeToString(hash[:]) || first.Origin.Commit != gdcmCommit ||
		!strings.Contains(first.Origin.SourceRow, `element="xx10"`) {
		t.Fatalf("imported record = %+v", first)
	}
	if s.Records[1].VM != "1-n" {
		t.Fatalf("VM = %q", s.Records[1].VM)
	}
	if !bytes.Contains(encoded, []byte(`"source_row": "  <entry owner=`)) {
		t.Fatal("source rows are HTML-escaped")
	}
	if err := verifyGDCM(s, xmlData); err != nil {
		t.Fatal(err)
	}
	again, err := buildGDCMSource(xmlData, []byte("SYNTHETIC A\t0019\t10\nSYNTHETIC A\t0019\t11\n"))
	if err != nil || !bytes.Equal(again, encoded) {
		t.Fatal("selection order changes the imported source")
	}
}

func TestBuildGDCMSourceRefusesUnreviewableRows(t *testing.T) {
	xmlData, _ := gdcmFixture()
	for name, selection := range map[string]string{
		"sequence":        "SYNTHETIC A\t0019\t12\n",
		"uncertain name":  "SYNTHETIC A\t0019\t13\n",
		"two definitions": "SYNTHETIC A\t0019\t14\n",
		"absent":          "SYNTHETIC A\t0019\t20\n",
		"other creator":   "SYNTHETIC B\t0019\t10\n",
		"duplicate key":   "SYNTHETIC A\t0019\t10\nSYNTHETIC A\t0019\t10\n",
		"lowercase key":   "SYNTHETIC A\t0019\t1a\n",
		"padded creator":  "SYNTHETIC A \t0019\t10\n",
		"short line":      "SYNTHETIC A\t0019\n",
		"empty":           "# nothing selected\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := buildGDCMSource(xmlData, []byte(selection)); !errors.Is(err, errSource) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if _, err := importGDCMSource(xmlData, []byte("SYNTHETIC A\t0019\t10\n")); !errors.Is(err, errSource) {
		t.Fatal("importer accepted a privatedicts.xml other than the pinned file")
	}
}

func TestVerifyGDCMDetectsDriftAndConflicts(t *testing.T) {
	xmlData, selection := gdcmFixture()
	encoded, err := buildGDCMSource(xmlData, selection)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := decodeSource(encoded)
	if err := verifyGDCM(s, append(bytes.Clone(xmlData), 'x')); err == nil {
		t.Fatal("source drift accepted")
	}
	conflict := append(bytes.Clone(xmlData), `  <entry owner="SYNTHETIC A" group="0019" element="0010" vr="SS" vm="1" name="Override"/>`+"\n"...)
	hash := sha256.Sum256(conflict)
	for i := range s.Records {
		s.Records[i].Origin.SourceSHA256 = hex.EncodeToString(hash[:])
	}
	if err := verifyGDCM(s, conflict); err == nil {
		t.Fatal("competing upstream definition accepted")
	}
}

func TestPrivateCatalogRejectsUnreviewedGDCMDefinitions(t *testing.T) {
	gdcm := readSource(t, "../../dictionary/curated/catalog_gdcm.json")
	if _, err := validatedRecords(gdcm); err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*sourceRecord){
		"keyword not derived": func(r *sourceRecord) { r.Keyword += "X" },
		"name differs":        func(r *sourceRecord) { r.Name += " X"; r.Keyword += "X" },
		"uncertain name":      func(r *sourceRecord) { r.Name = "?" + r.Name },
		"VR contradicts row":  func(r *sourceRecord) { r.VR = "FD" },
		"offset contradicts":  func(r *sourceRecord) { r.Offset = "FE" },
		"unpinned version":    func(r *sourceRecord) { r.Origin.Version = "3.2.11" },
		"wrong source file": func(r *sourceRecord) {
			r.Origin.SourceURL = strings.Replace(r.Origin.SourceURL, "privatedicts.xml", "HarvestedPrivate.xml", 1)
		},
		"wrong license URL": func(r *sourceRecord) {
			r.Origin.LicenseURL = strings.Replace(r.Origin.LicenseURL, "Copyright.txt", "LICENSE", 1)
		},
		"unknown project":      func(r *sourceRecord) { r.Origin.Project = "gdcm" },
		"row is not an entry":  func(r *sourceRecord) { r.Origin.SourceRow = "<dict>" },
		"missing restrictions": func(r *sourceRecord) { r.Origin.Restrictions = "" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			s := readSource(t, "../../dictionary/curated/catalog_gdcm.json")
			mutate(&s.Records[0])
			if _, err := validatedRecords(s); !errors.Is(err, errSource) {
				t.Fatal("invalid GDCM record accepted")
			}
		})
	}
	merged := sourceFixture(t)
	merged.Records = append(merged.Records, gdcm.Records[0])
	if _, err := validatedRecords(merged); !errors.Is(err, errSource) {
		t.Fatal("a key defined by two sources was accepted")
	}
	if code := run([]string{"-import-gdcm", "privatedicts.xml", "-check"}, &bytes.Buffer{}); code != 2 {
		t.Fatal("-import-gdcm with -check must be a usage error")
	}
}

// gitShow reads a file of a pinned commit from a reference checkout.
func gitShow(t *testing.T, dir, commit, file string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "show", commit+":"+file).Output()
	if err != nil {
		t.Fatalf("git show %s:%s in %s: %v", commit[:8], file, dir, err)
	}
	return out
}

// TestPrivateCatalogMatchesPinnedUpstreams re-imports the GDCM source and
// verifies every record against the pinned upstream files when
// DICOMGO_REFERENCE_ROOT names the reference checkouts (dcmtk/, GDCM/).
func TestPrivateCatalogMatchesPinnedUpstreams(t *testing.T) {
	root := os.Getenv("DICOMGO_REFERENCE_ROOT")
	if root == "" {
		t.Skip("DICOMGO_REFERENCE_ROOT is not set")
	}
	xmlData := gitShow(t, filepath.Join(root, "GDCM"), gdcmCommit, gdcmSourceFile)
	selection, err := os.ReadFile("../../dictionary/curated/gdcm_selection.tsv")
	if err != nil {
		t.Fatal(err)
	}
	imported, err := importGDCMSource(xmlData, selection)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := os.ReadFile("../../dictionary/curated/catalog_gdcm.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(imported, committed) {
		t.Fatal("catalog_gdcm.json differs from a fresh import of the pinned privatedicts.xml")
	}
	s := sourceFixture(t)
	if err := verifyGDCM(s, xmlData); err != nil {
		t.Fatal(err)
	}
	dcmtk := s.Records[0].Origin.Commit
	if err := verifyUpstream(s, gitShow(t, filepath.Join(root, "dcmtk"), dcmtk, "dcmdata/data/private.dic")); err != nil {
		t.Fatal(err)
	}
}
