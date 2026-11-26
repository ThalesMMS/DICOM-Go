package sr

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// A focused attribute roundtrip through DCMTK's independent SR implementation.
// It is not an IOD, clinical or PACS qualification and makes no speed claim.
func TestNumericRepresentationsDCMTK(t *testing.T) {
	dsr2xml := dcmtkoracle.Tool(t, "dsr2xml")
	xml2dsr := dcmtkoracle.Tool(t, "xml2dsr")
	dcmdump := dcmtkoracle.Tool(t, "dcmdump")
	representation := NumericRepresentation{DecimalString: " 1.2300E-01 ", HasDecimalString: true, HasFloatingPoint: true, FloatingPoint: .123, HasRational: true, RationalNumerator: 123, RationalDenominator: 1000}
	source := numericFixture(t, representation, false)
	result, snapshot, err := ReadDocumentWithNumericRepresentations(source, ReadOptions{}, MetadataSnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	emitted, err := result.Document.DatasetWithMetadataSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	// Supply synthetic common attributes so XML has a nonempty patient/study.
	for _, a := range []struct {
		tag   core.Tag
		vr    core.VR
		value string
	}{
		{core.NewTag(0x0010, 0x0010), core.VRPN, "Numeric^Fixture"},
		{core.NewTag(0x0010, 0x0020), core.VRLO, "SYNTHETIC1526"},
		{core.NewTag(0x0010, 0x0030), core.VRDA, "19700101"},
		{core.NewTag(0x0010, 0x0040), core.VRCS, "O"},
		{core.NewTag(0x0008, 0x0020), core.VRDA, "20261009"},
		{core.NewTag(0x0008, 0x0030), core.VRTM, "120000"},
		{core.NewTag(0x0008, 0x0090), core.VRPN, "Synthetic^Physician"},
		{core.NewTag(0x0020, 0x0010), core.VRSH, "1526"},
		{core.NewTag(0x0008, 0x0050), core.VRSH, "SYNTHETIC1526"},
		{core.NewTag(0x0008, 0x0070), core.VRLO, "Synthetic"},
	} {
		emitted.Put(strElem(a.tag, a.vr, a.value))
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "synthetic-numeric.dcm")
	xml := filepath.Join(dir, "synthetic-numeric.xml")
	output := filepath.Join(dir, "dcmtk-numeric.dcm")
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, &object.File{Dataset: emitted, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(tool string, args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command := dcmtkoracle.Command(ctx, t, "exact local DS bytes; DCMTK DS spelling after its observed space trimming; exact FD bits and fraction attrs", tool, args...)
		command.WaitDelay = time.Second
		out, err := command.CombinedOutput()
		t.Logf("actual tool output:\n%s", out)
		if err != nil {
			t.Fatalf("numeric oracle: %v", err)
		}
	}
	run(dsr2xml, input, xml)
	xmlBytes, err := os.ReadFile(xml)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("actual DCMTK SR XML:\n%s", xmlBytes)
	run(xml2dsr, xml, output)
	run(dcmdump, "+P", "0040,a30a", "+P", "0040,a161", "+P", "0040,a162", "+P", "0040,a163", output)
	converted, err := object.OpenFile(output)
	if err != nil {
		t.Fatal(err)
	}
	numericSame(t, emitted, representation)
	convertedExpected := representation
	convertedExpected.DecimalString = "1.2300E-01"
	t.Logf("observed DCMTK XML DS space normalization: %q -> %q", representation.DecimalString, convertedExpected.DecimalString)
	if !strings.Contains(string(xmlBytes), "<value>1.2300E-01</value>") {
		t.Fatal("unexpected DCMTK decimal spelling")
	}
	numericSame(t, converted.Dataset, convertedExpected)
}
