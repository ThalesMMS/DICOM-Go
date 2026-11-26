package sr

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

// dcmtkOracleReport is the report testdata/dcmtk_tid1500_oracle.cxx builds
// with DCMTK's TID 1500 classes (testdata/dcmtk_tid1500.dcm). DCMTK's
// report has no evidence sequence, so its references have no study or
// series (withEvidence adds them).
func dcmtkOracleReport() *MeasurementReport {
	const ct = "1.2.826.0.1.3680043.8.498.20260926.1"
	lesion := CodedEntry{CodeValue: "52988006", CodingSchemeDesignator: "SCT", CodeMeaning: "Lesion"}
	return &MeasurementReport{
		SOPClassUID:       EnhancedSRStorage,
		SOPInstanceUID:    "1.2.826.0.1.3680043.8.498.20260926.7",
		SeriesInstanceUID: "1.2.826.0.1.3680043.8.498.20260926.5",
		ContentDate:       "20260926",
		ContentTime:       "120000",
		Observers:         []Observer{{DeviceUID: "1.2.826.0.1.3680043.8.498.20260926.9", DeviceName: "dicom-go test"}},
		Groups: []MeasurementGroup{
			{
				Tracking:          TrackingIdentifier{Identifier: "Lesion 1", UID: "1.2.826.0.1.3680043.8.498.20260926.3"},
				Finding:           lesion,
				FindingSites:      []CodedEntry{{CodeValue: "39607008", CodingSchemeDesignator: "SCT", CodeMeaning: "Lung"}},
				ReferencedSegment: SegmentReference{SOPClassUID: "1.2.840.10008.5.1.4.1.1.66.4", SOPInstanceUID: "1.2.826.0.1.3680043.8.498.20260926.2", SegmentNumber: 1},
				SourceSeriesUID:   "1.2.826.0.1.3680043.8.498.20260926.6",
				Measurements: []ReportMeasurement{{
					ConceptName: CodedEntry{CodeValue: "118565006", CodingSchemeDesignator: "SCT", CodeMeaning: "Volume"},
					Value:       1234.5,
					Units:       CodedEntry{CodeValue: "mm3", CodingSchemeDesignator: "UCUM", CodeMeaning: "cubic millimeter"},
				}},
			},
			{
				Tracking: TrackingIdentifier{Identifier: "Lesion 2", UID: "1.2.826.0.1.3680043.8.498.20260926.4"},
				Finding:  lesion,
				Measurements: []ReportMeasurement{{
					ConceptName: CodedEntry{CodeValue: "81827009", CodingSchemeDesignator: "SCT", CodeMeaning: "Diameter"},
					Value:       12.5,
					Units:       CodedEntry{CodeValue: "mm", CodingSchemeDesignator: "UCUM", CodeMeaning: "millimeter"}, // as in the DCMTK fixture
					Image:       ImageReference{SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: ct},
				}},
			},
		},
	}
}

// contentOutline lists every content item below the root as its path,
// relationship, value type, concept and value, the structure two writers of
// the same report must agree on.
func contentOutline(items []ContentItem, prefix string) []string {
	var out []string
	for i, item := range items {
		path := fmt.Sprintf("%s%d", prefix, i+1)
		value := ""
		switch item.ValueType {
		case ValueCode:
			value = item.Code.CodeValue + "^" + item.Code.CodingSchemeDesignator
		case ValueText:
			value = item.Text
		case ValueUIDRef:
			value = item.UID
		case ValueNum:
			if item.Measurement != nil {
				value = fmt.Sprintf("%g %s", item.Measurement.Value, item.Measurement.Units.CodeValue)
			}
		case ValueImage:
			value = item.Image.SOPClassUID + "/" + item.Image.SOPInstanceUID
		}
		out = append(out, fmt.Sprintf("%s %s %s (%s,%s) %q", path, item.RelationshipType, item.ValueType,
			item.ConceptName.CodeValue, item.ConceptName.CodingSchemeDesignator, value))
		out = append(out, contentOutline(item.Children, path+".")...)
	}
	return out
}

// templatePaths maps the items that identify a template to the template.
func templatePaths(metadata TemplateValidationMetadata) map[string]string {
	out := map[string]string{}
	for _, identification := range metadata.TemplateIdentifications {
		out[identification.Path] = identification.MappingResource + " " + identification.Identifier
	}
	return out
}

