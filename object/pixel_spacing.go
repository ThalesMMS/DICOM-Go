package object

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
)

// SpacingValues tells how a two-valued spacing attribute (Pixel Spacing and
// its relatives, VM 2: row spacing, then column spacing) was read.
type SpacingValues string

const (
	// SpacingValuesAbsent: the attribute is missing or empty.
	SpacingValuesAbsent SpacingValues = ""
	// SpacingValuesPair: two values, as the standard requires.
	SpacingValuesPair SpacingValues = "pair"
	// SpacingValuesSingle: one value, possibly followed by an empty one
	// (0.5\ with a trailing backslash), used for both rows and columns as
	// ITK does.
	SpacingValuesSingle SpacingValues = "single"
	// SpacingValuesExtra: more than two values; the first two are used.
	SpacingValuesExtra SpacingValues = "extra"
)

// ErrMalformedSpacing reports a spacing attribute whose values are not
// finite positive decimal numbers ("N/A", "0.5abc", "0", an empty first
// value). Such a spacing is never replaced by a default silently.
var ErrMalformedSpacing = errors.New("dicom: malformed spacing")

// SpacingPair reads a two-valued spacing attribute such as Pixel Spacing
// (0028,0030) or Imager Pixel Spacing (0018,1164): row and column spacing in
// mm and how its values were read. One value (or one and an empty second)
// is isotropic, more than two use the first two, and a value that is not a
// finite positive number fails with ErrMalformedSpacing. A missing or empty
// attribute returns SpacingValuesAbsent without error.
func (o *Object) SpacingPair(tag core.Tag) (row, column float64, values SpacingValues, err error) {
	if o == nil {
		return 0, 0, SpacingValuesAbsent, nil
	}
	element, ok := o.Get(tag)
	if !ok {
		return 0, 0, SpacingValuesAbsent, nil
	}
	if element.VR() != core.VRDS {
		return 0, 0, SpacingValuesAbsent, fmt.Errorf("%w: %s has VR %s, want DS", ErrMalformedSpacing, tag, element.VR())
	}
	texts, err := o.LookupStrings(tag)
	if err != nil {
		return 0, 0, SpacingValuesAbsent, fmt.Errorf("%w: %s: %v", ErrMalformedSpacing, tag, err)
	}
	return ParseSpacingPair(texts)
}

// ParseSpacingPair applies SpacingPair's rules to the backslash-separated
// values of a DS attribute.
func ParseSpacingPair(texts []string) (row, column float64, values SpacingValues, err error) {
	cleaned := make([]string, len(texts))
	for i, text := range texts {
		cleaned[i] = core.CleanTextValue(core.VRDS, text)
	}
	for len(cleaned) > 0 && cleaned[len(cleaned)-1] == "" {
		cleaned = cleaned[:len(cleaned)-1]
	}
	parsed := make([]float64, 0, 2)
	for i, text := range cleaned {
		if i == 2 {
			break
		}
		value, parseErr := strconv.ParseFloat(text, 64)
		if parseErr != nil || !(value > 0) || math.IsInf(value, 0) {
			return 0, 0, SpacingValuesAbsent, fmt.Errorf("%w: value %d is %q", ErrMalformedSpacing, i+1, text)
		}
		parsed = append(parsed, value)
	}
	switch len(cleaned) {
	case 0:
		return 0, 0, SpacingValuesAbsent, nil
	case 1:
		return parsed[0], parsed[0], SpacingValuesSingle, nil
	case 2:
		return parsed[0], parsed[1], SpacingValuesPair, nil
	default:
		return parsed[0], parsed[1], SpacingValuesExtra, nil
	}
}

// PixelSpacingSemantic tells in which plane a resolved pixel spacing
// measures distances (PS3.3 10.7.1.3).
type PixelSpacingSemantic string

const (
	// SpacingNone: no spacing attribute applies.
	SpacingNone PixelSpacingSemantic = ""
	// SpacingPatient: Pixel Spacing of a cross-sectional image (CT, MR, PET,
	// NM, enhanced Pixel Measures), a distance in the patient.
	SpacingPatient PixelSpacingSemantic = "patient"
	// SpacingDetector: Imager Pixel Spacing of a projection image, a
	// distance at the front plane of the detector. Magnification makes
	// patient distances smaller; it is never corrected silently.
	SpacingDetector PixelSpacingSemantic = "detector"
	// SpacingCalibrated: Pixel Spacing of a projection or secondary capture
	// image with a Pixel Spacing Calibration Type (geometry or fiducial).
	SpacingCalibrated PixelSpacingSemantic = "calibrated"
	// SpacingRTImagePlane: RT Image Plane Pixel Spacing, at the RT image
	// receptor plane.
	SpacingRTImagePlane PixelSpacingSemantic = "rt-image-plane"
	// SpacingScanned: Nominal Scanned Pixel Spacing of digitized film or
	// paper, a distance on the scanned medium.
	SpacingScanned PixelSpacingSemantic = "scanned"
	// SpacingUnknown: Pixel Spacing whose plane the IOD leaves undefined, for
	// example a projection image without calibration type whose value differs
	// from Imager Pixel Spacing.
	SpacingUnknown PixelSpacingSemantic = "unknown"
)

