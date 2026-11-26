package dcmdump

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestDumpRecoveryFilteredDiagnosticsAndCaps(t *testing.T) {
	body := append(dsControl(binary.LittleEndian, 0xe00d, 0), dsShort(transfer.ExplicitVRLittleEndian, 0x0010, 0x0020, "LO", []byte("NO^PHI  "))...)
	input := dsPart10(transfer.ExplicitVRLittleEndian, body)
	path := dsPath(t, input)
	code, out, _ := dsRun(t, path)
	if code != 1 || out != "" {
		t.Fatal("default recovered malformed dataset")
	}
	code, out, stderr := dsRun(t, "-read-recover-delimiter-anomalies", "-search-tag", "PatientID", "-show-offsets", path)
	if code != 0 || !strings.Contains(stderr, "orphan-delimiter-skipped") || strings.Contains(stderr, "NO^PHI") || !strings.Contains(out, "PatientID") {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	// The offset is the original header position after the skipped delimiter.
	wantOffset := uint32(len(dicomtest.NewFileMetaBuilder().Encode()) + 132 + 8)
	if !strings.Contains(out, fmtOffset(wantOffset)) {
		t.Fatal("offset points at skipped delimiter")
	}
	more := dsPart10(transfer.ExplicitVRLittleEndian, append(dsControl(binary.LittleEndian, 0xe00d, 0), body...))
	dir := t.TempDir()
	code, out, _ = dsRun(t, "-read-recover-delimiter-anomalies", "-read-max-boundary-recoveries", "1", "-write-values", dir, "-search-tag", "PatientID", dsPath(t, more))
	entries, _ := os.ReadDir(dir)
	if code != 1 || out != "" || len(entries) != 0 {
		t.Fatal("truncated report published values")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(after, input) {
		t.Fatal("inspection changed source")
	}
}
func fmtOffset(n uint32) string {
	const digits = "0123456789ABCDEF"
	b := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		b[i] = digits[n&15]
		n >>= 4
	}
	return string(b)
}

func TestDumpOddAndReservedCLIContract(t *testing.T) {
	odd := dsPath(t, dsPart10(transfer.ExplicitVRLittleEndian, dsShort(transfer.ExplicitVRLittleEndian, 0x0010, 0x0020, "LO", []byte("ABC"))))
	if code, _, _ := dsRun(t, odd); code != 1 {
		t.Fatal("odd lengths accepted by default")
	}
	if code, _, stderr := dsRun(t, "-read-odd-length", "accept", odd); code != 0 || !strings.Contains(stderr, "odd-length-accepted") {
		t.Fatalf("odd recovery: %d %s", code, stderr)
	}
	reserved := dsLong(transfer.ExplicitVRLittleEndian, 0x7777, 0x1001, "OB", 2, []byte{1, 2})
	reserved[6] = 1
	path := dsPath(t, dsPart10(transfer.ExplicitVRLittleEndian, reserved))
	if code, _, _ := dsRun(t, path); code != 0 {
		t.Fatal("default reserved-byte policy tightened")
	}
	if code, _, _ := dsRun(t, "-read-strict-reserved-bytes", path); code != 1 {
		t.Fatal("reserved-byte strict opt-in ignored")
	}
}

func TestDumpMetaRecoveryOffsets(t *testing.T) {
	meta := dicomtest.NewFileMetaBuilder().Build()
	rest := dicomtest.EncodeElements(transfer.ImplicitVRLittleEndian, meta[1:]...)
	gl := make([]byte, 4)
	binary.LittleEndian.PutUint32(gl, uint32(len(rest)))
	implicitMeta := append(dsControl(binary.LittleEndian, 0, 4), gl...)
	binary.LittleEndian.PutUint16(implicitMeta, 2)
	input := append(append(make([]byte, 128), "DICM"...), append(append(implicitMeta, rest...), dsShort(transfer.ExplicitVRLittleEndian, 0x0010, 0x0020, "LO", []byte("SAFE"))...)...)
	path := dsPath(t, input)
	code, out, stderr := dsRun(t, "-read-allow-implicit-file-meta", "-show-offsets", path)
	if code != 0 || !strings.Contains(out, "00000084") || !strings.Contains(stderr, "implicit-vr-file-meta") {
		t.Fatalf("meta offsets: %d %s", code, stderr)
	}
	missing := dsPath(t, append(append(make([]byte, 128), "DICM"...), append(dicomtest.NewFileMetaBuilder().Encode()[12:], dsShort(transfer.ExplicitVRLittleEndian, 0x0010, 0x0020, "LO", []byte("SAFE"))...)...))
	code, _, stderr = dsRun(t, "-read-allow-missing-meta-group-length", "-show-offsets", missing)
	if code != 0 || !strings.Contains(stderr, "missing-meta-group-length-accepted") {
		t.Fatalf("missing GL offsets: %d %s", code, stderr)
	}
}

func TestDumpMetaRepairReportedAndJointCapBeforeExport(t *testing.T) {
	body := dsShort(transfer.ExplicitVRLittleEndian, 0x0010, 0x0010, "LO", []byte("SAFE"))
	data := dsPart10(transfer.ExplicitVRLittleEndian, body)
	at := bytes.Index(data[132:], []byte{2, 0, 1, 0, 'O', 'B'}) + 132
	if at < 132 {
		t.Fatal("fixture lacks Meta version")
	}
	copy(data[at+4:at+6], "UN")
	path := dsPath(t, data)
	if code, _, diagnostics := dsRun(t, "-read-prefer-dictionary-vr", path); code != 0 || !strings.Contains(diagnostics, "scope=meta path=(0002,0001) reason=dictionary-vr-preferred") {
		t.Fatalf("code=%d diagnostics=%s", code, diagnostics)
	}
	dir := t.TempDir()
	code, out, _ := dsRun(t, "-read-prefer-dictionary-vr", "-read-max-boundary-recoveries", "1", "-search-tag", "PatientName", "-write-values", dir, path)
	entries, err := os.ReadDir(dir)
	if code != 1 || out != "" || err != nil || len(entries) != 0 {
		t.Fatal("joint Meta/dataset report cap allowed publication")
	}
}

func TestDumpMetaReplayNearFileTotalByteLimit(t *testing.T) {
	data := dsPart10(transfer.ExplicitVRLittleEndian, dsShort(transfer.ExplicitVRLittleEndian, 0x0010, 0x0020, "LO", []byte("SAFE")))
	at := bytes.Index(data[132:], []byte{2, 0, 1, 0, 'O', 'B'}) + 132
	if at < 132 {
		t.Fatal("fixture lacks Meta version")
	}
	copy(data[at+4:at+6], "UN")
	path := dsPath(t, data)
	code, out, diagnostics := dsRun(t, "-read-prefer-dictionary-vr", "-read-max-total-bytes", strconv.Itoa(len(data)+8), "-search-tag", "PatientID", path)
	if code != 0 || !strings.Contains(out, "PatientID") || !strings.Contains(diagnostics, "scope=meta path=(0002,0001) reason=dictionary-vr-preferred") {
		t.Fatalf("accepted short input was refused at total-byte boundary: code=%d diagnostics=%s", code, diagnostics)
	}
}

func TestDumpReplayRejectsReplacedSource(t *testing.T) {
	path := dsPath(t, dsNested())
	opts, _, err := parseArgs([]string{"-search-tag", "PatientName", "-write-values", t.TempDir(), path}, bytes.NewBuffer(nil))
	if err != nil {
		t.Fatal(err)
	}
	opts.sourceInfo, err = dumpSourceInfo(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := object.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	replacement := filepath.Join(filepath.Dir(path), "replacement")
	if err := os.WriteFile(replacement, dsNested(), 0600); err != nil {
		t.Fatal(err)
	}
	// Identity must decide this case, not a lucky mtime difference.
	if err := os.Chtimes(replacement, opts.sourceInfo.ModTime(), opts.sourceInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if err := runSelectedDump(context.Background(), bytes.NewBuffer(nil), path, path, file, opts, false); err == nil {
		t.Fatal("replaced input source replayed")
	}
}

func TestDumpOpenRejectsReplacedIdentityAtEqualSizeAndTime(t *testing.T) {
	path := dsPath(t, dsNested())
	opts, _, err := parseArgs([]string{"-show-offsets", path}, bytes.NewBuffer(nil))
	if err != nil {
		t.Fatal(err)
	}
	opts.sourceInfo, err = dumpSourceInfo(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(filepath.Dir(path), "same-size-time-replacement")
	if err := os.WriteFile(replacement, dsNested(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, opts.sourceInfo.ModTime(), opts.sourceInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	file, _, _, _, _, cleanup, err := openFileForDumpInput(context.Background(), path, opts)
	defer cleanup()
	if err == nil || file != nil {
		t.Fatal("input replaced between snapshot and opening was accepted", err)
	}
}
