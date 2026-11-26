package doccli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/encapdoc"
	"github.com/ThalesMMS/dicom-go/internal/wrappercli"
)

func TestCDACommandOptInProfilePreservesSourceAndRefusesPublication(t *testing.T) {
	const header = `<ClinicalDocument xmlns="urn:hl7-org:v3"><templateId root="2.16.840.1.113883.10.20.22.1.2"/>
<id root="2.25.165"/><code code="%s" codeSystem="2.16.840.1.113883.6.1"/>
<effectiveTime value="20261115"/><confidentialityCode code="N" codeSystem="2.16.840.1.113883.5.25"/>
<recordTarget><patientRole><id root="2.25.166"/></patientRole></recordTarget>
<author><time value="20261115"/><assignedAuthor><id root="2.25.167"/></assignedAuthor></author>
<custodian><assignedCustodian><representedCustodianOrganization><id root="2.25.168"/></representedCustodianOrganization></assignedCustodian></custodian>
<component><structuredBody><component><section><text><paragraph>Reviewed.</paragraph></text></section></component></structuredBody></component></ClinicalDocument>
`
	for _, tc := range []struct {
		name, profile, code string
		wantErr             error
	}{
		{name: "ccd-match", profile: "ccd", code: "34133-9"},
		{name: "ccd-refuses-well-formed-xml", profile: "ccd", code: "hidden-source-value", wantErr: encapdoc.ErrInvalidDocument},
		{name: "base-checks-without-ccd", profile: "base", code: "hidden-source-value"},
		{name: "default-compatibility", code: "hidden-source-value"},
		{name: "unknown-profile", profile: "unsupported-profile", code: "34133-9", wantErr: encapdoc.ErrUnsupportedCDAValidationProfile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			input, output := filepath.Join(dir, "report.xml"), filepath.Join(dir, "report.dcm")
			original := []byte(fmt.Sprintf(header, tc.code))
			if err := os.WriteFile(input, original, 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{input, output}
			if tc.profile != "" {
				args = append([]string{"-check-profile", tc.profile}, args...)
			}
			var stdout, stderr bytes.Buffer
			err := Execute(context.Background(), "cda2dcm", args, &stdout, &stderr)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Execute() = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("validation refusal created destination: %v", err)
				}
				diagnostic := fmt.Sprint(err) + stderr.String()
				if stdout.Len() != 0 || strings.Contains(diagnostic, tc.code) ||
					tc.profile == "ccd" && !strings.Contains(diagnostic, "ccd-document-code at /ClinicalDocument/code") {
					t.Fatalf("unexpected refusal diagnostic: %q, stdout: %q", diagnostic, stdout.String())
				}
				return
			}
			file, err := wrappercli.ReadDICOM(output, false)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			doc, err := encapdoc.Extract(file.Dataset)
			if err != nil || !bytes.Equal(doc.Data, original) {
				t.Fatalf("published document bytes changed, Extract() = %v", err)
			}
			onDisk, err := os.ReadFile(input)
			if err != nil || !bytes.Equal(onDisk, original) {
				t.Fatalf("input document changed, read = %v", err)
			}
		})
	}
}
