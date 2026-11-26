//go:build !jpeg2000_openjpeg && !codecfull

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJPEG2000DefaultProfileRefusesBeforeInput(t *testing.T) {
	for _, flags := range [][]string{
		{"-ts", "1.2.840.10008.1.2.4.90"},
		{"-ts", "1.2.840.10008.1.2.4.91", "-allow-lossy", "-j2k-rate", "10"},
	} {
		output := filepath.Join(t.TempDir(), "out.dcm")
		argv := append(flags, "-j2k-backend", "openjpeg", "-o", output, "missing-input.dcm")
		code, _, stderr := runConv(t, argv...)
		if code != 2 || !strings.Contains(stderr, "OpenJPEG profile unavailable") || strings.Contains(stderr, "no such file") {
			t.Fatalf("unavailable profile=%d %s", code, stderr)
		}
		if _, err := os.Stat(output); !os.IsNotExist(err) {
			t.Fatal("unavailable profile created output")
		}
	}
	for _, flags := range [][]string{
		{"-ts", "1.2.840.10008.1.2.4.90", "-j2k-rate", "1"},
		{"-ts", "1.2.840.10008.1.2.4.91", "-allow-lossy"},
		{"-ts", "1.2.840.10008.1.2.4.91", "-j2k-rate", "10"},
	} {
		code, _, stderr := runConv(t, append(flags, "-j2k-backend", "openjpeg", "-o", "out.dcm", "missing-input.dcm")...)
		if code != 2 || strings.Contains(stderr, "no such file") || strings.Contains(stderr, "profile unavailable") {
			t.Fatalf("coding contract did not precede profile admission=%d %s", code, stderr)
		}
	}
}
