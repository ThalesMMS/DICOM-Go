package roi

import (
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/codes/dcm"
	"github.com/ThalesMMS/dicom-go/codes/sct"
	"github.com/ThalesMMS/dicom-go/codes/ucum"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/sr"
)

type CPRLumenReportOptions struct {
	SOPInstanceUID, StudyInstanceUID, SeriesInstanceUID, FrameOfReferenceUID string
	ContentDate, ContentTime                                                 string
	ImageLibrary                                                             []sr.ImageReference
	ThresholdUnits                                                           sr.CodedEntry
}

func lumenPrivateCode(value, meaning string) sr.CodedEntry {
	return sr.CodedEntry{CodeValue: value, CodingSchemeDesignator: "99DICOMGO", CodeMeaning: meaning}
}

// MeasurementReport exports TID 1500 groups with standard vessel concepts,
// UCUM units, patient-space contour/centre evidence and explicit analysis and
// reference parameters. It does not label arbitrary reference selection as a
// clinically prescribed NASCET/WASID method. Invalid sections have a validity
// marker and arc position, but no fabricated zero area or stenosis percentage.
func (profile *CPRLumenProfile) MeasurementReport(opts CPRLumenReportOptions) (*sr.MeasurementReport, error) {
	if profile == nil || profile.ReferenceIndex < 0 || profile.ReferenceIndex >= len(profile.Sections) || !profile.Sections[profile.ReferenceIndex].Valid || opts.FrameOfReferenceUID == "" {
		return nil, ErrCPRLumenReference
	}
	ref := profile.Sections[profile.ReferenceIndex]
	if !(ref.AreaMM2 > 0) || !(ref.MinDiameterMM > 0) || !finiteLumen(ref.AreaMM2) || !finiteLumen(ref.MinDiameterMM) {
		return nil, ErrCPRLumenReference
	}
	mm, mm2, pct, none := sr.FromCode(ucum.Mm), sr.FromCode(ucum.SquareMillimeter), sr.FromCode(ucum.Percent), sr.FromCode(ucum.NoUnits)
	thresholdUnits := opts.ThresholdUnits
	if thresholdUnits.CodeValue == "" {
		thresholdUnits = none
	}
	metadata := sr.MeasurementGroup{Tracking: sr.TrackingIdentifier{UID: core.NameUID("dicom-go/roi/cpr-lumen", opts.SOPInstanceUID, "parameters"), Identifier: "CPR lumen analysis parameters"}, Finding: lumenPrivateCode("CPRLumen", "Threshold-connected transverse lumen")}
	for _, m := range []sr.ReportMeasurement{
		{ConceptName: lumenPrivateCode("Lower", "Lower source-modality threshold"), Value: profile.Options.ThresholdMin, Units: thresholdUnits},
		{ConceptName: lumenPrivateCode("Upper", "Upper source-modality threshold"), Value: profile.Options.ThresholdMax, Units: thresholdUnits},
		{ConceptName: lumenPrivateCode("ReferenceArc", "Explicit reference arc length"), Value: ref.ArcLengthMM, Units: mm},
		{ConceptName: lumenPrivateCode("ReferenceArea", "Reference lumen area"), Value: ref.AreaMM2, Units: mm2},
		{ConceptName: lumenPrivateCode("ReferenceMin", "Reference minimum Feret diameter"), Value: ref.MinDiameterMM, Units: mm},
		{ConceptName: lumenPrivateCode("PixelSpacing", "Transverse analysis pixel spacing"), Value: profile.Options.PixelSpacingMM, Units: mm},
		{ConceptName: lumenPrivateCode("SectionStep", "Requested arc sampling interval"), Value: profile.Options.SectionStepMM, Units: mm},
		{ConceptName: lumenPrivateCode("Radius", "Transverse analysis radius"), Value: profile.Options.RadiusMM, Units: mm},
		{ConceptName: lumenPrivateCode("Rotation", "Transverse basis rotation"), Value: profile.Options.RotationDegrees, Units: sr.FromCode(ucum.Degree)},
	} {
		if !finiteLumen(m.Value) {
			return nil, ErrCPRLumenInput
		}
		metadata.Measurements = append(metadata.Measurements, m)
	}
	report := &sr.MeasurementReport{SOPClassUID: sr.Comprehensive3DSRStorage, SOPInstanceUID: opts.SOPInstanceUID, StudyInstanceUID: opts.StudyInstanceUID, SeriesInstanceUID: opts.SeriesInstanceUID, FrameOfReferenceUID: opts.FrameOfReferenceUID, ContentDate: opts.ContentDate, ContentTime: opts.ContentTime, ProceduresReported: []sr.CodedEntry{lumenPrivateCode("CPRAnalysis", "CPR threshold lumen analysis")}, ImageLibrary: opts.ImageLibrary, Groups: []sr.MeasurementGroup{metadata}}
	for i, section := range profile.Sections {
		if !finiteLumen(section.ArcLengthMM) {
			return nil, ErrCPRLumenInput
		}
		point := sr.SpatialReference{FrameOfReferenceUID: opts.FrameOfReferenceUID, GraphicType: sr.GraphicTypePoint3D, Coordinates: []sr.Point3D{{X: section.Center.X, Y: section.Center.Y, Z: section.Center.Z}}}
		valid := 0.0
		if section.Valid {
			valid = 1
		}
		group := sr.MeasurementGroup{Tracking: sr.TrackingIdentifier{UID: core.NameUID("dicom-go/roi/cpr-lumen", opts.SOPInstanceUID, fmt.Sprint(i)), Identifier: fmt.Sprintf("CPR lumen at %.3f mm", section.ArcLengthMM)}, Finding: lumenPrivateCode("CPRLumen", "Threshold-connected transverse lumen"), Measurements: []sr.ReportMeasurement{
			{ConceptName: lumenPrivateCode("ArcLength", "Arc length from path start"), Value: section.ArcLengthMM, Units: mm, Spatial: point},
			{ConceptName: lumenPrivateCode("Valid", "Lumen section measurable"), Value: valid, Units: none},
		}}
		if section.Valid {
			if !(section.AreaMM2 > 0) || !(section.MinDiameterMM > 0) || section.MaxDiameterMM < section.MinDiameterMM {
				return nil, ErrCPRLumenInput
			}
			for _, value := range []float64{section.AreaMM2, section.MinDiameterMM, section.MaxDiameterMM} {
				if !finiteLumen(value) {
					return nil, ErrCPRLumenInput
				}
			}
			areaGeometry := point
			for _, contour := range section.Contours {
				if ClosedPolygonArea(contour) > 0 {
					areaGeometry = sr.SpatialReference{FrameOfReferenceUID: opts.FrameOfReferenceUID, GraphicType: sr.GraphicTypePolygon3D}
					for _, p := range contour {
						patient := section.Center.Add(section.Normal.Scale(p.X)).Add(section.Binormal.Scale(p.Y))
						areaGeometry.Coordinates = append(areaGeometry.Coordinates, sr.Point3D{X: patient.X, Y: patient.Y, Z: patient.Z})
					}
					if len(areaGeometry.Coordinates) > 0 {
						areaGeometry.Coordinates = append(areaGeometry.Coordinates, areaGeometry.Coordinates[0])
					}
					break
				}
			}
			group.Measurements = append(group.Measurements,
				sr.ReportMeasurement{ConceptName: sr.FromCode(dcm.VesselLumenCrossSectionalArea), Value: section.AreaMM2, Units: mm2, Spatial: areaGeometry},
				sr.ReportMeasurement{ConceptName: sr.FromCode(sct.VesselLumenDiameter), Derivation: sr.FromCode(sct.Minimum), Value: section.MinDiameterMM, Units: mm, Spatial: point},
				sr.ReportMeasurement{ConceptName: sr.FromCode(sct.VesselLumenDiameter), Derivation: sr.FromCode(sct.Maximum), Value: section.MaxDiameterMM, Units: mm, Spatial: point},
				sr.ReportMeasurement{ConceptName: sr.FromCode(sct.LumenAreaStenosis), Value: 100 * (1 - section.AreaMM2/ref.AreaMM2), Units: pct, Spatial: point},
				sr.ReportMeasurement{ConceptName: sr.FromCode(sct.LumenDiameterStenosis), Value: 100 * (1 - section.MinDiameterMM/ref.MinDiameterMM), Units: pct, Spatial: point},
			)
			for _, contour := range section.Contours {
				area := ClosedPolygonArea(contour)
				if area < 0 {
					group.Measurements = append(group.Measurements, sr.ReportMeasurement{ConceptName: lumenPrivateCode("ExcludedArea", "Excluded lumen hole area"), Value: math.Abs(area), Units: mm2})
				}
			}
		}
		report.Groups = append(report.Groups, group)
	}
	return report, nil
}
