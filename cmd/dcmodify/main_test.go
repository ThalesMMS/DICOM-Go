package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func writeInput(t *testing.T) string {
	t.Helper()
	data, err := dicomtest.Part10File(transfer.ExplicitVRLittleEndian,
		dicomtest.StringElement(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		dicomtest.StringElement(core.NewTag(0x0008, 0x0018), core.VRUI, "1.2.826.0.1.3680043.10.1.982.1"),
		dicomtest.StringElement(core.NewTag(0x0010, 0x0010), core.VRPN, "DOE^JOHN"),
		dicomtest.StringElement(core.NewTag(0x0010, 0x1000), core.VRLO, "OLDID"),
	)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "in.dcm")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDcmodifyInPlacePreservesPermissions(t *testing.T) {
	input := writeInput(t)
	if err := os.Chmod(input, 0o640); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(input)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-i", "PatientID=7", "-in-place", input}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, &stderr)
	}
	after, err := os.Stat(input)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode().Perm() != before.Mode().Perm() {
		t.Fatalf("permissions = %v, want %v", after.Mode().Perm(), before.Mode().Perm())
	}
}

func TestDcmodifyEditsInCommandLineOrder(t *testing.T) {
	input := writeInput(t)
	output := filepath.Join(filepath.Dir(input), "out.dcm")
	script := filepath.Join(filepath.Dir(input), "edits.txt")
	if err := os.WriteFile(script, []byte("insert ClinicalTrialSubjectID=S1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"-i", "ReferencedSeriesSequence[0].SeriesInstanceUID=1.2.3", "-m", "PatientName=ANON", "-e", "(0010,1000)", "-i", "PatientName=LAST", "-script", script, "-o", output, input}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	file, err := object.OpenFile(output)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if name, _ := file.Dataset.GetString(core.NewTag(0x0010, 0x0010)); name != "LAST" {
		t.Fatalf("PatientName %q: edits did not keep their order", name)
	}
	items, ok := file.Dataset.GetSequence(core.NewTag(0x0008, 0x1115))
	if !ok || len(items) != 1 || file.Dataset.Has(core.NewTag(0x0010, 0x1000)) || !file.Dataset.Has(core.NewTag(0x0012, 0x0040)) {
		t.Fatal("edits missing from the output")
	}
	if original, _ := object.OpenFile(input); original != nil {
		defer original.Close()
		if name, _ := original.Dataset.GetString(core.NewTag(0x0010, 0x0010)); name != "DOE^JOHN" {
			t.Fatal("input changed without -in-place")
		}
	}
}

func TestDcmodifyUsageAndFailures(t *testing.T) {
	input := writeInput(t)
	for _, args := range [][]string{
		{"-i", "PatientName=X", input},
		{"-i", "PatientName=X", "-o", "a.dcm", "-in-place", input},
		{"-o", "a.dcm", input},
		{"-i", "Nope=1", "-o", filepath.Join(t.TempDir(), "a.dcm"), input},
	} {
		var stderr bytes.Buffer
		if code := run(args, &bytes.Buffer{}, &stderr); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	var stderr bytes.Buffer
	if code := run([]string{"-m", "PatientID=1", "-in-place", input}, &bytes.Buffer{}, &stderr); code != 1 || !strings.Contains(stderr.String(), "not found") {
		t.Fatalf("missing attribute: exit %d %s", code, stderr.String())
	}
	if code := run([]string{"-e-if-present", "PatientID", "-i", "PatientID=7", "-in-place", input}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("in-place exit %d", code)
	}
	file, _ := object.OpenFile(input)
	defer file.Close()
	if id, _ := file.Dataset.GetString(core.NewTag(0x0010, 0x0020)); id != "7" {
		t.Fatalf("in-place PatientID %q", id)
	}
}

func TestDcmodifyRecordsOriginalAttributes(t *testing.T) {
	input := writeInput(t)
	output := filepath.Join(filepath.Dir(input), "out.dcm")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-m", "PatientName=ANON", "-original-attributes", "coerce", "-modifying-system", "SITE GATEWAY", "-o", output, input}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	file, err := object.OpenFile(output)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	records, ok := file.Dataset.GetSequence(core.NewTag(0x0400, 0x0561))
	if !ok || len(records) != 1 {
		t.Fatal("no Original Attributes item")
	}
	if reason, _ := records[0].GetString(core.NewTag(0x0400, 0x0565)); reason != "COERCE" {
		t.Fatalf("reason %q", reason)
	}
	if system, _ := records[0].GetString(core.NewTag(0x0400, 0x0563)); system != "SITE GATEWAY" {
		t.Fatalf("system %q", system)
	}
	previous, _ := records[0].GetSequence(core.NewTag(0x0400, 0x0550))
	if len(previous) != 1 {
		t.Fatal("no Modified Attributes item")
	}
	if name, _ := previous[0].GetString(core.NewTag(0x0010, 0x0010)); name != "DOE^JOHN" {
		t.Fatalf("previous Patient Name %q", name)
	}
	if previous[0].Has(core.NewTag(0x0010, 0x1000)) {
		t.Fatal("an unchanged attribute was recorded")
	}
	if code := run([]string{"-m", "PatientName=X", "-original-attributes", "edit", "-o", output, input}, &stdout, &stderr); code != 2 {
		t.Fatalf("unknown reason: exit %d, want 2", code)
	}
}