// PixelSpacingResolution is the spacing that applies to a frame, where it
// came from and what it measures.
type PixelSpacingResolution struct {
	// RowSpacing and ColumnSpacing are in mm: the distance between adjacent
	// rows and between adjacent columns, in DICOM value order.
	RowSpacing    float64
	ColumnSpacing float64
	// Tag is the attribute that supplied the values and Source where it was
	// read (FunctionalGroupNone for the top level).
	Tag      core.Tag
	Source   FunctionalGroupSource
	Semantic PixelSpacingSemantic
	// Values tells whether the attribute had the two values it should, or
	// one (isotropic) or more than two (see SpacingPair).
	Values SpacingValues
	// CalibrationType and CalibrationDescription are Pixel Spacing
	// Calibration Type (0028,0A02) and Description (0028,0A04).
	CalibrationType        string
	CalibrationDescription string
	// MagnificationFactor is Estimated Radiographic Magnification Factor
	// (0018,1114), zero when absent. It is reported, never applied; see
	// EstimatedPatientSpacing.
	MagnificationFactor float64
}

// Valid reports whether a spacing was resolved.
func (r PixelSpacingResolution) Valid() bool {
	return r.Semantic != SpacingNone && r.RowSpacing > 0 && r.ColumnSpacing > 0
}

// EstimatedPatientSpacing divides a detector spacing by the Estimated
// Radiographic Magnification Factor, the explicit estimate of distances at
// the isocenter or object plane that callers may offer on request.
func (r PixelSpacingResolution) EstimatedPatientSpacing() (row, column float64, ok bool) {
	if r.Semantic != SpacingDetector || r.MagnificationFactor <= 0 || !r.Valid() {
		return 0, 0, false
	}
	return r.RowSpacing / r.MagnificationFactor, r.ColumnSpacing / r.MagnificationFactor, true
}

var (
	tagImagerPixelSpacing              = core.NewTag(0x0018, 0x1164)
	tagNominalScannedPixelSpacing      = core.NewTag(0x0018, 0x2010)
	tagRTImagePlanePixelSpacing        = core.NewTag(0x3002, 0x0011)
	tagPixelSpacingCalibrationType     = core.NewTag(0x0028, 0x0A02)
	tagPixelSpacingCalibrationDesc     = core.NewTag(0x0028, 0x0A04)
	tagEstimatedMagnificationFactor    = core.NewTag(0x0018, 0x1114)
	tagXRayFramePixelDataPropertiesSeq = core.NewTag(0x0028, 0x9443)
	tagModality                        = core.NewTag(0x0008, 0x0060)
	tagSOPClassUIDSpacing              = core.NewTag(0x0008, 0x0016)
)

type spacingFamily int

const (
	familyOther spacingFamily = iota
	familyCrossSectional
	familyProjection
	familyRTImage
	familySecondaryCapture
)

