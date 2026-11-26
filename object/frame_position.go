package object

import (
	"encoding/binary"
	"math"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
)

// GeometrySource names where a FrameGeometry position or orientation came
// from.
type GeometrySource string

const (
	// GeometrySourceAbsent: the value is missing.
	GeometrySourceAbsent GeometrySource = ""
	// GeometrySourceImagePlane: top-level Image Plane attributes
	// (PS3.3 C.7.6.2).
	GeometrySourceImagePlane GeometrySource = "image-plane"
	// GeometrySourceSharedFunctionalGroups and
	// GeometrySourcePerFrameFunctionalGroups: the Plane Position (Patient) or
	// Plane Orientation (Patient) functional group (PS3.3 C.7.6.16).
	GeometrySourceSharedFunctionalGroups   GeometrySource = "shared-functional-groups"
	GeometrySourcePerFrameFunctionalGroups GeometrySource = "per-frame-functional-groups"
	// GeometrySourceNMDetector: the frame's item of the Detector Information
	// Sequence (0054,0022) of the NM Detector Module (PS3.3 C.8.4.11).
	GeometrySourceNMDetector GeometrySource = "nm-detector-information"
	// GeometrySourceNMSliceVector: an NM reconstructed slice placed from its
	// Slice Vector (0054,0080) number and Spacing Between Slices.
	GeometrySourceNMSliceVector GeometrySource = "nm-slice-vector"
	// GeometrySourceLegacyFrameIncrement: a legacy multi-frame frame placed
	// from the top-level position by the opt-in
	// FrameGeometryOptions.DeriveLegacyFramePositions recovery.
	GeometrySourceLegacyFrameIncrement GeometrySource = "legacy-frame-increment"
)

// FrameGeometryFinding is a stable, PHI-free code reported in
// FrameGeometry.Findings.
type FrameGeometryFinding string

const (
	// FindingNegativeSpacingBetweenSlices: Spacing Between Slices is
	// negative; slices were placed against the slice normal.
	FindingNegativeSpacingBetweenSlices FrameGeometryFinding = "negative-spacing-between-slices"
	// FindingSpacingBetweenSlicesMissing: no usable (finite, non-zero)
	// Spacing Between Slices, so slices were not placed.
	FindingSpacingBetweenSlicesMissing FrameGeometryFinding = "spacing-between-slices-missing"
	// FindingSliceThicknessAsSpacing: the opt-in
	// FrameGeometryOptions.SliceThicknessAsSpacing used Slice Thickness as
	// the step.
	FindingSliceThicknessAsSpacing FrameGeometryFinding = "slice-thickness-as-spacing"
	// FindingSliceVectorMissing: an NM reconstruction has no Slice Vector
	// value for the frame.
	FindingSliceVectorMissing FrameGeometryFinding = "slice-vector-missing"
	// FindingSliceVectorOutOfRange: a Slice Vector value is below 1 or above
	// Number of Slices (0054,0081).
	FindingSliceVectorOutOfRange FrameGeometryFinding = "slice-vector-out-of-range"
	// FindingFrameOrderAsSliceIndex: the opt-in legacy recovery used frame
	// order for an NM reconstruction without Slice Vector.
	FindingFrameOrderAsSliceIndex FrameGeometryFinding = "frame-order-as-slice-index"
	// FindingNMDetectorAmbiguous: several Detector Information items and no
	// Detector Vector (0054,0020) value selecting one.
	FindingNMDetectorAmbiguous FrameGeometryFinding = "nm-detector-ambiguous"
	// FindingNMDetectorGeometryMissing: neither the detector item nor the top
	// level carries the Image Position/Orientation (Patient) to place slices.
	FindingNMDetectorGeometryMissing FrameGeometryFinding = "nm-detector-geometry-missing"
	// FindingFrameIncrementNotSpatial: the legacy recovery was requested, but
	// the Frame Increment Pointer (0028,0009) names a non-spatial attribute
	// (for example Frame Time), so frames are not slices.
	FindingFrameIncrementNotSpatial FrameGeometryFinding = "frame-increment-not-spatial"
	// FindingSliceLocationVectorInvalid: Slice Location Vector (0018,2005)
	// does not hold one finite value per frame.
	FindingSliceLocationVectorInvalid FrameGeometryFinding = "slice-location-vector-invalid"
)

