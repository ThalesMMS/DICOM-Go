package sr

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
)

var (
	meanDerivation     = CodedEntry{CodeValue: "373098007", CodingSchemeDesignator: "SCT", CodeMeaning: "Mean"}
	maximumDerivation  = CodedEntry{CodeValue: "56851009", CodingSchemeDesignator: "SCT", CodeMeaning: "Maximum"}
	attenuationConcept = CodedEntry{CodeValue: "112031", CodingSchemeDesignator: "DCM", CodeMeaning: "Attenuation Coefficient"}
	hounsfieldUnit     = CodedEntry{CodeValue: "[hnsf'U]", CodingSchemeDesignator: "UCUM", CodeMeaning: "Hounsfield unit"}
)

// interopReport is the oracle report with the optional parts an independent
// reader should find: a person and a device observer, a derived measurement
// and 3D coordinates, in the Comprehensive 3D SR.
func interopReport() *MeasurementReport {
	report := withEvidence(dcmtkOracleReport())
	report.SOPClassUID = Comprehensive3DSRStorage
	report.Observers = append([]Observer{{PersonName: "Reader^Synthetic"}}, report.Observers...)
	report.Groups[0].Measurements = append(report.Groups[0].Measurements, ReportMeasurement{
		ConceptName: attenuationConcept,
		Value:       15.5,
		Units:       hounsfieldUnit,
		Derivation:  meanDerivation,
	})
	report.Groups[1].Measurements[0].Spatial = SpatialReference{
		FrameOfReferenceUID: "1.2.826.0.1.3680043.8.498.20260926.13",
		GraphicType:         GraphicTypePoint3D,
		Coordinates:         []Point3D{{X: 1.5, Y: -2, Z: 30.25}},
	}
	return report
}

