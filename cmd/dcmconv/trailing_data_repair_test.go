package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConvTrailingDataRepairIsExplicitAndCreateOnly(t *testing.T) {
	input, _ := writeImage(t)
	prefix, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	wire := append(bytes.Clone(prefix), []byte{0xfc, 0xff, 0xfc, 0xff, 'X', 'Z'}...)
	if err := os.WriteFile(input, wire, 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "repair.dcm")
	if code, _, stderr := runConv(t, "-ts", "explicit-le", "-o", output, input); code != 1 {
		t.Fatalf("strict conversion: code=%d stderr=%s", code, stderr)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("strict conversion published output")
	}
	if code, _, stderr := runConv(t, "-repair-trailing-data", "-ts", "explicit-le", "-o", output, input); code != 2 {
		t.Fatalf("conflicting flags: code=%d stderr=%s", code, stderr)
	}
	if code, stdout, stderr := runConv(t, "-repair-trailing-data", "-o", output, input); code != 0 || !strings.Contains(stdout, "removed 6 trailing bytes") || stderr != "" {
		t.Fatalf("repair: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if got, err := os.ReadFile(output); err != nil || !bytes.Equal(got, prefix) {
		t.Fatalf("incorrect output: %v", err)
	}
	if code, _, _ := runConv(t, "-repair-trailing-data", "-o", output, input); code != 1 {
		t.Fatalf("existing output: code=%d", code)
	}
	if got, err := os.ReadFile(input); err != nil || !bytes.Equal(got, wire) {
		t.Fatalf("source changed: %v", err)
	}
}
