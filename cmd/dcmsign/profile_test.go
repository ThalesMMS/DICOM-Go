package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/testutil"
)

func TestAuthorizationProfileRequiresExplicitHumanDeclaration(t *testing.T) {
	dir := t.TempDir()
	input := writeInput(t, dir)
	pki := testutil.NewTestPKI(t)
	cred := pki.Issue(t, "human-declaration-cli.test", "rsa2048")
	output := filepath.Join(dir, "signed.dcm")
	base := []string{"sign", "-key", cred.KeyFile, "-cert", cred.CertFile, "-profile", "auth", "-o", output}
	code, out := dcmsign(t, append(base, input)...)
	if code == 0 || !strings.Contains(out, "coverage incomplete") {
		t.Fatalf("unknown human context: %d %s", code, out)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("failed preflight published output: %v", err)
	}
	args := append(append([]string(nil), base...), "-human-verified-empty", input)
	mustRun(t, args...)
	code, out = dcmsign(t, "verify", "-require-profile", "auth", output)
	if code == 0 || !strings.Contains(out, "needs-context") {
		t.Fatalf("verify unknown human context: %d %s", code, out)
	}
	out = mustRun(t, "verify", "-require-profile", "auth", "-human-verified-empty", output)
	if !strings.Contains(out, "attribute-profile coverage: Authorization RSA (complete)") {
		t.Fatalf("bounded coverage label: %s", out)
	}
	out = mustRun(t, "verify", "-require-profile", "auth", "-human-verified-path", "ReferencedStudySequence[0].ReferencedSOPInstanceUID", output)
	if !strings.Contains(out, "attribute-profile coverage") {
		t.Fatal(out)
	}
	out = mustRun(t, "list", output)
	if strings.Contains(out, "profiles met") || !strings.Contains(out, "Authorization RSA (needs-context)") {
		t.Fatalf("misleading profile label: %s", out)
	}
	for _, extra := range [][]string{
		{"-human-verified-empty", "-human-verified-path", "PatientName"},
		{"-human-verified-path", "ReferencedStudySequence[*].ReferencedSOPInstanceUID"},
		{"-human-verified-path", "ReferencedStudySequence[0]"},
	} {
		args := append([]string{"verify", "-require-profile", "auth"}, extra...)
		args = append(args, output)
		if code, out := dcmsign(t, args...); code == 0 {
			t.Fatalf("invalid declaration accepted: %s", out)
		}
	}
	// Declaring a human attribute does not silently add it to the signature.
	subset := filepath.Join(dir, "subset.dcm")
	mustRun(t, "sign", "-key", cred.KeyFile, "-cert", cred.CertFile, "-tag", "SOPClassUID", "-tag", "SOPInstanceUID", "-tag", "StudyInstanceUID", "-tag", "SeriesInstanceUID", "-o", subset, input)
	if code, out := dcmsign(t, "verify", "-require-profile", "auth", "-human-verified-path", "PatientName", subset); code == 0 || !strings.Contains(out, "attribute-profile issue") {
		t.Fatalf("unsigned declaration: %d %s", code, out)
	}
}
