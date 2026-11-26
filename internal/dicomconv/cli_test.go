package dicomconv

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunConvertsBothWaysWithBulkData(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "in.dcm")
	file := richFile(t)
	if err := WritePart10(source, file); err != nil {
		t.Fatal(err)
	}
	want := datasetBytes(t, file)
	for _, pair := range []struct {
		encode, decode Tool
		ext            string
	}{{DCM2JSON, JSON2DCM, "json"}, {DCM2XML, XML2DCM, "xml"}} {
		out := filepath.Join(dir, pair.ext)
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		document := filepath.Join(out, "doc."+pair.ext)
		var stdout, stderr bytes.Buffer
		args := []string{"-meta", "-bulk-dir", filepath.Join(out, "bulk"), "-bulk-threshold", "8", source, document}
		if code := Run(pair.encode, args, &stdout, &stderr); code != 0 {
			t.Fatalf("%s: exit %d: %s", pair.encode, code, stderr.String())
		}
		if entries, _ := os.ReadDir(filepath.Join(out, "bulk")); len(entries) == 0 {
			t.Fatalf("%s wrote no bulk data", pair.encode)
		}
		back := filepath.Join(out, "back.dcm")
		if code := Run(pair.decode, []string{"-charset", "ISO_IR 100", document, back}, &stdout, &stderr); code != 0 {
			t.Fatalf("%s: exit %d: %s", pair.decode, code, stderr.String())
		}
		written, err := ReadPart10(back)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(datasetBytes(t, written), want) {
			t.Fatalf("%s -> %s changed the data set", pair.encode, pair.decode)
		}
		_ = written.Close()
	}

	// Standard output, and usage errors.
	var stdout, stderr bytes.Buffer
	if code := Run(DCM2JSON, []string{"-compact", source}, &stdout, &stderr); code != 0 || !strings.HasPrefix(stdout.String(), "{") {
		t.Fatalf("stdout conversion: %d %q", code, stderr.String())
	}
	for _, args := range [][]string{{}, {"a.json"}, {"-unrepresentable", "maybe", "a.json", "b.dcm"}} {
		if code := Run(JSON2DCM, args, &stdout, &stderr); code != 2 {
			t.Fatalf("%v: exit %d, want 2", args, code)
		}
	}
	if code := Run(DCM2JSON, []string{"-bulk-dir", dir, source}, &stdout, &stderr); code != 1 {
		t.Fatalf("bulk data without an output file: exit %d", code)
	}
}