// TestMeasurementReportRoundTripsDerivation writes and reads the derivation
// of a measurement (TID 300 row 3).
func TestMeasurementReportRoundTripsDerivation(t *testing.T) {
	report := interopReport()
	file, err := WriteMeasurementReport(report)
	if err != nil {
		t.Fatal(err)
	}
	round, err := ReadMeasurementReport(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	report.Groups[0].Template, report.Groups[1].Template = TemplateVolumetricMeasurements, TemplateMeasurementGroup
	if !reflect.DeepEqual(round.Groups, report.Groups) {
		t.Fatalf("groups\n%+v\nwant\n%+v", round.Groups, report.Groups)
	}
}

// highdicomCode is a coded entry as testdata/highdicom_measurement_report.py
// prints it.
type highdicomCode struct {
	Value   string `json:"value"`
	Scheme  string `json:"scheme"`
	Meaning string `json:"meaning"`
}

type highdicomImage struct {
	SOPClass    string `json:"sop_class"`
	SOPInstance string `json:"sop_instance"`
}

type highdicomCoordinates struct {
	GraphicType      string    `json:"graphic_type"`
	FrameOfReference string    `json:"frame_of_reference"`
	Data             []float64 `json:"data"`
}

type highdicomMeasurement struct {
	Name        *highdicomCode         `json:"name"`
	Value       float64                `json:"value"`
	Unit        *highdicomCode         `json:"unit"`
	Derivation  *highdicomCode         `json:"derivation"`
	Images      []highdicomImage       `json:"images"`
	Coordinates []highdicomCoordinates `json:"coordinates"`
}

type highdicomSegment struct {
	SOPClass       string `json:"sop_class"`
	SOPInstance    string `json:"sop_instance"`
	SegmentNumbers []int  `json:"segment_numbers"`
	SourceSeries   string `json:"source_series"`
}

type highdicomGroup struct {
	Kind               string                 `json:"kind"`
	TrackingIdentifier string                 `json:"tracking_identifier"`
	TrackingUID        string                 `json:"tracking_uid"`
	Finding            *highdicomCode         `json:"finding"`
	FindingSites       []highdicomCode        `json:"finding_sites"`
	Measurements       []highdicomMeasurement `json:"measurements"`
	Segment            *highdicomSegment      `json:"segment,omitempty"`
}

type highdicomObserver struct {
	Type       highdicomCode `json:"type"`
	PersonName string        `json:"person_name,omitempty"`
	DeviceUID  string        `json:"device_uid,omitempty"`
	DeviceName string        `json:"device_name,omitempty"`
}

type highdicomEvidence struct {
	Study       string `json:"study"`
	Series      string `json:"series"`
	SOPInstance string `json:"sop_instance"`
	SOPClass    string `json:"sop_class"`
}

// highdicomSummary is what highdicom's MeasurementReport found in a report.
type highdicomSummary struct {
	Versions     map[string]string   `json:"versions"`
	SOPClass     string              `json:"sop_class"`
	Template     string              `json:"template"`
	Observers    []highdicomObserver `json:"observers"`
	ImageLibrary []highdicomImage    `json:"image_library"`
	Groups       []highdicomGroup    `json:"groups"`
	Evidence     []highdicomEvidence `json:"evidence"`
}

func toHighdicomCode(code CodedEntry) *highdicomCode {
	if code.Empty() {
		return nil
	}
	return &highdicomCode{Value: code.CodeValue, Scheme: code.CodingSchemeDesignator, Meaning: code.CodeMeaning}
}

// expectedHighdicomSummary is what highdicom should find in the written
// report: its image measurement groups (TID 1501) before its volumetric ones
// (TID 1411), as the script lists them.
func expectedHighdicomSummary(report *MeasurementReport) highdicomSummary {
	want := highdicomSummary{SOPClass: report.SOPClassUID, Template: TemplateMeasurementReport}
	for _, observer := range report.Observers {
		if observer.PersonName != "" {
			want.Observers = append(want.Observers, highdicomObserver{Type: *toHighdicomCode(personObserverType), PersonName: observer.PersonName})
			continue
		}
		want.Observers = append(want.Observers, highdicomObserver{Type: *toHighdicomCode(deviceObserverType), DeviceUID: observer.DeviceUID, DeviceName: observer.DeviceName})
	}
	for _, image := range measuredImages(report.Groups) {
		want.ImageLibrary = append(want.ImageLibrary, highdicomImage{SOPClass: image.SOPClassUID, SOPInstance: image.SOPInstanceUID})
	}
	for _, image := range measurementEvidence(report) {
		want.Evidence = append(want.Evidence, highdicomEvidence{Study: image.StudyInstanceUID, Series: image.SeriesInstanceUID,
			SOPInstance: image.SOPInstanceUID, SOPClass: image.SOPClassUID})
	}
	var imageGroups, volumetricGroups []highdicomGroup
	for _, group := range report.Groups {
		out := highdicomGroup{
			Kind:               "image",
			TrackingIdentifier: group.Tracking.Identifier,
			TrackingUID:        group.Tracking.UID,
			Finding:            toHighdicomCode(group.Finding),
			FindingSites:       []highdicomCode{},
			Measurements:       []highdicomMeasurement{},
		}
		for _, site := range group.FindingSites {
			out.FindingSites = append(out.FindingSites, *toHighdicomCode(site))
		}
		for _, measurement := range group.Measurements {
			m := highdicomMeasurement{
				Name:        toHighdicomCode(measurement.ConceptName),
				Value:       measurement.Value,
				Unit:        toHighdicomCode(measurement.Units),
				Derivation:  toHighdicomCode(measurement.Derivation),
				Images:      []highdicomImage{},
				Coordinates: []highdicomCoordinates{},
			}
			if hasImageReference(measurement.Image) {
				m.Images = append(m.Images, highdicomImage{SOPClass: measurement.Image.SOPClassUID, SOPInstance: measurement.Image.SOPInstanceUID})
			}
			if hasSpatialReference(measurement.Spatial) {
				m.Coordinates = append(m.Coordinates, highdicomCoordinates{GraphicType: measurement.Spatial.GraphicType,
					FrameOfReference: measurement.Spatial.FrameOfReferenceUID, Data: flattenPoint3D(measurement.Spatial.Coordinates)})
			}
			out.Measurements = append(out.Measurements, m)
		}
		if !hasSegmentReference(group.ReferencedSegment) {
			imageGroups = append(imageGroups, out)
			continue
		}
		out.Kind = "volumetric"
		out.Segment = &highdicomSegment{SOPClass: group.ReferencedSegment.SOPClassUID, SOPInstance: group.ReferencedSegment.SOPInstanceUID,
			SegmentNumbers: []int{group.ReferencedSegment.SegmentNumber}, SourceSeries: group.SourceSeriesUID}
		volumetricGroups = append(volumetricGroups, out)
	}
	want.Groups = append(imageGroups, volumetricGroups...)
	return want
}

// TestMeasurementReportReadsInHighdicom writes a report and reads it with
// highdicom (highdicom.sr.srread and its MeasurementReport), which must find
// the same observers, image library, groups, tracking identifiers,
// measurements, references and evidence. Enable with DICOM_GO_HIGHDICOM=1;
// DICOM_GO_PYTHON selects an interpreter with highdicom (0.28.1 was
// qualified), otherwise python3.
func TestMeasurementReportReadsInHighdicom(t *testing.T) {
	if os.Getenv("DICOM_GO_HIGHDICOM") != "1" {
		t.Skip("opt-in: DICOM_GO_HIGHDICOM=1 and DICOM_GO_PYTHON with highdicom")
	}
	python := strings.TrimSpace(os.Getenv("DICOM_GO_PYTHON"))
	if python == "" {
		var err error
		if python, err = exec.LookPath("python3"); err != nil {
			t.Fatal("python3 is required when DICOM_GO_HIGHDICOM=1")
		}
	}
	report := interopReport()
	file, err := WriteMeasurementReport(report)
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
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(python, filepath.Join("testdata", "highdicom_measurement_report.py"), path).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("highdicom rejected the report: %v\n%s", err, exit.Stderr)
		}
		t.Fatal(err)
	}
	var got highdicomSummary
	if err := json.Unmarshal(output, &got); err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	t.Logf("oracle highdicom %s (pydicom %s)", got.Versions["highdicom"], got.Versions["pydicom"])
	got.Versions = nil
	want := expectedHighdicomSummary(report)
	for _, evidence := range [][]highdicomEvidence{got.Evidence, want.Evidence} {
		sort.Slice(evidence, func(i, j int) bool { return evidence[i].SOPInstance < evidence[j].SOPInstance })
	}
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", " ")
		wantJSON, _ := json.MarshalIndent(want, "", " ")
		t.Fatalf("highdicom read\n%s\nwant\n%s", gotJSON, wantJSON)
	}
}