// PixelSpacingAt resolves the spacing for the zero-based frameIndex by IOD
// family, from the modality and SOP Class:
//
//   - cross-sectional (CT, MR, PT, NM and enhanced objects): Pixel Measures
//     of the frame's functional groups, then Pixel Spacing, in the patient;
//   - projection (CR, DX, MG, IO, PX, XA, RF): Pixel Spacing with a
//     calibration type is calibrated; Pixel Spacing equal to Imager Pixel
//     Spacing is the detector spacing; other Pixel Spacing is unknown;
//     otherwise Imager Pixel Spacing (or that of the X-Ray Frame Pixel Data
//     Properties functional group), at the detector;
//   - RT Image: RT Image Plane Pixel Spacing;
//   - secondary capture: calibrated Pixel Spacing, then Nominal Scanned Pixel
//     Spacing, then Pixel Spacing of unknown plane;
//   - other modalities: Pixel Spacing of unknown plane.
//
// Ultrasound regions are resolved by package ultrasound.
func (o *Object) PixelSpacingAt(frameIndex int) PixelSpacingResolution {
	if o == nil {
		return PixelSpacingResolution{}
	}
	calibrationType, _ := o.GetString(tagPixelSpacingCalibrationType)
	calibrationType = strings.ToUpper(strings.TrimSpace(calibrationType))
	description, _ := o.GetString(tagPixelSpacingCalibrationDesc)
	magnification, _ := o.GetFloat(tagEstimatedMagnificationFactor)
	base := PixelSpacingResolution{
		CalibrationType:        calibrationType,
		CalibrationDescription: strings.TrimSpace(description),
	}
	if magnification > 0 {
		base.MagnificationFactor = magnification
	}
	with := func(values *spacingValue, tag core.Tag, source FunctionalGroupSource, semantic PixelSpacingSemantic) PixelSpacingResolution {
		out := base
		out.RowSpacing, out.ColumnSpacing, out.Values = values.row, values.column, values.values
		out.Tag, out.Source, out.Semantic = tag, source, semantic
		return out
	}
	pixelSpacing, psSource := o.functionalSpacing(frameIndex, tagPixelMeasuresSequence, tagPixelSpacing)

	switch o.spacingFamily() {
	case familyCrossSectional:
		if pixelSpacing != nil {
			return with(pixelSpacing, tagPixelSpacing, psSource, SpacingPatient)
		}
	case familyProjection:
		imager, imagerSource := o.functionalSpacing(frameIndex, tagXRayFramePixelDataPropertiesSeq, tagImagerPixelSpacing)
		switch {
		case pixelSpacing != nil && calibrationType != "":
			return with(pixelSpacing, tagPixelSpacing, psSource, SpacingCalibrated)
		case pixelSpacing != nil && imager != nil && sameSpacing(pixelSpacing, imager):
			return with(pixelSpacing, tagPixelSpacing, psSource, SpacingDetector)
		case pixelSpacing != nil:
			return with(pixelSpacing, tagPixelSpacing, psSource, SpacingUnknown)
		case imager != nil:
			return with(imager, tagImagerPixelSpacing, imagerSource, SpacingDetector)
		}
	case familyRTImage:
		if values := positiveSpacing(o, tagRTImagePlanePixelSpacing); values != nil {
			return with(values, tagRTImagePlanePixelSpacing, FunctionalGroupNone, SpacingRTImagePlane)
		}
	case familySecondaryCapture:
		switch scanned := positiveSpacing(o, tagNominalScannedPixelSpacing); {
		case pixelSpacing != nil && calibrationType != "":
			return with(pixelSpacing, tagPixelSpacing, psSource, SpacingCalibrated)
		case scanned != nil:
			return with(scanned, tagNominalScannedPixelSpacing, FunctionalGroupNone, SpacingScanned)
		}
	}
	if pixelSpacing != nil {
		return with(pixelSpacing, tagPixelSpacing, psSource, SpacingUnknown)
	}
	return base
}

// PixelSpacingAt resolves the dataset's spacing for frameIndex.
func (f *File) PixelSpacingAt(frameIndex int) PixelSpacingResolution {
	if f == nil {
		return PixelSpacingResolution{}
	}
	return f.Dataset.PixelSpacingAt(frameIndex)
}

// functionalSpacing reads a two-valued spacing from the frame's functional
// group macro, falling back to the top level.
func (o *Object) functionalSpacing(frameIndex int, macro, tag core.Tag) (*spacingValue, FunctionalGroupSource) {
	if item, source, _ := o.FunctionalGroupItem(frameIndex, macro); item != nil {
		if values := positiveSpacing(item, tag); values != nil {
			return values, source
		}
	}
	return positiveSpacing(o, tag), FunctionalGroupNone
}

// spacingValue is a usable spacing read by SpacingPair.
type spacingValue struct {
	row, column float64
	values      SpacingValues
}

// positiveSpacing is the spacing of tag, or nil when it is absent or
// malformed.
func positiveSpacing(o *Object, tag core.Tag) *spacingValue {
	row, column, values, err := o.SpacingPair(tag)
	if err != nil || values == SpacingValuesAbsent {
		return nil
	}
	return &spacingValue{row: row, column: column, values: values}
}

func sameSpacing(a, b *spacingValue) bool {
	const tolerance = 1e-6
	return abs(a.row-b.row) <= tolerance && abs(a.column-b.column) <= tolerance
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func (o *Object) spacingFamily() spacingFamily {
	sop, _ := o.GetString(tagSOPClassUIDSpacing)
	sop = strings.TrimRight(strings.TrimSpace(sop), "\x00")
	switch {
	case sop == "1.2.840.10008.5.1.4.1.1.481.1":
		return familyRTImage
	case sop == "1.2.840.10008.5.1.4.1.1.7" || strings.HasPrefix(sop, "1.2.840.10008.5.1.4.1.1.7."):
		return familySecondaryCapture
	case strings.HasPrefix(sop, "1.2.840.10008.5.1.4.1.1.12."): // (Enhanced) XA and XRF
		return familyProjection
	}
	modality, _ := o.GetString(tagModality)
	switch strings.ToUpper(strings.TrimSpace(modality)) {
	case "CT", "MR", "PT", "NM":
		return familyCrossSectional
	case "CR", "DX", "MG", "IO", "PX", "XA", "RF", "RG", "DF":
		return familyProjection
	case "RTIMAGE":
		return familyRTImage
	}
	if o.HasFunctionalGroups() {
		return familyCrossSectional
	}
	return familyOther
}
