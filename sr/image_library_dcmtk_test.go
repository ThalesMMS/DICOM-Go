package sr

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/object"
)

// Actual peer SR conversion is opt-in and independent of the domain/template
// checks. It measures descriptor/group/reference survival, not complete IOD or
// clinical conformance. No success is inferred merely from dcmdump acceptance.
func TestImageLibraryDescriptorsDCMTK(t *testing.T) {
	toXML := dcmtkoracle.Tool(t, "dsr2xml")
	fromXML := dcmtkoracle.Tool(t, "xml2dsr")
	report := ilReport(
		ImageLibraryGroup{Descriptors: []ContentItem{ilModality("DX")}, Entries: []ImageLibraryEntry{ilEntry("1.2.3.131", ilNum("111026", "DCM", "mm", .4), ilText("111044", "L"))}},
		ImageLibraryGroup{Descriptors: []ContentItem{ilModality("CT")}, Entries: []ImageLibraryEntry{ilEntry("1.2.3.132", ilNum("112225", "DCM", "mm", 2), ContentItem{ValueType: ValueCode, RelationshipType: RelationshipHasAcqContext, ConceptName: ilCode("113820", "DCM"), Code: ilCode("113804", "DCM")})}},
		ImageLibraryGroup{Descriptors: []ContentItem{ilModality("MR")}, Entries: []ImageLibraryEntry{ilEntry("1.2.3.133", ilText("128230", "SyntheticSequence"), ilNum("130542", "DCM", "T", 3))}},
		ImageLibraryGroup{Descriptors: []ContentItem{ilModality("PT")}, Entries: []ImageLibraryEntry{ilEntry("1.2.3.134", ilNum("123006", "DCM", "Bq", 12000), ContentItem{ValueType: ValueDateTime, RelationshipType: RelationshipHasAcqContext, ConceptName: ilCode("123003", "DCM"), DateTime: "20261010110000"})}},
	)
	if err := ValidateImageLibrary(context.Background(), nil, report.ImageLibraryGroups, ImageLibraryOptions{}); err != nil {
		t.Fatal(err)
	}
	qualification, qualificationErr := ValidateImageLibraryTemplate(context.Background(), report, ImageLibraryOptions{}, TemplateValidationOptions{Mode: ValidationModeWarn})
	t.Logf("Separate pinned template report: %+v; error: %v; no complete IOD claim", qualification, qualificationErr)
	if qualificationErr != nil {
		t.Fatal(qualificationErr)
	}
	file, err := WriteMeasurementReport(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []struct {
		tag   core.Tag
		vr    core.VR
		value string
	}{{core.NewTag(0x0010, 0x0010), core.VRPN, "Synthetic^ImageLibrary"}, {core.NewTag(0x0010, 0x0020), core.VRLO, "SYNTHETIC1484"}, {core.NewTag(0x0010, 0x0030), core.VRDA, "19700101"}, {core.NewTag(0x0010, 0x0040), core.VRCS, "O"}, {core.NewTag(0x0008, 0x0020), core.VRDA, "20261010"}, {core.NewTag(0x0008, 0x0030), core.VRTM, "120000"}, {core.NewTag(0x0020, 0x0010), core.VRSH, "1484"}, {core.NewTag(0x0008, 0x0050), core.VRSH, "SYNTHETIC1484"}, {core.NewTag(0x0008, 0x0070), core.VRLO, "Synthetic"}} {
		file.Dataset.Put(strElem(a.tag, a.vr, a.value))
	}
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, file); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "synthetic-image-library.dcm")
	xml := filepath.Join(dir, "peer-image-library.xml")
	output := filepath.Join(dir, "peer-image-library.dcm")
	if err := os.WriteFile(input, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []struct {
		tool string
		args []string
	}{{toXML, []string{input, xml}}, {fromXML, []string{xml, output}}} {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		command := dcmtkoracle.Command(ctx, t, "exact descriptor concept/value/unit, IMAGE references, group association; XML whitespace and group/header defaults ignored", stage.tool, stage.args...)
		command.WaitDelay = time.Second
		capture := &ilPeerLog{}
		command.Stdout = capture
		command.Stderr = capture
		err := command.Run()
		cancel()
		t.Logf("Actual peer output: %s", capture.bytes.Bytes())
		if capture.exceeded {
			t.Fatal("peer output exceeded 64 KiB")
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	converted, err := object.OpenFile(output)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := ReadMeasurementReport(converted.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	if len(actual.ImageLibraryGroups) != 4 {
		t.Fatal("peer group count", len(actual.ImageLibraryGroups))
	}
	for i, want := range report.ImageLibraryGroups {
		got := actual.ImageLibraryGroups[i]
		if len(got.Descriptors) != 1 || got.Descriptors[0].Code.CodeValue != want.Descriptors[0].Code.CodeValue || len(got.Entries) != 1 {
			t.Fatalf("peer group %d association", i)
		}
		if got.Entries[0].Image.SOPInstanceUID != want.Entries[0].Image.SOPInstanceUID {
			t.Fatalf("peer group %d reference", i)
		}
		if len(got.Entries[0].Descriptors) != len(want.Entries[0].Descriptors) {
			t.Fatalf("peer group %d descriptors", i)
		}
		for j, d := range want.Entries[0].Descriptors {
			g := got.Entries[0].Descriptors[j]
			if g.ValueType != d.ValueType || g.ConceptName.CodeValue != d.ConceptName.CodeValue || g.ConceptName.CodingSchemeDesignator != d.ConceptName.CodingSchemeDesignator || g.Text != d.Text || g.DateTime != d.DateTime || g.Code.CodeValue != d.Code.CodeValue {
				t.Fatalf("peer descriptor %d/%d", i, j)
			}
			if d.Measurement != nil && (g.Measurement == nil || g.Measurement.Value != d.Measurement.Value || g.Measurement.Units.CodeValue != d.Measurement.Units.CodeValue || g.Measurement.Units.CodingSchemeDesignator != "UCUM") {
				t.Fatalf("peer numeric %d/%d", i, j)
			}
		}
	}
}

// Bounded memory capture still drains both pipes after the cap is reached.
type ilPeerLog struct {
	mu       sync.Mutex
	bytes    bytes.Buffer
	exceeded bool
}

func (log *ilPeerLog) Write(p []byte) (int, error) {
	log.mu.Lock()
	defer log.mu.Unlock()
	n := len(p)
	remaining := (64 << 10) - log.bytes.Len()
	if len(p) > remaining {
		log.exceeded = true
		p = p[:remaining]
	}
	_, _ = log.bytes.Write(p)
	return n, nil
}
