package calcium

import (
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/codes/dcm"
	"github.com/ThalesMMS/dicom-go/codes/sct"
	"github.com/ThalesMMS/dicom-go/codes/ucum"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/sr"
)

// ReportOptions identify the Measurement Report of a score.
type ReportOptions struct {
	SOPClassUID         string
	SOPInstanceUID      string
	StudyInstanceUID    string
	SeriesInstanceUID   string
	FrameOfReferenceUID string
	ContentDate         string
	ContentTime         string
	// ImageLibrary lists the scored source images (evidence); nil lists
	// none.
	ImageLibrary []sr.ImageReference
}

// Concepts of the calcium measurements.
var (
	agatstonConcept    = sr.FromCode(sct.CoronaryArteryCalciumScore)
	volumeConcept      = sr.FromCode(dcm.CalciumVolume)
	massConcept        = sr.FromCode(dcm.CalciumMass)
	thresholdConcept   = sr.FromCode(dcm.AgatstonScoreThreshold)
	massThreshold      = sr.FromCode(dcm.CalciumMassThreshold)
	calibrationConcept = sr.FromCode(dcm.CalciumScoringCalibration)
	calciumFinding     = sr.FromCode(dcm.Calcium)
)

// TrackingIdentifier is the Tracking Identifier of a vessel's measurement
// group.
func TrackingIdentifier(vessel Vessel) string {
	return "Coronary calcium: " + vessel.Name()
}

// MeasurementGroups returns one TID 1501 measurement group per vessel, then
// one for the total: the finding Calcium at the vessel's site with the
// Agatston score (Coronary artery calcium score, no units), the volume score
// (Calcium Volume, mm3) and, when calibrated, the mass score (Calcium Mass,
// mg). The total group also records the Agatston Score Threshold and, when
// calibrated, the Calcium Mass Threshold and the Calcium Scoring Calibration.
// reportUID derives the tracking UIDs.
func (s Score) MeasurementGroups(reportUID string) ([]sr.MeasurementGroup, error) {
	for _, value := range []float64{s.ThresholdHU, s.MassCalibration} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("%w: non-finite score parameter", ErrInvalidOptions)
		}
	}
	scores := append(append([]VesselScore(nil), s.Vessels...), s.Total)
	groups := make([]sr.MeasurementGroup, 0, len(scores))
	for _, score := range scores {
		if math.IsNaN(score.Agatston) || math.IsNaN(score.VolumeMM3) || math.IsNaN(score.MassMG) {
			return nil, fmt.Errorf("%w: non-finite %s score", ErrInvalidOptions, score.Vessel)
		}
		group := sr.MeasurementGroup{
			Tracking: sr.TrackingIdentifier{
				Identifier: TrackingIdentifier(score.Vessel),
				UID:        core.NameUID("dicom-go/roi/calcium tracking", reportUID, string(score.Vessel)),
			},
			Finding:      calciumFinding,
			FindingSites: []sr.CodedEntry{sr.FromCode(score.Vessel.Site())},
			Measurements: []sr.ReportMeasurement{
				{ConceptName: agatstonConcept, Value: score.Agatston, Units: sr.FromCode(ucum.NoUnits)},
				{ConceptName: volumeConcept, Value: score.VolumeMM3, Units: sr.FromCode(ucum.CubicMillimeter)},
			},
		}
		if s.MassCalibrated {
			group.Measurements = append(group.Measurements, sr.ReportMeasurement{ConceptName: massConcept, Value: score.MassMG, Units: sr.FromCode(ucum.Mg)})
		}
		if score.Vessel == Total {
			hu := sr.FromCode(ucum.HounsfieldUnit)
			group.Measurements = append(group.Measurements, sr.ReportMeasurement{ConceptName: thresholdConcept, Value: s.ThresholdHU, Units: hu})
			if s.MassCalibrated {
				group.Measurements = append(group.Measurements,
					sr.ReportMeasurement{ConceptName: massThreshold, Value: s.ThresholdHU, Units: hu},
					sr.ReportMeasurement{ConceptName: calibrationConcept, Value: s.MassCalibration, Units: sr.FromCode(ucum.MgPerHnsfUCm3)},
				)
			}
		}
		groups = append(groups, group)
	}
	return groups, nil
}

// MeasurementReport returns the score as a TID 1500 Measurement Report
// (Comprehensive 3D SR unless opts names another SOP Class) whose procedure
// reported is "Cardiac CT for calcium scoring"; write it with
// sr.WriteMeasurementReport.
func (s Score) MeasurementReport(opts ReportOptions) (*sr.MeasurementReport, error) {
	if opts.SOPInstanceUID == "" || opts.StudyInstanceUID == "" {
		return nil, fmt.Errorf("%w: the report needs its SOP Instance and Study Instance UIDs", sr.ErrInvalidMeasurementReport)
	}
	groups, err := s.MeasurementGroups(opts.SOPInstanceUID)
	if err != nil {
		return nil, err
	}
	library := opts.ImageLibrary
	if library == nil {
		library = []sr.ImageReference{}
	}
	sopClass := opts.SOPClassUID
	if sopClass == "" {
		sopClass = sr.Comprehensive3DSRStorage
	}
	return &sr.MeasurementReport{
		SOPClassUID:         sopClass,
		SOPInstanceUID:      opts.SOPInstanceUID,
		StudyInstanceUID:    opts.StudyInstanceUID,
		SeriesInstanceUID:   opts.SeriesInstanceUID,
		FrameOfReferenceUID: opts.FrameOfReferenceUID,
		ContentDate:         opts.ContentDate,
		ContentTime:         opts.ContentTime,
		ProceduresReported:  []sr.CodedEntry{sr.FromCode(sct.CardiacCTForCalciumScoring)},
		ImageLibrary:        library,
		Groups:              groups,
	}, nil
}
