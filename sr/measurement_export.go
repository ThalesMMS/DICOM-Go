package sr

import (
	"errors"
	"fmt"
	"github.com/ThalesMMS/dicom-go/codes/dcm"
	"github.com/ThalesMMS/dicom-go/codes/sct"
	"github.com/ThalesMMS/dicom-go/codes/ucum"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
)

type MeasurementKind string

const (
	MeasurementKindLength MeasurementKind = "length"
	MeasurementKindAngle  MeasurementKind = "angle"
	MeasurementKindArea   MeasurementKind = "area"
	// MeasurementKindLongAxis and MeasurementKindShortAxis are the two
	// diameters of a bidimensional (RECIST/WHO) measurement, (103339001,
	// SCT, "Long Axis") and (103340004, SCT, "Short Axis") of CID 7470.
	MeasurementKindLongAxis  MeasurementKind = "long_axis"
	MeasurementKindShortAxis MeasurementKind = "short_axis"
	// MeasurementKindRealWorldValue is a value in the units of a Real World
	// Value Mapping, such as the mean ADC in an ROI. Units are required (the
	// mapping's Measurement Units Code Sequence); the concept defaults to
	// (246205007, SCT, "Quantity") and is normally the mapping's quantity.
	MeasurementKindRealWorldValue MeasurementKind = "real_world_value"
)

// cidGeneralROIMeasurementModifier is CID 7464, the Derivation values.
const cidGeneralROIMeasurementModifier = 7464

// MeanDerivation is (373098007, SCT, "Mean") of CID 7464.
func MeanDerivation() CodedEntry {
	return fromContextGroup(cidGeneralROIMeasurementModifier, sct.MeanValueOfPopulation)
}

// MinimumDerivation is (255605001, SCT, "Minimum") of CID 7464.
func MinimumDerivation() CodedEntry {
	return fromContextGroup(cidGeneralROIMeasurementModifier, sct.Minimum)
}

// MaximumDerivation is (56851009, SCT, "Maximum") of CID 7464.
func MaximumDerivation() CodedEntry {
	return fromContextGroup(cidGeneralROIMeasurementModifier, sct.Maximum)
}

// StandardDeviationDerivation is (386136009, SCT, "Standard Deviation")
// of CID 7464.
func StandardDeviationDerivation() CodedEntry {
	return fromContextGroup(cidGeneralROIMeasurementModifier, sct.StandardDeviation)
}

// LongAxisConcept is (103339001, SCT, "Long Axis") of CID 7470.
func LongAxisConcept() CodedEntry {
	return fromContextGroup(cidLinearMeasurement, sct.LongAxis)
}

// ShortAxisConcept is (103340004, SCT, "Short Axis") of CID 7470.
func ShortAxisConcept() CodedEntry {
	return fromContextGroup(cidLinearMeasurement, sct.ShortAxis)
}

var ErrMissingReference = errors.New("dicom/sr: missing measurement reference")

type ViewerMeasurement struct {
	Tracking    TrackingIdentifier
	Kind        MeasurementKind
	Label       string
	Value       float64
	ConceptName CodedEntry
	Units       CodedEntry
	// Derivation (CID 7464), such as MeanDerivation, is written as the
	// measurement's HAS CONCEPT MOD (121401, DCM, "Derivation").
	Derivation  CodedEntry
	SourceImage ImageReference
	Spatial     SpatialReference
	// GroupMeasurements are further measurements of the same measurement
	// group (TID 1501), such as the short axis that goes with a long axis;
	// their Tracking is ignored.
	GroupMeasurements []ViewerMeasurement
}

type MeasurementExportOptions struct {
	SOPClassUID         string
	SOPInstanceUID      string
	StudyInstanceUID    string
	SeriesInstanceUID   string
	FrameOfReferenceUID string
	ContentDate         string
	ContentTime         string
	Title               CodedEntry
	Measurements        []ViewerMeasurement
}

