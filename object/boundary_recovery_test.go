package object_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func leItem(body []byte, overrun uint32) []byte {
	return append(le(uint16(0xFFFE), uint16(0xE000), uint32(len(body))+overrun), body...)
}

// overrunFile is a Part 10 file whose last element is a directory-record-like
// sequence with an overrunning last item, so the anomaly is found at the very
// end of the read.
func overrunFile(t *testing.T) []byte {
	t.Helper()
	s := &unStream{order: binary.LittleEndian}
	s.text(0x0008, 0x0016, "UI", "1.2.840.10008.5.1.4.1.1.7")
	s.text(0x0008, 0x0018, "UI", "2.25.943")
	record := func(kind string) []byte {
		inner := &unStream{order: binary.LittleEndian}
		inner.text(0x0004, 0x1430, "CS", kind)
		return inner.buf.Bytes()
	}
	items := append(leItem(record("SERIES"), 0), leItem(record("IMAGE"), 40)...)
	s.header(0x0004, 0x1220, "SQ", uint32(len(items)))
	s.buf.Write(items)
	meta, err := dicomtest.Part10File(transfer.ExplicitVRLittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	return append(meta, s.buf.Bytes()...)
}

func TestReadFileReportsContainerRecoveries(t *testing.T) {
	data := overrunFile(t)
	if _, err := object.ReadFile(bytes.NewReader(data)); !errors.Is(err, parser.ErrContainerOverrun) {
		t.Fatalf("strict read: %v", err)
	}
	file, err := object.ReadFileWithOptions(bytes.NewReader(data), object.ReadFileOptions{RecoverContainerOverruns: true})
	if err != nil {
		t.Fatal(err)
	}
	records, _ := file.Dataset.GetSequence(core.NewTag(0x0004, 0x1220))
	if len(records) != 2 {
		t.Fatalf("%d records", len(records))
	}
	if kind, _ := records[1].GetString(core.NewTag(0x0004, 0x1430)); kind != "IMAGE" {
		t.Fatalf("last record %q", kind)
	}
	recoveries, truncated := file.Dataset.BoundaryRecoveries()
	if truncated || len(recoveries) != 1 || recoveries[0].Path != "(0004,1220)[1]" || recoveries[0].Reason != parser.BoundaryItemOverrun {
		t.Fatalf("recoveries %+v", recoveries)
	}
	// A clean file reports none, so callers can tell a recovery apart.
	minimal, err := dicomtest.MinimalPart10File(transfer.ExplicitVRLittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	clean, err := object.ReadFileWithOptions(bytes.NewReader(minimal), object.ReadFileOptions{RecoverContainerOverruns: true})
	if err != nil {
		t.Fatal(err)
	}
	if list, _ := clean.Dataset.BoundaryRecoveries(); len(list) != 0 {
		t.Fatalf("clean file recoveries %+v", list)
	}
	// Data set reads (no Part 10 header) propagate them too.
	dataset := data[bytes.Index(data, le(uint16(0x0008), uint16(0x0016))):]
	obj, err := object.ReadDataSetWithOptions(bytes.NewReader(dataset), transfer.ExplicitVRLittleEndian, object.ReadFileOptions{RecoverContainerOverruns: true})
	if err != nil {
		t.Fatal(err)
	}
	if list, _ := obj.BoundaryRecoveries(); len(list) != 1 {
		t.Fatalf("data set recoveries %+v", list)
	}
}

// TestPydicomDICOMDIRNoOffsetOracle reads pydicom's DICOMDIR-nooffset, whose
// last record overruns the Directory Record Sequence, and compares the
// recovered records with pydicom 3.0.2 and with the intact DICOMDIR. Enable
// with DICOM_GO_PYDICOM_DICOMDIR=1 and DICOM_GO_PYTHON; the files come from
// the pydicom installation. Without the gate the test skips.
func TestPydicomDICOMDIRNoOffsetOracle(t *testing.T) {
	if os.Getenv("DICOM_GO_PYDICOM_DICOMDIR") != "1" {
		t.Skip("set DICOM_GO_PYDICOM_DICOMDIR=1 and DICOM_GO_PYTHON to run the pydicom DICOMDIR oracle")
	}
	python := strings.TrimSpace(os.Getenv("DICOM_GO_PYTHON"))
	if python == "" {
		t.Fatal("DICOM_GO_PYTHON is required when the pydicom DICOMDIR oracle is enabled")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	script := filepath.Join(t.TempDir(), "dicomdir_oracle.py")
	if err := os.WriteFile(script, []byte(dicomdirPydicomScript), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(ctx, python, script).Output()
	if err != nil {
		t.Fatalf("pydicom: %v", err)
	}
	var oracle struct {
		Dir   string              `json:"dir"`
		Types map[string][]string `json:"types"`
	}
	if err := json.Unmarshal(out, &oracle); err != nil {
		t.Fatalf("pydicom output: %v\n%s", err, out)
	}
	read := func(name string, recover bool) ([]string, []object.BoundaryRecovery, error) {
		file, err := object.OpenFileWithOptions(filepath.Join(oracle.Dir, name), object.ReadFileOptions{RecoverContainerOverruns: recover})
		if err != nil {
			return nil, nil, err
		}
		defer file.Close()
		records, _ := file.Dataset.GetSequence(core.NewTag(0x0004, 0x1220))
		var types []string
		for _, record := range records {
			kind, _ := record.GetString(core.NewTag(0x0004, 0x1430))
			types = append(types, kind)
		}
		recoveries, _ := file.Dataset.BoundaryRecoveries()
		return types, recoveries, nil
	}
	if _, _, err := read("DICOMDIR-nooffset", false); !errors.Is(err, parser.ErrContainerOverrun) {
		t.Fatalf("strict read of DICOMDIR-nooffset: %v", err)
	}
	intact, intactRecoveries, err := read("DICOMDIR", true)
	if err != nil || len(intactRecoveries) != 0 {
		t.Fatalf("DICOMDIR: %v %+v", err, intactRecoveries)
	}
	recovered, recoveries, err := read("DICOMDIR-nooffset", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(recoveries) != 1 || recoveries[0].Reason != parser.BoundaryItemOverrun {
		t.Fatalf("recoveries %+v", recoveries)
	}
	for name, got := range map[string][]string{"DICOMDIR": intact, "DICOMDIR-nooffset": recovered} {
		if strings.Join(got, ",") != strings.Join(oracle.Types[name], ",") {
			t.Fatalf("%s: dicom-go %d records, pydicom %d", name, len(got), len(oracle.Types[name]))
		}
	}
	if strings.Join(recovered, ",") != strings.Join(intact, ",") {
		t.Fatal("DICOMDIR-nooffset records differ from DICOMDIR")
	}
}

const dicomdirPydicomScript = `import json, os
import pydicom
from pydicom.data import get_testdata_file
out = {"types": {}}
for name in ("DICOMDIR", "DICOMDIR-nooffset"):
    path = get_testdata_file(name)
    out["dir"] = os.path.dirname(path)
    ds = pydicom.dcmread(path)
    out["types"][name] = [str(r.DirectoryRecordType) for r in ds.DirectoryRecordSequence]
print(json.dumps(out))
`