// FrameGeometryOptions selects opt-in geometry recoveries. The zero value
// applies only the standard rules.
type FrameGeometryOptions struct {
	// DeriveLegacyFramePositions places the frames of a legacy multi-frame
	// image (no Plane Position functional group) that carries one top-level
	// Image Position and Orientation (Patient). When the Frame Increment
	// Pointer is absent, frame k lies k steps along the slice normal, the
	// step being Spacing Between Slices (sign kept), else Slice Thickness;
	// when it names Slice Location Vector (0018,2005) only, frame k lies at
	// its value minus the first frame's. Any other pointer (Frame Time,
	// Frame Time Vector, ...) marks the frames as non-spatial and nothing is
	// derived. The same option uses frame order as the slice number of an NM
	// reconstruction without Slice Vector.
	DeriveLegacyFramePositions bool
	// SliceThicknessAsSpacing lets an NM reconstruction without a usable
	// Spacing Between Slices step by Slice Thickness.
	SliceThicknessAsSpacing bool
}

// FramePositionDerivation describes a computed Image Position (Patient):
// Origin + Offset·Normal.
type FramePositionDerivation struct {
	// Origin is the position of the first frame (length 3).
	Origin []float64
	// Normal is the unit slice normal, row cosine × column cosine.
	Normal []float64
	// Offset is the signed distance in mm from Origin along Normal.
	Offset float64
	// SliceIndex is the frame's slice number relative to the first frame
	// (Slice Vector difference or frame index); zero when the offset comes
	// from Slice Location Vector.
	SliceIndex int
	// Step is the signed mm per slice index and StepSource its attribute;
	// both are zero when the offset comes from Slice Location Vector.
	Step       float64
	StepSource SliceSpacingSource
	// Recovered reports a derivation made by an opt-in
	// FrameGeometryOptions recovery rather than a standard rule.
	Recovered bool
}

var (
	tagImageType                   = core.NewTag(0x0008, 0x0008)
	tagNumberOfFrames              = core.NewTag(0x0028, 0x0008)
	tagFrameIncrementPointer       = core.NewTag(0x0028, 0x0009)
	tagSliceLocationVector         = core.NewTag(0x0018, 0x2005)
	tagDetectorVector              = core.NewTag(0x0054, 0x0020)
	tagDetectorInformationSequence = core.NewTag(0x0054, 0x0022)
	tagSliceVector                 = core.NewTag(0x0054, 0x0080)
	tagNumberOfSlices              = core.NewTag(0x0054, 0x0081)
)

// GetTags returns the values of an AT element (for example Frame Increment
// Pointer), decoded in the object's value byte order.
func (o *Object) GetTags(tag core.Tag) ([]core.Tag, bool) {
	if o == nil {
		return nil, false
	}
	element, ok := o.Get(tag)
	if !ok || element.VR() != core.VRAT {
		return nil, false
	}
	if values, ok := element.Value.(core.TagValue); ok {
		return append([]core.Tag(nil), values...), len(values) > 0
	}
	raw, ok := element.RawBytes()
	if !ok || len(raw) == 0 || len(raw)%4 != 0 {
		return nil, false
	}
	order := o.ValueByteOrder()
	if order == nil {
		order = binary.LittleEndian
	}
	out := make([]core.Tag, len(raw)/4)
	for i := range out {
		out[i] = core.NewTag(order.Uint16(raw[4*i:]), order.Uint16(raw[4*i+2:]))
	}
	return out, true
}

func deriveFramePosition(o *Object, frameIndex int, g *FrameGeometry, options FrameGeometryOptions) {
	if frameIndex < 0 {
		return
	}
	pointers, _ := o.GetTags(tagFrameIncrementPointer)
	if isNMReconstruction(o, pointers) {
		deriveNMSlicePosition(o, frameIndex, pointers, g, options)
		return
	}
	if options.DeriveLegacyFramePositions {
		deriveLegacyFramePosition(o, frameIndex, pointers, g)
	}
}

