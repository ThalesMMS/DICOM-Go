package calcium

import (
	"context"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/codes/dcm"
	"github.com/ThalesMMS/dicom-go/codes/sct"
	"github.com/ThalesMMS/dicom-go/sr"
)

func TestMeasurementReportWritesOneGroupPerVesselAndTotal(t *testing.T) {
	g, slices := phantom(3)
	analysis, err := Detect(context.Background(), g, SliceValues(slices), Options{MassCalibration: 0.8})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range phantomPlaques {
		if p.wantVessel == Unassigned {
			continue
		}
		id, ok := analysis.LesionAt(int(p.zStart/3), p.clickX, p.clickY)
		if !ok {
			t.Fatalf("no %s lesion", p.name)
		}
		if _, err := analysis.AssignGroup(id, p.wantVessel); err != nil {
			t.Fatal(err)
		}
	}
	score := analysis.Score()
	report, err := score.MeasurementReport(ReportOptions{
		SOPInstanceUID:    "2.25.1201001",
		StudyInstanceUID:  "2.25.1201002",
		SeriesInstanceUID: "2.25.1201003",
		ContentDate:       "20261007",
		ContentTime:       "120000",
		ImageLibrary: []sr.ImageReference{{
			StudyInstanceUID: "2.25.1201002", SeriesInstanceUID: "2.25.1201004",
			SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: "2.25.1201005",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	file, err := sr.WriteMeasurementReport(report)
	if err != nil {
		t.Fatal(err)
	}
	read, err := sr.ReadMeasurementReport(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.ProceduresReported) != 1 || read.ProceduresReported[0].CodeValue != sct.CardiacCTForCalciumScoring.Value {
		t.Fatalf("procedures reported = %+v", read.ProceduresReported)
	}
	if len(read.ImageLibrary) != 1 || read.ImageLibrary[0].SOPInstanceUID != "2.25.1201005" {
		t.Fatalf("image library = %+v", read.ImageLibrary)
	}
	wantSites := []string{
		sct.LeftMainCoronaryArtery.Value, sct.LeftAnteriorDescendingCoronaryArtery.Value,
		sct.CircumflexCoronaryArtery.Value, sct.RightCoronaryArtery.Value,
		sct.CoronaryArtery.Value, sct.CoronaryArtery.Value,
	}
	wantAgatston := []float64{1, 24, 0, 24, 1, 50}
	if len(read.Groups) != len(wantSites) {
		t.Fatalf("groups = %d, want %d", len(read.Groups), len(wantSites))
	}
	for i, group := range read.Groups {
		if group.Finding.CodeValue != dcm.Calcium.Value || len(group.FindingSites) != 1 || group.FindingSites[0].CodeValue != wantSites[i] {
			t.Errorf("group %d finding %+v sites %+v", i, group.Finding, group.FindingSites)
		}
		if group.Tracking.UID == "" || group.Tracking.Identifier == "" {
			t.Errorf("group %d tracking = %+v", i, group.Tracking)
		}
		values := map[string]float64{}
		for _, measurement := range group.Measurements {
			values[measurement.ConceptName.CodeValue] = measurement.Value
		}
		if math.Abs(values[sct.CoronaryArteryCalciumScore.Value]-wantAgatston[i]) > 1e-9 {
			t.Errorf("group %d Agatston = %v, want %v", i, values[sct.CoronaryArteryCalciumScore.Value], wantAgatston[i])
		}
		if _, ok := values[dcm.CalciumMass.Value]; !ok {
			t.Errorf("group %d has no calcium mass", i)
		}
		if i == len(read.Groups)-1 {
			if values[dcm.CalciumVolume.Value] != 48 || values[dcm.AgatstonScoreThreshold.Value] != 130 || values[dcm.CalciumScoringCalibration.Value] != 0.8 {
				t.Errorf("total group values = %v", values)
			}
		}
	}
	// Without calibration the report carries no mass.
	if err := analysis.SetMassCalibration(0); err != nil {
		t.Fatal(err)
	}
	groups, err := analysis.Score().MeasurementGroups("2.25.1201001")
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range groups {
		for _, measurement := range group.Measurements {
			if measurement.ConceptName.CodeValue == dcm.CalciumMass.Value || measurement.ConceptName.CodeValue == dcm.CalciumScoringCalibration.Value {
				t.Fatalf("uncalibrated report has %s", measurement.ConceptName.CodeMeaning)
			}
		}
	}
	if _, err := score.MeasurementReport(ReportOptions{}); err == nil {
		t.Fatal("a report without UIDs was built")
	}
}
