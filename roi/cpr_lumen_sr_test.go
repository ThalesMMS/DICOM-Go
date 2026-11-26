package roi

import (
	"context"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/codes/sct"
	"github.com/ThalesMMS/dicom-go/codes/ucum"
	"github.com/ThalesMMS/dicom-go/sr"
)

func TestCPRLumenSRRoundTripPreservesUnitsReferenceAndGeometry(t *testing.T) {
	volume, path := lumenStenosisPhantom(t)
	profile, err := AnalyzeCPRLumen(context.Background(), volume, path, CPRLumenOptions{ThresholdMin: 250, ThresholdMax: 750, SectionStepMM: 2, RadiusMM: 9, ReferenceArcMM: 1.3})
	if err != nil {
		t.Fatal(err)
	}
	report, err := profile.MeasurementReport(CPRLumenReportOptions{SOPInstanceUID: "2.25.135001", StudyInstanceUID: "2.25.135002", SeriesInstanceUID: "2.25.135003", FrameOfReferenceUID: "2.25.135004", ContentDate: "20261014", ContentTime: "120000", ThresholdUnits: sr.FromCode(ucum.HounsfieldUnit)})
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
	if len(read.Groups) != len(profile.Sections)+1 {
		t.Fatal("lost sections", len(read.Groups))
	}
	for _, m := range read.Groups[0].Measurements {
		if m.ConceptName.CodeValue == "ReferenceArc" && (m.Value != 1.3 || m.Units.CodeValue != "mm") {
			t.Fatal("reference changed", m)
		}
		if m.ConceptName.CodeValue == "Lower" && m.Units.CodeValue != "[hnsf'U]" {
			t.Fatal("threshold units changed", m)
		}
	}
	for i, g := range read.Groups[1:] {
		section := profile.Sections[i]
		for _, m := range g.Measurements {
			if m.Spatial.GraphicType == sr.GraphicTypePolygon3D {
				if m.Spatial.FrameOfReferenceUID != "2.25.135004" || len(m.Spatial.Coordinates) < 4 || m.Spatial.Coordinates[0] != m.Spatial.Coordinates[len(m.Spatial.Coordinates)-1] {
					t.Fatal("lost closed patient contour", m.Spatial)
				}
			}
			if m.ConceptName.CodeValue == sct.LumenDiameterStenosis.Value && (m.Units.CodeValue != "%" || math.Abs(m.Value-section.DiameterReductionPercent) > 1e-8) {
				t.Fatal("diameter percentage changed", m)
			}
			if m.ConceptName.CodeValue == sct.VesselLumenDiameter.Value && (m.Units.CodeValue != "mm" || m.Derivation.CodeValue == "") {
				t.Fatal("diameter derivation lost", m)
			}
		}
	}
}