// isNMReconstruction: the Frame Increment Pointer names Slice Vector, which
// PS3.3 C.8.4.8.1 requires of RECON TOMO and RECON GATED TOMO, or Image Type
// value 3 names one of those.
func isNMReconstruction(o *Object, pointers []core.Tag) bool {
	if containsTag(pointers, tagSliceVector) {
		return true
	}
	values, ok := o.GetStrings(tagImageType)
	if !ok || len(values) < 3 {
		return false
	}
	switch strings.ToUpper(strings.TrimSpace(values[2])) {
	case "RECON TOMO", "RECON GATED TOMO":
		return true
	}
	return false
}

// deriveNMSlicePosition places frame k of an NM reconstruction at
// origin + (s_k − s_0)·SBS·normal (PS3.3 C.8.4.8, C.8.4.11, C.8.4.15): the
// origin and orientation are those of the Detector Information item, which
// locate the first transmitted frame, s is the frame's Slice Vector number,
// and SBS the signed Spacing Between Slices.
func deriveNMSlicePosition(o *Object, frameIndex int, pointers []core.Tag, g *FrameGeometry, options FrameGeometryOptions) {
	detector, finding := nmDetectorItem(o, frameIndex, pointers)
	if finding != "" {
		g.Findings = append(g.Findings, finding)
		return
	}
	origin := []float64(nil)
	if detector != nil {
		item := detector.FrameGeometry()
		if len(item.ImageOrientationPatient) >= 6 {
			g.ImageOrientationPatient = item.ImageOrientationPatient
			g.OrientationSource = GeometrySourceNMDetector
		}
		if len(item.ImagePositionPatient) >= 3 {
			origin = item.ImagePositionPatient[:3]
		}
	}
	if origin == nil && len(g.ImagePositionPatient) >= 3 {
		origin = g.ImagePositionPatient[:3]
	}
	normal, normalOK := sliceNormal(g.ImageOrientationPatient)
	if origin == nil || !normalOK || !finiteValues(origin) {
		g.Findings = append(g.Findings, FindingNMDetectorGeometryMissing)
		return
	}

	recovered := false
	sliceIndex := 0
	if numbers, err := o.GetInts(tagSliceVector); err == nil && frameIndex < len(numbers) && len(numbers) > 0 {
		limit := int64(math.MaxInt32)
		if slices, err := o.GetInt(tagNumberOfSlices); err == nil && slices > 0 {
			limit = slices
		}
		first, current := numbers[0], numbers[frameIndex]
		if first < 1 || current < 1 || first > limit || current > limit {
			g.Findings = append(g.Findings, FindingSliceVectorOutOfRange)
			return
		}
		sliceIndex = int(current - first)
	} else if options.DeriveLegacyFramePositions {
		sliceIndex, recovered = frameIndex, true
		g.Findings = append(g.Findings, FindingFrameOrderAsSliceIndex)
	} else {
		g.Findings = append(g.Findings, FindingSliceVectorMissing)
		return
	}

	step, stepSource := 0.0, SliceSpacingUnknown
	switch spacing := g.SliceSpacing(); {
	case spacing.Source == SliceSpacingFromSpacingBetweenSlices:
		step, stepSource = g.SpacingBetweenSlices, spacing.Source
		if spacing.Negative {
			g.Findings = append(g.Findings, FindingNegativeSpacingBetweenSlices)
		}
	case spacing.Source == SliceSpacingFromSliceThickness && options.SliceThicknessAsSpacing:
		step, stepSource, recovered = spacing.Value, spacing.Source, true
		g.Findings = append(g.Findings, FindingSliceThicknessAsSpacing)
	default:
		g.Findings = append(g.Findings, FindingSpacingBetweenSlicesMissing)
		return
	}
	setDerivedPosition(g, GeometrySourceNMSliceVector, FramePositionDerivation{
		Origin: append([]float64(nil), origin...), Normal: normal,
		Offset: float64(sliceIndex) * step, SliceIndex: sliceIndex,
		Step: step, StepSource: stepSource, Recovered: recovered,
	})
}

