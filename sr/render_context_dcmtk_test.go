package sr

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// This is a focused presentation comparison, not a formatting oracle or a
// general SR conformance claim. The fixture is authored independently here.
func TestContextRenderDCMTKIdentifiers(t *testing.T) {
	tool := requireDSRDump(t)
	doc, metadata := contextRenderFixture()
	// The oracle fixture supplies an optional URN coding scheme. The separate
	// contract tests cover the standard-permitted scheme-free URN form.
	doc.Content[0].ConceptName.CodingSchemeDesignator = "99CONCEPT"
	doc.Content[3].ConceptName = CodedEntry{CodeValue: "SHORTNAME", CodingSchemeDesignator: "99TEST", CodeMeaning: "Short name"}
	doc.SOPInstanceUID = "1.2.826.0.1.3680043.10.543.1533.1"
	doc.StudyInstanceUID = "1.2.826.0.1.3680043.10.543.1533.2"
	doc.SeriesInstanceUID = "1.2.826.0.1.3680043.10.543.1533.3"
	snapshot := mustSnapshot(t, doc, metadata)
	dataset, err := doc.DatasetWithMetadataSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "synthetic-code-context.dcm")
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := dcmtkoracle.Command(ctx, t, "exact identifier/scheme/version presence in synthetic code roles; formatting not compared", tool, "+Pn", "+Pc", "+Pl", path)
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	t.Logf("actual DCMTK presentation:\n%s", out)
	if err != nil {
		t.Fatalf("DCMTK presentation: %v", err)
	}
	var local bytes.Buffer
	if err := RenderTextWithMetadataSnapshot(&local, doc, snapshot, RenderOptions{ShowCodes: true}); err != nil {
		t.Fatal(err)
	}
	t.Logf("actual dicom-go presentation:\n%s", local.String())
	for _, value := range []string{"synthetic-long-title-code", "urn:example:concept", "synthetic-long-value-code", "synthetic-long-units-code", "urn:example:qualifier", "SHORT", "99TITLE", "99VALUE", "99UNITS", "99QUAL", "99TEST", "V1", "V2", "V3", "V4", "V5"} {
		if !strings.Contains(string(out), value) || !strings.Contains(local.String(), value) {
			t.Errorf("identifier/scheme/version %q absent from an actual presentation", value)
		}
	}
	for _, role := range []struct {
		prefix string
		values []string
	}{
		{"1  ", []string{"synthetic-long-title-code", "99TITLE", "V1"}},
		{"1.1  ", []string{"urn:example:concept", "99CONCEPT", "synthetic-long-value-code", "99VALUE", "V2"}},
		{"1.2  ", []string{"synthetic-long-units-code", "99UNITS", "V3"}},
		{"1.3  ", []string{"urn:example:qualifier", "99QUAL", "V4"}},
		{"1.4  ", []string{"SHORT", "99TEST", "V5"}},
	} {
		line := ""
		for _, candidate := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(candidate, role.prefix) {
				line = candidate
				break
			}
		}
		for _, value := range role.values {
			if !strings.Contains(line, value) {
				t.Errorf("DCMTK role %s omits %q", role.prefix, value)
			}
		}
	}
}
