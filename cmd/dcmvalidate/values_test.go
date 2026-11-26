package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/validation/iod"
)

func TestValuesOptInCLIPrivacySourceAndLegacy(t *testing.T) {
	file := ctImage(t)
	const sensitive = "PRIVATE_SEX"
	file.Dataset.Put(dicomtest.NewStringElement(core.NewTag(0x0010, 0x0040), core.VRCS, sensitive))
	path := writeFile(t, "value.dcm", file)
	if code, out, errOut := runTool("-iod-only", path); code != 0 || strings.Contains(out, string(iod.InvalidEnumeratedValue)) {
		t.Fatalf("default changed: %d %s %s", code, out, errOut)
	}
	code, out, errOut := runTool("-values", "-iod-only", path)
	if code != 1 || !strings.Contains(out, "invalid-enumerated-value (0010,0040)") || !strings.Contains(out, "rule=table_C.7-1/") || !strings.Contains(out, "source=PS3.3 2026d table_C.7-1") {
		t.Fatalf("text: %d %s %s", code, out, errOut)
	}
	if strings.Contains(out, sensitive) || strings.Contains(errOut, sensitive) {
		t.Fatal("value disclosed")
	}
	code, out, errOut = runTool("-values", "-json", "-iod-only", path)
	var report fileReport
	if code != 1 || json.Unmarshal([]byte(out), &report) != nil {
		t.Fatalf("json: %d %s %s", code, out, errOut)
	}
	found := false
	for _, f := range report.Findings {
		if f.Code == string(iod.InvalidEnumeratedValue) {
			found = true
			if f.Rule == "" || f.Source == nil || f.Source.TableID != "table_C.7-1" || len(f.Source.SHA256) != 64 || f.Source.Tag != core.NewTag(0x0010, 0x0040) {
				t.Fatalf("source missing: %+v", f)
			}
		}
	}
	if !found || strings.Contains(out, sensitive) {
		t.Fatal("missing finding or value leaked")
	}
	file.Dataset.Put(dicomtest.NewStringElement(core.NewTag(0x0010, 0x0040), core.VRCS, ""))
	if code, out, _ := runTool("-values", writeFile(t, "empty.dcm", file)); code != 0 || strings.Contains(out, "invalid-enumerated-value") {
		t.Fatalf("Type2 empty: %d %s", code, out)
	}
}

func TestValuesAdditionalTermsStayWarningsInCLI(t *testing.T) {
	file := ctImage(t)
	// Context override is an explicit CLI contract. Supply the only SC-specific
	// mandatory field while retaining this synthetic, non-PHI image envelope.
	file.Dataset.Put(dicomtest.NewStringElement(core.NewTag(0x0008, 0x0064), core.VRCS, "WSD"))
	const term = "VENDOR_PI"
	file.Dataset.Put(dicomtest.NewStringElement(core.NewTag(0x0028, 0x0004), core.VRCS, term))
	path := writeFile(t, "defined.dcm", file)
	args := []string{"-values", "-iod-only", "-iod", "Secondary Capture Image", path}
	code, out, errOut := runTool(args...)
	if code != 0 || !strings.Contains(out, "warning additional-defined-term") || strings.Contains(out, "invalid-enumerated-value") || strings.Contains(out, term) {
		t.Fatalf("additional term: %d %s %s", code, out, errOut)
	}
	if code, _, _ := runTool(append([]string{"-strict"}, args...)...); code != 1 {
		t.Fatalf("strict warning exit %d", code)
	}
	if code, out, _ := runTool("-values", "-info", "-iod-only", path); code != 1 || !strings.Contains(out, "manual-value-constraint") {
		t.Fatalf("manual coverage info: %d %s", code, out)
	}
}