// nmDetectorItem selects the Detector Information item of the frame: the only
// item, or the one its Detector Vector value names. No sequence is not an
// error (the top-level attributes may still place the frame).
func nmDetectorItem(o *Object, frameIndex int, pointers []core.Tag) (*Object, FrameGeometryFinding) {
	items, ok := o.GetSequence(tagDetectorInformationSequence)
	if !ok || len(items) == 0 {
		return nil, ""
	}
	if len(items) == 1 {
		return items[0], ""
	}
	if vector, err := o.GetInts(tagDetectorVector); err == nil && frameIndex < len(vector) {
		if number := vector[frameIndex]; number >= 1 && number <= int64(len(items)) {
			return items[number-1], ""
		}
	}
	return nil, FindingNMDetectorAmbiguous
}

// deriveLegacyFramePosition implements the opt-in
// FrameGeometryOptions.DeriveLegacyFramePositions for a non-NM frame.
func deriveLegacyFramePosition(o *Object, frameIndex int, pointers []core.Tag, g *FrameGeometry) {
	if g.PositionSource != GeometrySourceImagePlane || g.OrientationSource != GeometrySourceImagePlane ||
		len(g.ImagePositionPatient) < 3 || !finiteValues(g.ImagePositionPatient[:3]) {
		return
	}
	frames, err := o.GetInt(tagNumberOfFrames)
	if err != nil || frames < 2 || int64(frameIndex) >= frames {
		return
	}
	normal, ok := sliceNormal(g.ImageOrientationPatient)
	if !ok {
		return
	}
	derivation := FramePositionDerivation{
		Origin: append([]float64(nil), g.ImagePositionPatient[:3]...), Normal: normal, Recovered: true,
	}
	switch {
	case len(pointers) == 0:
		spacing := g.SliceSpacing()
		switch spacing.Source {
		case SliceSpacingFromSpacingBetweenSlices:
			derivation.Step = g.SpacingBetweenSlices
			if spacing.Negative {
				g.Findings = append(g.Findings, FindingNegativeSpacingBetweenSlices)
			}
		case SliceSpacingFromSliceThickness:
			derivation.Step = spacing.Value
		default:
			g.Findings = append(g.Findings, FindingSpacingBetweenSlicesMissing)
			return
		}
		derivation.StepSource = spacing.Source
		derivation.SliceIndex = frameIndex
		derivation.Offset = float64(frameIndex) * derivation.Step
	case len(pointers) == 1 && pointers[0] == tagSliceLocationVector:
		locations, err := o.GetFloats(tagSliceLocationVector)
		if err != nil || int64(len(locations)) != frames || !finiteValues(locations) {
			g.Findings = append(g.Findings, FindingSliceLocationVectorInvalid)
			return
		}
		derivation.Offset = locations[frameIndex] - locations[0]
	default:
		g.Findings = append(g.Findings, FindingFrameIncrementNotSpatial)
		return
	}
	setDerivedPosition(g, GeometrySourceLegacyFrameIncrement, derivation)
}

func setDerivedPosition(g *FrameGeometry, source GeometrySource, derivation FramePositionDerivation) {
	position := make([]float64, 3)
	for axis := range position {
		position[axis] = derivation.Origin[axis] + derivation.Offset*derivation.Normal[axis]
	}
	g.ImagePositionPatient = position
	g.PositionSource = source
	g.PositionDerivation = &derivation
}

// sliceNormal is the unit row × column cosine of Image Orientation
// (Patient).
func sliceNormal(iop []float64) ([]float64, bool) {
	if len(iop) < 6 || !finiteValues(iop[:6]) {
		return nil, false
	}
	x := iop[1]*iop[5] - iop[2]*iop[4]
	y := iop[2]*iop[3] - iop[0]*iop[5]
	z := iop[0]*iop[4] - iop[1]*iop[3]
	length := math.Sqrt(x*x + y*y + z*z)
	if length == 0 || !finiteFloat(length) {
		return nil, false
	}
	return []float64{x / length, y / length, z / length}, true
}

func finiteValues(values []float64) bool {
	for _, value := range values {
		if !finiteFloat(value) {
			return false
		}
	}
	return true
}

func containsTag(tags []core.Tag, want core.Tag) bool {
	for _, tag := range tags {
		if tag == want {
			return true
		}
	}
	return false
}