// dcmqiReport is the report of testdata/upstream/dcmqi/tid1500.dcm, written
// by dcmqi's tid1500writer from testdata/dcmqi_tid1500.json.
func dcmqiReport() *MeasurementReport {
	const (
		study     = "1.2.826.0.1.3680043.8.498.20260926.20"
		ctSeries  = "1.2.826.0.1.3680043.8.498.20260926.21"
		segSeries = "1.2.826.0.1.3680043.8.498.20260926.22"
		ct        = "1.2.826.0.1.3680043.8.498.20260926.25."
	)
	neoplasm := CodedEntry{CodeValue: "108369006", CodingSchemeDesignator: "SCT", CodeMeaning: "Neoplasm"}
	volume := CodedEntry{CodeValue: "118565006", CodingSchemeDesignator: "SCT", CodeMeaning: "Volume"}
	cubicMillimeter := CodedEntry{CodeValue: "mm3", CodingSchemeDesignator: "UCUM", CodeMeaning: "cubic millimeter"}
	segment := SegmentReference{StudyInstanceUID: study, SeriesInstanceUID: segSeries, SOPClassUID: "1.2.840.10008.5.1.4.1.1.66.4",
		SOPInstanceUID: "1.2.826.0.1.3680043.8.498.20260926.23", SegmentNumber: 1}
	report := &MeasurementReport{
		SOPClassUID:        EnhancedSRStorage,
		StudyInstanceUID:   study,
		Language:           CodedEntry{CodeValue: "eng", CodingSchemeDesignator: "RFC5646", CodeMeaning: "English"},
		Observers:          []Observer{{PersonName: "Reader^Synthetic"}},
		ProceduresReported: []CodedEntry{{CodeValue: "77477000", CodingSchemeDesignator: "SCT", CodeMeaning: "CT of chest"}},
		Groups: []MeasurementGroup{
			{
				Template:          TemplateVolumetricMeasurements,
				Tracking:          TrackingIdentifier{Identifier: "Lesion 1", UID: "1.2.826.0.1.3680043.8.498.20260926.26"},
				Finding:           neoplasm,
				FindingSites:      []CodedEntry{{CodeValue: "39607008", CodingSchemeDesignator: "SCT", CodeMeaning: "Lung"}},
				ReferencedSegment: segment,
				SourceSeriesUID:   ctSeries,
				Measurements: []ReportMeasurement{
					{ConceptName: volume, Value: 24, Units: cubicMillimeter},
					{ConceptName: attenuationConcept, Value: 15.5, Units: hounsfieldUnit, Derivation: meanDerivation},
					{ConceptName: attenuationConcept, Value: 30, Units: hounsfieldUnit, Derivation: maximumDerivation},
				},
			},
			{
				Template:          TemplateVolumetricMeasurements,
				Tracking:          TrackingIdentifier{Identifier: "Lesion 1 core"},
				Finding:           neoplasm,
				ReferencedSegment: segment,
				SourceSeriesUID:   ctSeries,
				Measurements:      []ReportMeasurement{{ConceptName: volume, Value: 8, Units: cubicMillimeter}},
			},
		},
	}
	for _, n := range []string{"1", "2", "3"} {
		report.ImageLibrary = append(report.ImageLibrary, ImageReference{StudyInstanceUID: study, SeriesInstanceUID: ctSeries,
			SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: ct + n})
	}
	return report
}

