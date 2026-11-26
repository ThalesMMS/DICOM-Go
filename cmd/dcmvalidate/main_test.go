package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/imagebuild"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
)

// ctImage is a synthetic CT image without patient data that has no IOD
// error.
func ctImage(t *testing.T) *object.File {
	t.Helper()
	geometry := imagebuild.Geometry{Rows: 2, Columns: 2, PixelSpacing: [2]float64{1, 1},
		RowDirection: [3]float64{1, 0, 0}, ColumnDirection: [3]float64{0, 1, 0}, SliceThickness: 1}
	files, err := imagebuild.Build(geometry, []imagebuild.Slice{{Values: []float64{0, 1, 2, 3}}}, imagebuild.Options{
		IOD: imagebuild.CT, RescaleType: "HU", ContentTime: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	return files[0]
}

func writeFile(t *testing.T, name string, file *object.File) string {
	t.Helper()
	var buf bytes.Buffer
	if err := object.WriteFile(&buf, file); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runTool(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestValidate(t *testing.T) {
	valid := writeFile(t, "valid.dcm", ctImage(t))
	if code, stdout, stderr := runTool(valid); code != 0 || !strings.Contains(stdout, ": CT Image: 0 error(s)") {
		t.Fatalf("valid image: code %d stdout %q stderr %q", code, stdout, stderr)
	}

	// A missing Type 1 attribute fails the IOD check; a malformed date
	// fails the encoding check without its value being printed.
	broken := ctImage(t)
	broken.Dataset.Remove(core.NewTag(0x0020, 0x000D))
	broken.Dataset.Put(dicomtest.NewStringElement(core.NewTag(0x0008, 0x0020), core.VRDA, "SECRET-DATE"))
	path := writeFile(t, "broken.dcm", broken)
	code, stdout, _ := runTool(path)
	if code != 1 || !strings.Contains(stdout, "error missing-type-1 (0020,000D) (Type 1, General Study)") ||
		!strings.Contains(stdout, "encoding") && !strings.Contains(stdout, "vr.") {
		t.Fatalf("broken image: code %d stdout:\n%s", code, stdout)
	}
	if strings.Contains(stdout, "SECRET") || strings.Contains(stdout, "info ") {
		t.Fatalf("a value was printed:\n%s", stdout)
	}
	if !strings.Contains(stdout, "(0008,0020)") {
		t.Fatalf("the malformed date was not reported:\n%s", stdout)
	}

	code, stdout, _ = runTool("-json", "-iod-only", path, valid)
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if code != 1 || len(lines) != 2 {
		t.Fatalf("-json: code %d stdout %q", code, stdout)
	}
	var report fileReport
	if err := json.Unmarshal([]byte(lines[0]), &report); err != nil {
		t.Fatal(err)
	}
	if report.IOD != "CT Image" || report.Errors == 0 || report.StandardVersion == "" {
		t.Fatalf("report %+v", report)
	}
	for _, f := range report.Findings {
		if f.Check != "iod" {
			t.Fatalf("-iod-only reported %+v", f)
		}
	}

	// -info adds the conditions left for manual review.
	if _, stdout, _ := runTool("-info", path); !strings.Contains(stdout, "info manual-condition") {
		t.Fatalf("-info:\n%s", stdout)
	}

	// -iod checks against another IOD.
	if code, stdout, _ := runTool("-iod", "MR Image", valid); code != 1 || !strings.Contains(stdout, "MR Image") {
		t.Fatalf("-iod: code %d %q", code, stdout)
	}
}

func TestStrictAndErrors(t *testing.T) {
	unknown := ctImage(t)
	unknown.Dataset.Put(dicomtest.NewUIElement(core.NewTag(0x0008, 0x0016), "1.2.826.0.1.3680043.10.1.1193.99"))
	if code, _, stderr := runTool(writeFile(t, "unknown.dcm", unknown)); code != 1 || !strings.Contains(stderr, "dcmvalidate:") {
		t.Fatalf("unknown SOP Class: code %d %q", code, stderr)
	}
	if code, _, _ := runTool(filepath.Join(t.TempDir(), "missing.dcm")); code != 1 {
		t.Fatalf("missing file: code %d", code)
	}
	for _, args := range [][]string{{}, {"-max-findings", "0", "x.dcm"}} {
		if code, _, _ := runTool(args...); code != 2 {
			t.Fatalf("%v: code %d", args, code)
		}
	}
	if code, _, stderr := runTool("-h"); code != 0 || !strings.Contains(stderr, "Usage: dcmvalidate") {
		t.Fatalf("-h: %d %q", code, stderr)
	}
}