// FromMeasurements maps viewer-level measurement primitives to the measurement
// report model used by WriteMeasurementReport. Each measurement must carry a
// complete source image reference so the resulting SR NUM item can be traced
// back to image evidence.
func FromMeasurements(opts MeasurementExportOptions) (*MeasurementReport, error) {
	if strings.TrimSpace(opts.SOPInstanceUID) == "" {
		return nil, fmt.Errorf("%w: SOP Instance UID is required", ErrInvalidMeasurementReport)
	}
	if strings.TrimSpace(opts.StudyInstanceUID) == "" || strings.TrimSpace(opts.SeriesInstanceUID) == "" {
		return nil, fmt.Errorf("%w: study and series instance UIDs are required", ErrMissingReference)
	}
	report := &MeasurementReport{
		SOPClassUID:         firstMeasurementNonEmpty(opts.SOPClassUID, Comprehensive3DSRStorage),
		SOPInstanceUID:      strings.TrimSpace(opts.SOPInstanceUID),
		StudyInstanceUID:    strings.TrimSpace(opts.StudyInstanceUID),
		SeriesInstanceUID:   strings.TrimSpace(opts.SeriesInstanceUID),
		FrameOfReferenceUID: strings.TrimSpace(opts.FrameOfReferenceUID),
		ContentDate:         strings.TrimSpace(opts.ContentDate),
		ContentTime:         strings.TrimSpace(opts.ContentTime),
		Title:               opts.Title,
		Groups:              make([]MeasurementGroup, 0, len(opts.Measurements)),
	}
	for index, measurement := range opts.Measurements {
		group, err := measurementGroupFromViewer(report.SOPInstanceUID, index, measurement)
		if err != nil {
			return nil, err
		}
		report.Groups = append(report.Groups, group)
	}
	return report, nil
}

func measurementGroupFromViewer(reportUID string, index int, measurement ViewerMeasurement) (MeasurementGroup, error) {
	group := MeasurementGroup{Tracking: trackingForViewerMeasurement(reportUID, index, measurement)}
	for partIndex, part := range append([]ViewerMeasurement{measurement}, measurement.GroupMeasurements...) {
		if partIndex > 0 && len(part.GroupMeasurements) > 0 {
			return MeasurementGroup{}, fmt.Errorf("%w: group measurements cannot nest", ErrInvalidMeasurementReport)
		}
		if err := validateMeasurementSourceImage(part.SourceImage); err != nil {
			return MeasurementGroup{}, err
		}
		if err := validateMeasurementSpatialReference(part.Spatial); err != nil {
			return MeasurementGroup{}, err
		}
		concept, units, err := conceptAndUnitsForMeasurement(part)
		if err != nil {
			return MeasurementGroup{}, err
		}
		group.Measurements = append(group.Measurements, ReportMeasurement{
			ConceptName: concept,
			Value:       part.Value,
			Units:       units,
			Derivation:  part.Derivation,
			Image:       normalizedImageReference(part.SourceImage),
			Spatial:     normalizedSpatialReference(part.Spatial),
		})
	}
	return group, nil
}

func conceptAndUnitsForMeasurement(measurement ViewerMeasurement) (CodedEntry, CodedEntry, error) {
	concept, units, ok := defaultConceptAndUnits(measurement.Kind)
	if !ok {
		return CodedEntry{}, CodedEntry{}, fmt.Errorf("%w: unsupported measurement kind %q", ErrInvalidMeasurementReport, measurement.Kind)
	}
	if !measurement.ConceptName.Empty() {
		concept = measurement.ConceptName
	}
	if !measurement.Units.Empty() {
		units = measurement.Units
	}
	if units.Empty() {
		return CodedEntry{}, CodedEntry{}, fmt.Errorf("%w: measurement kind %q needs units", ErrInvalidMeasurementReport, measurement.Kind)
	}
	return concept, units, nil
}

func defaultConceptAndUnits(kind MeasurementKind) (CodedEntry, CodedEntry, bool) {
	switch kind {
	case MeasurementKindLength:
		return fromContextGroup(cidLinearMeasurement, dcm.Distance), MillimeterUnit(), true
	case MeasurementKindAngle:
		// (1483009, SCT, "Angle"); DCM 121207, used here before, is Height.
		return FromCode(sct.Angle), DegreeUnit(), true
	case MeasurementKindArea:
		return fromContextGroup(cidAreaMeasurement, sct.Area), SquareMillimeterUnit(), true
	case MeasurementKindLongAxis:
		return LongAxisConcept(), MillimeterUnit(), true
	case MeasurementKindShortAxis:
		return ShortAxisConcept(), MillimeterUnit(), true
	case MeasurementKindRealWorldValue:
		return FromCode(sct.Quantity), CodedEntry{}, true
	default:
		return CodedEntry{}, CodedEntry{}, false
	}
}

func validateMeasurementSourceImage(ref ImageReference) error {
	if strings.TrimSpace(ref.SOPClassUID) == "" || strings.TrimSpace(ref.SOPInstanceUID) == "" {
		return fmt.Errorf("%w: source image SOP class and instance UIDs are required", ErrMissingReference)
	}
	for _, frame := range ref.Frames {
		if frame <= 0 {
			return fmt.Errorf("%w: referenced frame numbers must be one-based", ErrMissingReference)
		}
	}
	return nil
}