// TestReadMeasurementReportReadsDcmqi reads the report dcmqi's
// tid1500writer wrote, then writes and reads it again.
func TestReadMeasurementReportReadsDcmqi(t *testing.T) {
	file, err := object.OpenFile(filepath.Join("..", "testdata", "upstream", "dcmqi", "tid1500.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	report, err := ReadMeasurementReport(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	want := dcmqiReport()
	// dcmqi generates the Tracking Unique Identifier it was not given.
	if len(report.Groups) != 2 || report.Groups[1].Tracking.UID == "" {
		t.Fatalf("groups: %+v", report.Groups)
	}
	want.Groups[1].Tracking.UID = report.Groups[1].Tracking.UID
	if report.SOPClassUID != want.SOPClassUID || report.StudyInstanceUID != want.StudyInstanceUID || report.Legacy ||
		report.Title != imagingMeasurementReportConcept || report.Language != want.Language ||
		!reflect.DeepEqual(report.Observers, want.Observers) || !reflect.DeepEqual(report.ProceduresReported, want.ProceduresReported) {
		t.Fatalf("report context: %+v", report)
	}
	if !reflect.DeepEqual(report.ImageLibrary, want.ImageLibrary) {
		t.Fatalf("image library\n%+v\nwant\n%+v", report.ImageLibrary, want.ImageLibrary)
	}
	if !reflect.DeepEqual(report.Groups, want.Groups) {
		t.Fatalf("groups\n%+v\nwant\n%+v", report.Groups, want.Groups)
	}
	rewritten, err := WriteMeasurementReport(report)
	if err != nil {
		t.Fatal(err)
	}
	round, err := ReadMeasurementReport(rewritten.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(round.Groups, want.Groups) || !reflect.DeepEqual(round.ImageLibrary, want.ImageLibrary) {
		t.Fatalf("rewritten\n%+v\nwant\n%+v", round, want)
	}
}