func readDCMTKOracle(t *testing.T) *object.File {
	t.Helper()
	file, err := object.OpenFile(filepath.Join("testdata", "dcmtk_tid1500.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

// TestWriteMeasurementReportMatchesDCMTK writes the oracle report and
// compares its content tree with the one DCMTK's TID 1500 classes wrote.
func TestWriteMeasurementReportMatchesDCMTK(t *testing.T) {
	ours, err := WriteMeasurementReport(withEvidence(dcmtkOracleReport()))
	if err != nil {
		t.Fatal(err)
	}
	oursRead, err := ReadDocumentWithOptions(ours.Dataset, ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	theirsRead, err := ReadDocumentWithOptions(readDCMTKOracle(t).Dataset, ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, want := contentOutline(oursRead.Document.Content, ""), contentOutline(theirsRead.Document.Content, "")
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("content tree differs from DCMTK:\nours\n%s\nDCMTK\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Both identify the same templates at the same items; DCMTK also
	// identifies the Image Library (TID 1600).
	oursTemplates, theirsTemplates := templatePaths(oursRead.TemplateMetadata), templatePaths(theirsRead.TemplateMetadata)
	for path, template := range theirsTemplates {
		if template == "DCMR 1600" {
			delete(theirsTemplates, path)
		}
	}
	if !reflect.DeepEqual(oursTemplates, theirsTemplates) {
		t.Fatalf("templates: ours %v, DCMTK %v", oursTemplates, theirsTemplates)
	}
}

// TestReadMeasurementReportReadsDCMTK reads the report DCMTK wrote.
func TestReadMeasurementReportReadsDCMTK(t *testing.T) {
	report, err := ReadMeasurementReport(readDCMTKOracle(t).Dataset)
	if err != nil {
		t.Fatal(err)
	}
	want := dcmtkOracleReport()
	if report.Legacy || report.Language != DefaultLanguage || !reflect.DeepEqual(report.Observers, want.Observers) ||
		!reflect.DeepEqual(report.ProceduresReported, []CodedEntry{DefaultProcedureReported}) ||
		len(report.ImageLibrary) != 1 || report.ImageLibrary[0].SOPInstanceUID != want.Groups[1].Measurements[0].Image.SOPInstanceUID {
		t.Fatalf("report context: %+v", report)
	}
	want.Groups[0].Template, want.Groups[1].Template = TemplateVolumetricMeasurements, TemplateMeasurementGroup
	if !reflect.DeepEqual(report.Groups, want.Groups) {
		t.Fatalf("groups\n%+v\nwant\n%+v", report.Groups, want.Groups)
	}
}

// TestMeasurementReportRoundTripsTheStandardContext writes and reads the
// optional parts of the report context.
func TestMeasurementReportRoundTripsTheStandardContext(t *testing.T) {
	report := withEvidence(dcmtkOracleReport())
	report.Language = CodedEntry{CodeValue: "de", CodingSchemeDesignator: "RFC5646", CodeMeaning: "German"}
	report.Observers = []Observer{
		{PersonName: "Doe^Jane", OrganizationName: "Synthetic Hospital"},
		{DeviceUID: "1.2.3.4", DeviceName: "Analyzer", DeviceManufacturer: "Acme", DeviceModelName: "M1", DeviceSerialNumber: "S1"},
	}
	report.ProceduresReported = []CodedEntry{{CodeValue: "77477000", CodingSchemeDesignator: "SCT", CodeMeaning: "CT of chest"}}
	report.ImageLibrary = []ImageReference{}
	file, err := WriteMeasurementReport(report)
	if err != nil {
		t.Fatal(err)
	}
	round, err := ReadMeasurementReport(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	if round.Language != report.Language || !reflect.DeepEqual(round.Observers, report.Observers) ||
		!reflect.DeepEqual(round.ProceduresReported, report.ProceduresReported) || len(round.ImageLibrary) != 0 {
		t.Fatalf("context: %+v", round)
	}
	if _, err := WriteMeasurementReport(&MeasurementReport{SOPInstanceUID: "1.2.3", Observers: []Observer{{}}}); err == nil {
		t.Fatal("accepted an observer without a name or device UID")
	}
}

// TestReadMeasurementReportReadsTheEarlierLayout reads a report written
// before TID 1500 conformance, with its groups directly under the root.
func TestReadMeasurementReportReadsTheEarlierLayout(t *testing.T) {
	file, err := WriteMeasurementReport(withEvidence(dcmtkOracleReport()))
	if err != nil {
		t.Fatal(err)
	}
	children, _ := file.Dataset.GetSequence(tagContentSequence)
	groups, _ := children[len(children)-1].GetSequence(tagContentSequence)
	items := make([]core.DataSet, len(groups))
	for i, group := range groups {
		items[i] = group.ToDataSet()
	}
	legacy := object.FromDataSet(file.Dataset.ToDataSet(), nil)
	legacy.Put(derivedio.Seq(tagContentSequence, items...))
	report, err := ReadMeasurementReport(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Legacy || len(report.Groups) != 2 || report.Groups[1].Measurements[0].Value != 12.5 {
		t.Fatalf("legacy report: %+v", report)
	}
}

// TestMeasurementReportPassesDCMTKChecks runs dsrdump and dciodvfy on the
// written interop report (with observers, a derivation and 3D coordinates).
// Enable with DICOMGO_DCMTK_INTEGRATION=1 (dsrdump on PATH)
// and DICOMGO_DCIODVFY (the dicom3tools dciodvfy executable).
func TestMeasurementReportPassesDCMTKChecks(t *testing.T) {
	dsrdump := dcmtkoracle.Tool(t, "dsrdump")
	file, err := WriteMeasurementReport(interopReport())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tid1500.dcm")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := object.WriteFile(out, file); err != nil {
		t.Fatal(err)
	}
	_ = out.Close()
	dump := dcmtkoracle.Run(t, dcmtkoracle.Accepted, dsrdump, "-Ec", path)
	if strings.Contains(string(dump), "W: ") || strings.Contains(string(dump), "E: ") {
		t.Fatalf("dsrdump:\n%s", dump)
	}
	if dciodvfy := os.Getenv("DICOMGO_DCIODVFY"); dciodvfy != "" {
		report, _ := exec.Command(dciodvfy, "-new", path).CombinedOutput()
		for _, line := range strings.Split(string(report), "\n") {
			if strings.HasPrefix(line, "Error") {
				t.Errorf("dciodvfy: %s", line)
			}
		}
	}
}

// withEvidence gives the references of report their study and series.
func withEvidence(report *MeasurementReport) *MeasurementReport {
	const study, ctSeries, segSeries = "1.2.826.0.1.3680043.8.498.20260926.10", "1.2.826.0.1.3680043.8.498.20260926.11", "1.2.826.0.1.3680043.8.498.20260926.12"
	report.StudyInstanceUID = study
	report.Groups[0].ReferencedSegment.StudyInstanceUID, report.Groups[0].ReferencedSegment.SeriesInstanceUID = study, segSeries
	report.Groups[1].Measurements[0].Image.StudyInstanceUID, report.Groups[1].Measurements[0].Image.SeriesInstanceUID = study, ctSeries
	return report
}

// TestMeasurementReportListsItsEvidence lists every referenced instance in
// the Current Requested Procedure Evidence Sequence and reads the study and
// series of the references back from it.
func TestMeasurementReportListsItsEvidence(t *testing.T) {
	report := withEvidence(dcmtkOracleReport())
	file, err := WriteMeasurementReport(report)
	if err != nil {
		t.Fatal(err)
	}
	studies, ok := file.Dataset.GetSequence(tagCurrentRequestedProcedureEvidence)
	if !ok || len(studies) != 1 {
		t.Fatalf("evidence: %d studies", len(studies))
	}
	if series, _ := studies[0].GetSequence(tagReferencedSeriesSequence); len(series) != 2 {
		t.Fatalf("evidence: %d series", len(series))
	}
	round, err := ReadMeasurementReport(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	if round.Groups[0].ReferencedSegment != report.Groups[0].ReferencedSegment || round.ImageLibrary[0].SeriesInstanceUID != report.Groups[1].Measurements[0].Image.SeriesInstanceUID ||
		!reflect.DeepEqual(round.Groups[1].Measurements[0].Image, report.Groups[1].Measurements[0].Image) {
		t.Fatalf("references: %+v", round.Groups)
	}
}