func validateMeasurementSpatialReference(ref SpatialReference) error {
	if !hasSpatialReference(ref) {
		return nil
	}
	if strings.TrimSpace(ref.FrameOfReferenceUID) == "" {
		return fmt.Errorf("%w: SCOORD3D requires referenced frame of reference UID", ErrMissingReference)
	}
	if strings.TrimSpace(ref.GraphicType) == "" || len(ref.Coordinates) == 0 {
		return fmt.Errorf("%w: SCOORD3D requires graphic type and coordinates", ErrMissingReference)
	}
	if !validSpatialPointCount(strings.TrimSpace(ref.GraphicType), ref.Coordinates) {
		return fmt.Errorf("%w: SCOORD3D %s cannot have %d points", ErrInvalidMeasurementReport, ref.GraphicType, len(ref.Coordinates))
	}
	return nil
}

// validSpatialPointCount checks the point count of the SCOORD3D graphic
// types that FromMeasurements writes; other types are passed through.
func validSpatialPointCount(graphicType string, points []Point3D) bool {
	switch graphicType {
	case GraphicTypePoint3D:
		return len(points) == 1
	case GraphicTypePolyline3D, GraphicTypeMultipoint3D:
		return len(points) >= 1
	case GraphicTypePolygon3D:
		return len(points) >= 4 && points[0] == points[len(points)-1]
	default:
		return true
	}
}

// SpatialPoints returns the distinct points of a SCOORD3D with graphic type
// POINT, MULTIPOINT, POLYLINE or POLYGON (without the repeated closing point
// of a polygon). Reports written before the graphic types were chosen by
// shape (#1074) listed every point under POINT; those are accepted too.
func SpatialPoints(ref SpatialReference) ([]Point3D, bool) {
	points := ref.Coordinates
	if len(points) == 0 {
		return nil, false
	}
	switch strings.TrimSpace(ref.GraphicType) {
	case GraphicTypePoint3D, GraphicTypeMultipoint3D, GraphicTypePolyline3D:
	case GraphicTypePolygon3D:
		if len(points) > 1 && points[0] == points[len(points)-1] {
			points = points[:len(points)-1]
		}
	default:
		return nil, false
	}
	return append([]Point3D(nil), points...), true
}

func trackingForViewerMeasurement(reportUID string, index int, measurement ViewerMeasurement) TrackingIdentifier {
	tracking := measurement.Tracking
	tracking.UID = strings.TrimSpace(tracking.UID)
	tracking.Identifier = firstMeasurementNonEmpty(tracking.Identifier, measurement.Label, defaultMeasurementIdentifier(measurement.Kind, index))
	if tracking.UID == "" {
		tracking.UID = derivedTrackingUID(strings.TrimSpace(reportUID), index+1)
	}
	return tracking
}

// derivedTrackingUID appends the measurement ordinal to the report UID when
// the result is a valid UID (PS3.5 9.1). A report UID that is invalid or too
// long to extend yields a deterministic 2.25 UID derived from both instead,
// so the same report always gets the same tracking UIDs.
func derivedTrackingUID(reportUID string, ordinal int) string {
	if candidate := fmt.Sprintf("%s.%d", reportUID, ordinal); core.IsValidUID(candidate) {
		return candidate
	}
	return core.NameUID("dicom-go/sr tracking", reportUID, strconv.Itoa(ordinal))
}

func defaultMeasurementIdentifier(kind MeasurementKind, index int) string {
	if kind == "" {
		return fmt.Sprintf("Measurement %d", index+1)
	}
	return fmt.Sprintf("Measurement %d %s", index+1, kind)
}

func normalizedImageReference(ref ImageReference) ImageReference {
	out := ImageReference{
		StudyInstanceUID:  strings.TrimSpace(ref.StudyInstanceUID),
		SeriesInstanceUID: strings.TrimSpace(ref.SeriesInstanceUID),
		SOPClassUID:       strings.TrimSpace(ref.SOPClassUID),
		SOPInstanceUID:    strings.TrimSpace(ref.SOPInstanceUID),
	}
	if len(ref.Frames) > 0 {
		out.Frames = append([]int(nil), ref.Frames...)
	}
	return out
}

func normalizedSpatialReference(ref SpatialReference) SpatialReference {
	out := SpatialReference{
		FrameOfReferenceUID: strings.TrimSpace(ref.FrameOfReferenceUID),
		GraphicType:         strings.TrimSpace(ref.GraphicType),
		FiducialUID:         strings.TrimSpace(ref.FiducialUID),
	}
	if len(ref.Coordinates) > 0 {
		out.Coordinates = append([]Point3D(nil), ref.Coordinates...)
	}
	return out
}

func MillimeterUnit() CodedEntry {
	return FromCode(ucum.Mm)
}

func DegreeUnit() CodedEntry {
	return FromCode(ucum.Degree)
}

func SquareMillimeterUnit() CodedEntry {
	return FromCode(ucum.SquareMillimeter)
}

func firstMeasurementNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
