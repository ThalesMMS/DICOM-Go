package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
)

func mdBroken(t *testing.T, n int) (string, []byte) {
	t.Helper()
	original := writeInput(t)
	data, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	header, err := object.ReadPart10Header(bytes.NewReader(data), object.ReadFileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	body := data[header.DataSetOffset:]
	for i := 0; i < n; i++ {
		body = append(dicomtest.SequenceControlBytes(binary.LittleEndian, core.NewTag(0xfffe, 0xe00d), 0), body...)
	}
	broken := append(append([]byte(nil), data[:header.DataSetOffset]...), body...)
	if err := os.WriteFile(original, broken, 0600); err != nil {
		t.Fatal(err)
	}
	return original, broken
}
func TestModifySharedReadRecoveryBeforeEdits(t *testing.T) {
	input, before := mdBroken(t, 1)
	output := filepath.Join(t.TempDir(), "out.dcm")
	var out, stderr bytes.Buffer
	args := []string{"-m", "PatientName=REDACTED", "-o", output, input}
	if code := run(args, &out, &stderr); code != 1 {
		t.Fatal("default parser repaired input")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("strict failure wrote output")
	}
	out.Reset()
	stderr.Reset()
	if code := run(append([]string{"-read-recover-delimiter-anomalies"}, args...), &out, &stderr); code != 0 || !strings.Contains(stderr.String(), "orphan-delimiter-skipped") {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "DOE^JOHN") || strings.Contains(stderr.String(), "REDACTED") {
		t.Fatal("value leaked")
	}
	file, err := object.OpenFile(output)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if got, _ := file.Dataset.GetString(core.NewTag(0x0010, 0x0010)); got != "REDACTED" {
		t.Fatal("edit not applied")
	}
	after, _ := os.ReadFile(input)
	if !bytes.Equal(after, before) {
		t.Fatal("source changed without in-place")
	}
}
func TestModifyRecoveryCapProtectsInPlaceSource(t *testing.T) {
	input, before := mdBroken(t, 2)
	var out, stderr bytes.Buffer
	if code := run([]string{"-read-recover-delimiter-anomalies", "-read-max-boundary-recoveries", "1", "-m", "PatientName=X", "-in-place", input}, &out, &stderr); code != 1 || out.Len() != 0 {
		t.Fatal("incomplete report allowed edit")
	}
	after, _ := os.ReadFile(input)
	if !bytes.Equal(after, before) {
		t.Fatal("in-place source changed on refusal")
	}
	for _, flags := range [][]string{{"-read-odd-length", "pad"}, {"-read-max-elements", "0"}, {"-read-prefer-dictionary-length-field-size"}} {
		if code := run(append(append(flags, "-i", "PatientID=X", "-in-place"), input), &out, &stderr); code != 2 {
			t.Fatalf("invalid read domain %v", flags)
		}
	}
}

func TestModifyMetaRepairReportedBeforePublication(t *testing.T) {
	input := writeInput(t)
	data, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	at := bytes.Index(data[132:], []byte{2, 0, 1, 0, 'O', 'B'}) + 132
	if at < 132 {
		t.Fatal("fixture lacks Meta version")
	}
	copy(data[at+4:at+6], "UN")
	if err := os.WriteFile(input, data, 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "out.dcm")
	var out, diagnostics bytes.Buffer
	if code := run([]string{"-read-prefer-dictionary-vr", "-m", "PatientName=SAFE", "-o", output, input}, &out, &diagnostics); code != 0 {
		t.Fatalf("code=%d diagnostics=%s", code, diagnostics.String())
	}
	if !strings.Contains(diagnostics.String(), "scope=meta path=(0002,0001) reason=dictionary-vr-preferred") {
		t.Fatal("Meta repair was not reported")
	}
	patient := bytes.Index(data, []byte{0x10, 0, 0x10, 0, 'P', 'N'})
	if patient < 0 {
		t.Fatal("fixture lacks PatientName")
	}
	copy(data[patient+4:patient+6], "LO")
	if err := os.WriteFile(input, data, 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	diagnostics.Reset()
	if code := run([]string{"-read-prefer-dictionary-vr", "-read-max-boundary-recoveries", "1", "-m", "PatientName=X", "-in-place", input}, &out, &diagnostics); code != 1 || out.Len() != 0 {
		t.Fatal("joint Meta/dataset cap allowed in-place edit")
	}
	after, _ := os.ReadFile(input)
	if !bytes.Equal(after, data) {
		t.Fatal("joint cap refusal changed source")
	}
}
