package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func cvMalformed(t *testing.T, n int) (string, []byte) {
	t.Helper()
	body := dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian, dicomtest.DatasetWithPixelData()...)
	for i := 0; i < n; i++ {
		body = append(dicomtest.SequenceControlBytes(binary.LittleEndian, core.NewTag(0xfffe, 0xe00d), 0), body...)
	}
	meta := dicomtest.NewFileMetaBuilder().Encode()
	data := append(append(append(make([]byte, 128), "DICM"...), meta...), body...)
	path := filepath.Join(t.TempDir(), "broken.dcm")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path, data
}
func TestConvSharedReadRecoveryNormalizesBeforeTranscode(t *testing.T) {
	input, before := cvMalformed(t, 1)
	dir := t.TempDir()
	out := filepath.Join(dir, "out.dcm")
	code, _, _ := runConv(t, "-ts", "explicit-le", "-o", out, input)
	if code != 1 {
		t.Fatal("default repaired input")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("strict failure published")
	}
	code, _, stderr := runConv(t, "-read-recover-delimiter-anomalies", "-ts", "explicit-be", "-o", out, input)
	if code != 0 || !strings.Contains(stderr, "orphan-delimiter-skipped") || strings.Contains(stderr, "TEST^PATIENT") {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	f, err := object.OpenFile(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if f.TransferSyntax.UID != transfer.ExplicitVRBigEndian.UID {
		t.Fatal("target syntax ignored")
	}
	after, _ := os.ReadFile(input)
	if !bytes.Equal(before, after) {
		t.Fatal("source was repaired in place")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatal("normalization precursor leaked")
	}
}

func TestConvMetaRepairReportedBeforePublication(t *testing.T) {
	input, data := cvMalformed(t, 0)
	at := bytes.Index(data[132:], []byte{2, 0, 1, 0, 'O', 'B'}) + 132
	if at < 132 {
		t.Fatal("fixture lacks Meta version")
	}
	copy(data[at+4:at+6], "UN")
	if err := os.WriteFile(input, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, recovery := range []bool{false, true} {
		out := filepath.Join(t.TempDir(), "out.dcm")
		args := []string{"-read-prefer-dictionary-vr", "-ts", "explicit-le", "-o", out, input}
		if recovery {
			args = append([]string{"-read-recover"}, args...)
		}
		code, _, diagnostics := runConv(t, args...)
		if code != 0 || !strings.Contains(diagnostics, "scope=meta path=(0002,0001) reason=dictionary-vr-preferred") {
			t.Fatalf("recovery=%v code=%d diagnostics=%s", recovery, code, diagnostics)
		}
	}
}
func TestConvReadRecoveryLimitsAndCleanup(t *testing.T) {
	input, _ := cvMalformed(t, 2)
	dir := t.TempDir()
	out := filepath.Join(dir, "out.dcm")
	if code, _, _ := runConv(t, "-ts", "explicit-le", "-read-recover-delimiter-anomalies", "-read-max-boundary-recoveries", "1", "-o", out, input); code != 1 {
		t.Fatal("incomplete report accepted")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatal("failure left output")
	}
	for _, flags := range [][]string{{"-read-odd-length", "pad"}, {"-read-max-elements", "0"}, {"-read-prefer-dictionary-length-field-size"}} {
		code, _, _ := runConv(t, append(append([]string{"-o", out}, flags...), input)...)
		if code != 2 {
			t.Fatalf("invalid domain %v", flags)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, err := prepareReadInput(ctx, options{input: input, output: out}, io.Discard); err != context.Canceled {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w := normalizationWriter{ctx: context.Background(), w: &buf, remaining: 3}
	if _, err := w.Write([]byte("1234")); err == nil || buf.Len() != 0 {
		t.Fatal("normalization output limit ignored")
	}
}
